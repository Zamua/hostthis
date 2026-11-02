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
const DAY = 86_400_000;

async function call(cell, op, payload) {
  const init = payload === undefined ? {} : { method: "POST", body: JSON.stringify(payload) };
  const res = await cell.fetch(new Request(`https://cell/x/${op}`, init));
  const text = await res.text();
  let body = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    body = text;
  }
  return { status: res.status, body };
}

async function pasteCell(identity, slug) {
  const paste = new Paste(state(new FakeStorage()));
  const res = await paste.put({
    row: { slug, identity, kind: "html", size: 3, createdAt: 7, updatedAt: 7, status: "ready", uploadId: "a".repeat(32) },
    generation: "g1", fingerprint: "f1",
  });
  assert.equal(res.status, 204);
  return paste;
}

// An Abuse cell over in-memory Paste and Denylist cells, a scripted owner
// index, and a bucket that records prefix deletes.
function world({ protectedOwners = "" } = {}) {
  const pastes = new Map();
  const index = new Map();
  const denylist = new Denylist(state(new FakeStorage()));
  const deleted = [];
  const ns = (lookup) => ({ idFromName: (name) => name, get: (name) => ({ fetch: (req) => lookup(name).fetch(req) }) });
  const env = {
    PASTES: ns((slug) => pastes.get(slug) ?? new Paste(state(new FakeStorage()))),
    DENYLIST: ns(() => denylist),
    IDENTITY: ns((identity) => ({
      fetch: async () => Response.json((index.get(identity) ?? []).map((slug) => ({ slug }))),
    })),
    PAYLOADS: {
      list: async ({ prefix }) => ({ objects: [{ key: `${prefix}0` }], truncated: false }),
      delete: async (keys) => { deleted.push(...keys); },
    },
    HOSTTHIS_PROTECTED_OWNERS: protectedOwners,
  };
  const storage = new FakeStorage();
  const abuse = new Abuse(state(storage), env);
  let now = NOW;
  abuse.now = () => now;
  const add = async (identity, slug) => {
    pastes.set(slug, await pasteCell(identity, slug));
    index.set(identity, [...(index.get(identity) ?? []), slug]);
  };
  // Runs the alarm until it is set past `until`.
  const runAlarms = async (until) => {
    for (let i = 0; i < 200 && storage.alarm !== null && storage.alarm <= until; i++) {
      now = Math.max(now, storage.alarm);
      await abuse.alarm();
    }
  };
  return { abuse, pastes, denylist, deleted, add, runAlarms, storage, at: (t) => { now = t; } };
}

const publicFound = async (paste) => (await call(paste, "resolve?path=/")).body.found !== false;

test("ban-owner bans the key, quarantines every slug, and deletes them after the retention window", async () => {
  const w = world();
  for (let i = 0; i < 23; i++) {
    await w.add(OWNER, `own${String(i).padStart(2, "0")}abc`);
  }
  await w.add(OTHER, "keep2345");
  const res = await call(w.abuse, "banowner", { identity: FP, reason: "spam", actor: "operator", now: NOW });
  assert.equal(res.status, 200);
  assert.equal(res.body.slugs, 23);
  assert.equal(res.body.deleteAfter, NOW + 7 * DAY);
  assert.deepEqual((await call(w.denylist, "list")).body.entries.map((e) => [e.entry, e.expiresAt]), [[FP, undefined]]);

  await w.runAlarms(NOW + DAY);
  for (const [slug, paste] of w.pastes) {
    assert.equal(await publicFound(paste), slug === "keep2345", slug);
  }
  let [ban] = (await call(w.abuse, "bans")).body.bans;
  assert.deepEqual([ban.state, ban.quarantined, ban.deleted], ["active", 23, 0]);
  assert.equal((await call(w.abuse, "list")).body.quarantined.length, 23);

  await w.runAlarms(NOW + 8 * DAY);
  [ban] = (await call(w.abuse, "bans")).body.bans;
  assert.deepEqual([ban.state, ban.deleted, ban.failed], ["deleted", 23, []]);
  for (const [slug, paste] of w.pastes) {
    assert.equal((await call(paste, "operatorrow")).status === 404, slug !== "keep2345", slug);
  }
  assert.equal(w.deleted.length, 23);
  assert.deepEqual((await call(w.abuse, "list")).body.quarantined, []);
});

test("a slug created after the ban is caught by the sweep", async () => {
  const w = world();
  await w.add(OWNER, "first234");
  await call(w.abuse, "banowner", { identity: FP, reason: "x", actor: "operator", now: NOW });
  await w.add(OWNER, "late2345");
  await w.runAlarms(NOW + 2 * 60_000);
  assert.equal(await publicFound(w.pastes.get("late2345")), false);
});

test("unban releases only the ban's own quarantines and lifts the key", async () => {
  const w = world();
  await w.add(OWNER, "aaaa2345");
  await w.add(OWNER, "bbbb2345");
  await call(w.abuse, "quarantine", { slug: "bbbb2345", reason: "phishing", actor: "operator", now: NOW - 1 });
  const { body: { banId } } = await call(w.abuse, "banowner", { slug: "aaaa2345", reason: "x", actor: "operator", now: NOW });
  await w.runAlarms(NOW + 60_000);
  const un = await call(w.abuse, "unbanowner", { banId, actor: "operator", now: NOW + 60_000 });
  assert.deepEqual(un.body, { banId, released: 1 });
  await w.runAlarms(NOW + 10 * DAY);
  assert.equal(await publicFound(w.pastes.get("aaaa2345")), true);
  assert.equal(await publicFound(w.pastes.get("bbbb2345")), false);
  assert.deepEqual((await call(w.denylist, "list")).body.entries, []);
  const [ban] = (await call(w.abuse, "bans")).body.bans;
  assert.equal(ban.state, "lifted");
  assert.equal((await call(w.pastes.get("aaaa2345"), "operatorrow")).status, 200, "a lifted ban deletes nothing");
});

test("ban-owner refuses a protected owner, a second open ban, and a bad retention", async () => {
  const w = world({ protectedOwners: OTHER });
  await w.add(OWNER, "aaaa2345");
  await w.add(OTHER, "cccc2345");
  assert.equal((await call(w.abuse, "banowner", { slug: "cccc2345", reason: "", actor: "operator", now: NOW })).status, 403);
  for (const retentionDays of [0, 91, 2.5]) {
    assert.equal((await call(w.abuse, "banowner", { identity: FP, reason: "", retentionDays, actor: "operator", now: NOW })).status, 400);
  }
  const first = await call(w.abuse, "banowner", { identity: FP, reason: "", retentionDays: 1, actor: "operator", now: NOW });
  assert.equal(first.body.deleteAfter, NOW + DAY);
  const again = await call(w.abuse, "banowner", { identity: FP, reason: "", actor: "operator", now: NOW + 1 });
  assert.deepEqual([again.status, again.body.banId], [409, first.body.banId]);
});

test("unban after deletion started is refused", async () => {
  const w = world();
  await w.add(OWNER, "aaaa2345");
  const { body: { banId } } = await call(w.abuse, "banowner", { identity: FP, reason: "", retentionDays: 1, actor: "operator", now: NOW });
  await w.runAlarms(NOW + 2 * DAY);
  assert.equal((await call(w.abuse, "unbanowner", { banId, actor: "operator", now: NOW + 2 * DAY })).status, 409);
  assert.equal((await call(w.abuse, "unbanowner", { banId: "nope", actor: "operator", now: NOW })).status, 404);
});
