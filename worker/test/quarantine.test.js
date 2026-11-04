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

test("watcher quarantines are not rate-capped", async () => {
  const w = abuseWorld();
  for (let i = 0; i < 8; i++) {
    const slug = `slug${i}abc`;
    w.pastes.set(slug, await pasteWithRow(OWNER, slug));
    assert.equal((await call(w.abuse, "quarantine", { slug, reason: "x", actor: "watcher", now: NOW + i })).status, 200);
  }
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

test("a watcher ban can name the key as a slug's owner, under the same guards", async () => {
  const w = abuseWorld({ protectedOwners: OTHER });
  w.pastes.set("abcd2345", await pasteWithRow(OWNER, "abcd2345"));
  w.pastes.set("efgh2345", await pasteWithRow(OTHER, "efgh2345"));
  const ban = await call(w.abuse, "bankey", { slug: "abcd2345", reason: "phishing", actor: "watcher", now: NOW });
  assert.equal(ban.status, 200);
  assert.equal(ban.body.identity, FP);
  assert.deepEqual((await call(w.denylist, "list")).body.entries.map((e) => e.entry), [FP]);
  assert.equal((await call(w.abuse, "bankey", { slug: "efgh2345", reason: "x", actor: "watcher", now: NOW })).status, 403);
  assert.equal((await call(w.abuse, "bankey", { slug: "nope2345", reason: "x", actor: "watcher", now: NOW })).status, 404);
});

test("an operator room delete reaches the room by app and id and is audited", async () => {
  const w = abuseWorld();
  const seen = [];
  w.abuse.env.ROOMS = {
    idFromName: (n) => n,
    get: (n) => ({ fetch: async (req) => { seen.push([n, new URL(req.url).pathname]); return Response.json({ removed: true }); } }),
  };
  const room = "11111111-1111-4111-8111-111111111111";
  const res = await call(w.abuse, "roomdelete", { app: "abcd2345", room, actor: "operator", now: NOW });
  assert.deepEqual(res.body, { app: "abcd2345", removed: true });
  assert.deepEqual(seen, [[`abcd2345|${room}`, "/room/remove"]]);
  const entry = (await call(w.abuse, "audit")).body.entries.at(-1);
  assert.deepEqual([entry.action, entry.outcome], ["room-delete", "ok"]);
  assert.ok(!entry.target.includes(room), "the audit log keeps the room id hashed");
  assert.equal((await call(w.abuse, "roomdelete", { app: "abcd2345", room: "nope", actor: "operator", now: NOW })).status, 400);
});

test("refused lists rate-capped bans newest first, short targets, and marks later bans", async () => {
  const w = abuseWorld();
  for (let i = 1; i <= 5; i++) {
    const fp = "SHA256:" + String(i).repeat(64);
    assert.equal((await call(w.abuse, "bankey", { identity: fp, reason: "x", actor: "watcher", now: NOW + i })).status, 200);
  }
  const k6 = "SHA256:" + "6".repeat(64);
  const k7 = "SHA256:" + "7".repeat(64);
  assert.equal((await call(w.abuse, "bankey", { identity: k6, reason: "first", actor: "watcher", now: NOW + 10 })).status, 429);
  assert.equal((await call(w.abuse, "bankey", { identity: k7, reason: "second", actor: "watcher", now: NOW + 11 })).status, 429);
  // The operator bans k6 later; k7 stays open.
  assert.equal((await call(w.abuse, "bankey", { identity: k6, reason: "op", actor: "operator", now: NOW + 20 })).status, 200);
  const res = await call(w.abuse, "refused", undefined, `?since=${NOW}&limit=50`);
  assert.equal(res.status, 200);
  assert.deepEqual(res.body.entries.map((e) => [e.at, e.action, e.target, e.reason, e.already_banned]), [
    [NOW + 11, "ban-key", "7".repeat(10), "second", false],
    [NOW + 10, "ban-key", "6".repeat(10), "first", true],
  ]);
  const raw = JSON.stringify(res.body);
  assert.ok(!raw.includes("SHA256"), "no raw key in the answer");
});

test("refused honors since and limit, and ignores accepted entries", async () => {
  const w = abuseWorld();
  for (let i = 1; i <= 5; i++) {
    await call(w.abuse, "bankey", { identity: "SHA256:" + String(i).repeat(64), reason: "x", actor: "watcher", now: NOW + i });
  }
  for (let i = 0; i < 3; i++) {
    await call(w.abuse, "bankey", { identity: "SHA256:" + String(8 + i % 2).repeat(64), reason: `r${i}`, actor: "watcher", now: NOW + 10 + i });
  }
  const all = await call(w.abuse, "refused", undefined, `?since=0&limit=200`);
  assert.equal(all.body.entries.length, 3);
  const one = await call(w.abuse, "refused", undefined, `?since=0&limit=1`);
  assert.deepEqual(one.body.entries.map((e) => e.at), [NOW + 12]);
  const later = await call(w.abuse, "refused", undefined, `?since=${NOW + 12}&limit=50`);
  assert.deepEqual(later.body.entries.map((e) => e.at), [NOW + 12]);
});

const fresh = (cell, subnet, fp, at) => call(cell, "fresh", { subnet, identity: `key:${fp}`, at });
const FPS = Array.from({ length: 6 }, (_, i) => "SHA256:" + String(i + 1).repeat(64));

test("keysubnets groups keys first seen on one subnet within the window, largest first, with short ids", async () => {
  const w = abuseWorld();
  const MIN = 60_000;
  for (let i = 0; i < 4; i++) {
    await fresh(w.abuse, "203.0.113.0/24", FPS[i], NOW + i * MIN);
  }
  for (let i = 3; i < 6; i++) {
    await fresh(w.abuse, "198.51.100.0/24", FPS[i], NOW + i * MIN);
  }
  await fresh(w.abuse, "192.0.2.0/24", FPS[0], NOW);
  await fresh(w.abuse, "203.0.113.0/24", FPS[0], NOW + 9 * MIN); // the same key again is one key
  await call(w.abuse, "bankey", { identity: FPS[1], reason: "x", actor: "operator", now: NOW + 10 * MIN });
  const res = await call(w.abuse, "keysubnets", undefined, `?now=${NOW + 20 * MIN}&window_min=60&min_keys=3&limit=50`);
  assert.equal(res.status, 200);
  assert.deepEqual(res.body.groups.map((g) => [g.subnet, g.keys.length]), [["203.0.113.0/24", 4], ["198.51.100.0/24", 3]]);
  const g = res.body.groups[0];
  assert.deepEqual([g.first, g.last], [NOW, NOW + 3 * MIN]);
  assert.deepEqual(g.keys.map((k) => [k.key, k.at, k.banned]), [
    ["1".repeat(10), NOW, false], ["2".repeat(10), NOW + MIN, true], ["3".repeat(10), NOW + 2 * MIN, false], ["4".repeat(10), NOW + 3 * MIN, false],
  ]);
  assert.match(g.keys[0].owner, /^[0-9a-f]{8}$/);
  assert.ok(!JSON.stringify(res.body).includes("SHA256"), "no full key");
  // A narrower window drops the older events; min_keys and limit apply.
  const narrow = await call(w.abuse, "keysubnets", undefined, `?now=${NOW + 20 * MIN}&window_min=18&min_keys=3&limit=50`);
  assert.deepEqual(narrow.body.groups.map((x) => x.subnet), ["198.51.100.0/24"]);
  const limited = await call(w.abuse, "keysubnets", undefined, `?now=${NOW + 20 * MIN}&window_min=60&min_keys=1&limit=1`);
  assert.equal(limited.body.groups.length, 1);
});

test("the fresh ring is bounded and drops events older than a day", async () => {
  const w = abuseWorld();
  const DAY = 86_400_000;
  await fresh(w.abuse, "203.0.113.0/24", FPS[0], NOW);
  await fresh(w.abuse, "203.0.113.0/24", FPS[1], NOW + DAY + 1);
  const res = await call(w.abuse, "keysubnets", undefined, `?now=${NOW + DAY + 2}&window_min=1440&min_keys=1&limit=50`);
  assert.deepEqual(res.body.groups[0].keys.map((k) => k.key), ["2".repeat(10)]);
});
