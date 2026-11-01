import assert from "node:assert/strict";
import test from "node:test";

import { Abuse } from "../src/cells/abuse.js";
import { Denylist } from "../src/cells/denylist.js";
import { Paste } from "../src/cells/paste.js";
import { FakeStorage, state } from "./harness.js";

const FP = "SHA256:" + "ab".repeat(32);
const OWNER = "key:" + FP;
const OTHER = "key:SHA256:" + "cd".repeat(32);
const NOW = 1_800_000_000_000;

async function pasteWithRow(identity = OWNER, slug = "abcd2345") {
  const paste = new Paste(state(new FakeStorage()));
  const res = await paste.put({
    row: { slug, identity, kind: "html", size: 3, createdAt: 7, updatedAt: 7, status: "ready", uploadId: "a".repeat(32) },
    generation: "g1", fingerprint: "f1",
  });
  assert.equal(res.status, 204);
  return paste;
}

async function call(cell, op, body, query = "") {
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  const res = await cell.fetch(new Request(`https://cell/x/${op}${query}`, init));
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : null };
}

test("a quarantined paste reads as absent publicly and as 451 to get, and release restores it", async () => {
  const paste = await pasteWithRow();
  const q = await call(paste, "quarantine", { reason: "probe", actor: "operator", at: NOW });
  assert.equal(q.status, 200);
  assert.deepEqual([q.body.identity, q.body.already], [OWNER, false]);
  assert.equal((await call(paste, "resolve", undefined, "?path=/")).body.found, false);
  assert.equal((await call(paste, "resolve", undefined, "?path=/&raw=1")).body.found, false);
  const got = await call(paste, "get");
  assert.equal(got.status, 451);
  assert.deepEqual(got.body, { error: "quarantined", identity: OWNER });
  assert.equal((await call(paste, "exists")).body.exists, false);
  const op = await call(paste, "operatorrow");
  assert.equal(op.body.row.identity, OWNER);
  assert.deepEqual(op.body.quarantine, { reason: "probe", actor: "operator", at: NOW });

  assert.equal((await call(paste, "quarantine", { reason: "again", actor: "watcher", at: NOW + 1 })).body.already, true);
  assert.equal((await call(paste, "operatorrow")).body.quarantine.reason, "probe");

  assert.equal((await call(paste, "release", {})).body.released, true);
  assert.equal((await call(paste, "get")).status, 200);
  assert.notEqual((await call(paste, "resolve", undefined, "?path=/")).body.found, false);
  assert.equal((await call(paste, "release", {})).body.released, false);
});

test("quarantining a slug with no row is a 404", async () => {
  const paste = new Paste(state(new FakeStorage()));
  const q = await call(paste, "quarantine", { reason: "", actor: "operator", at: NOW });
  assert.equal(q.status, 404);
  assert.equal(q.body.error, "no-such-paste");
});

// An Abuse cell over in-memory Paste and Denylist cells.
function abuseWorld({ protectedOwners = "" } = {}) {
  const pastes = new Map();
  const denylist = new Denylist(state(new FakeStorage()));
  const ns = (lookup) => ({
    idFromName: (name) => name,
    get: (name) => ({ fetch: (req) => lookup(name).fetch(req) }),
  });
  const env = {
    PASTES: ns((slug) => pastes.get(slug) ?? new Paste(state(new FakeStorage()))),
    DENYLIST: ns(() => denylist),
    HOSTTHIS_PROTECTED_OWNERS: protectedOwners,
  };
  return { abuse: new Abuse(state(new FakeStorage()), env), pastes, denylist };
}

test("an operator quarantine is indexed and audited; release unindexes it", async () => {
  const w = abuseWorld();
  w.pastes.set("abcd2345", await pasteWithRow());
  const q = await call(w.abuse, "quarantine", { slug: "abcd2345", reason: "probe", actor: "operator", now: NOW });
  assert.equal(q.status, 200);
  assert.equal(q.body.quarantined, true);
  const list = (await call(w.abuse, "list")).body.quarantined;
  assert.deepEqual(list, [{ slug: "abcd2345", owner: OWNER, reason: "probe", actor: "operator", at: NOW }]);
  assert.equal((await call(w.abuse, "release", { slug: "abcd2345", actor: "operator", now: NOW + 5 })).body.released, true);
  assert.deepEqual((await call(w.abuse, "list")).body.quarantined, []);
  const audit = (await call(w.abuse, "audit")).body.entries;
  assert.deepEqual(audit.map((e) => [e.action, e.actor, e.outcome]),
    [["quarantine", "operator", "ok"], ["release", "operator", "ok"]]);
});

test("the watcher cannot touch a protected owner, by fingerprint or by hash", async () => {
  const hash = [...new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(OTHER)))]
    .slice(0, 8).map((b) => b.toString(16).padStart(2, "0")).join("");
  const w = abuseWorld({ protectedOwners: `${FP}, ${hash}` });
  w.pastes.set("abcd2345", await pasteWithRow(OWNER, "abcd2345"));
  w.pastes.set("efgh2345", await pasteWithRow(OTHER, "efgh2345"));
  for (const slug of ["abcd2345", "efgh2345"]) {
    const res = await call(w.abuse, "quarantine", { slug, reason: "x", actor: "watcher", now: NOW });
    assert.equal(res.status, 403, slug);
    assert.equal(res.body.error, "protected-owner");
    assert.equal((await call(w.pastes.get(slug), "get")).status, 200, slug);
  }
  const ban = await call(w.abuse, "bankey", { identity: OTHER, reason: "x", actor: "watcher", now: NOW });
  assert.equal(ban.status, 403);
  assert.deepEqual((await call(w.denylist, "list")).body.entries, []);
  const outcomes = (await call(w.abuse, "audit")).body.entries.map((e) => e.outcome);
  assert.deepEqual(outcomes, ["protected-owner", "protected-owner", "protected-owner"]);
});

test("the watcher is capped at 5 quarantines per rolling hour, and the operator is not", async () => {
  const w = abuseWorld();
  for (let i = 0; i < 7; i++) {
    const slug = `slug${i}abc`;
    w.pastes.set(slug, await pasteWithRow(OWNER, slug));
  }
  for (let i = 0; i < 5; i++) {
    assert.equal((await call(w.abuse, "quarantine", { slug: `slug${i}abc`, reason: "x", actor: "watcher", now: NOW + i })).status, 200);
  }
  const capped = await call(w.abuse, "quarantine", { slug: "slug5abc", reason: "x", actor: "watcher", now: NOW + 10 });
  assert.equal(capped.status, 429);
  assert.equal(capped.body.error, "rate-capped");
  assert.equal((await call(w.pastes.get("slug5abc"), "get")).status, 200);
  assert.equal((await call(w.abuse, "quarantine", { slug: "slug5abc", reason: "x", actor: "operator", now: NOW + 11 })).status, 200);
  // The window rolls: an hour after the first, one slot is free again.
  assert.equal((await call(w.abuse, "quarantine", { slug: "slug6abc", reason: "x", actor: "watcher", now: NOW + 3_600_000 })).status, 200);
});

test("a watcher ban adds the key to the denylist, capped separately", async () => {
  const w = abuseWorld();
  const ban = await call(w.abuse, "bankey", { identity: OWNER, reason: "csam", actor: "watcher", now: NOW });
  assert.equal(ban.status, 200);
  assert.deepEqual((await call(w.denylist, "list")).body.entries.map((e) => [e.entry, e.reason]), [[FP, "csam"]]);
  for (let i = 1; i < 5; i++) {
    const fp = "SHA256:" + String(i).repeat(64);
    assert.equal((await call(w.abuse, "bankey", { identity: fp, reason: "x", actor: "watcher", now: NOW + i })).status, 200);
  }
  const capped = await call(w.abuse, "bankey", { identity: "SHA256:" + "9".repeat(64), reason: "x", actor: "watcher", now: NOW + 9 });
  assert.equal(capped.status, 429);
  const bad = await call(w.abuse, "bankey", { identity: "203.0.113.0/24", reason: "x", actor: "watcher", now: NOW });
  assert.equal(bad.status, 400);
});
