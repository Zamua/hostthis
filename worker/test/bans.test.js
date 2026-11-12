import assert from "node:assert/strict";
import test from "node:test";

import { Abuse } from "../src/cells/abuse.js";
import { Denylist } from "../src/cells/denylist.js";
import { Paste } from "../src/cells/paste.js";
import { FakeStorage, denylistEnv, state } from "./harness.js";

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
  const denylist = new Denylist(state(new FakeStorage()), denylistEnv());
  const deleted = [];
  const ns = (lookup) => ({ idFromName: (name) => name, get: (name) => ({ fetch: (req) => lookup(name).fetch(req) }) });
  const env = {
    PASTES: ns((slug) => pastes.get(slug) ?? new Paste(state(new FakeStorage()))),
    DENYLIST: ns(() => denylist),
    IDENTITY: ns((identity) => ({
      fetch: async (req) => (new URL(req.url).pathname.endsWith("/subnetlist")
        ? Response.json({ subnets: [{ subnet: "203.0.113.0/24", at: NOW - 5 }] })
        : Response.json((index.get(identity) ?? []).map((slug) => ({ slug })))),
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
  assert.deepEqual(res.body.subnets, [{ subnet: "203.0.113.0/24", at: NOW - 5 }]);
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

test("the watcher's owner ban runs the cascade at the default retention under the shared hourly cap", async () => {
  const w = world({ protectedOwners: OTHER });
  await w.add(OWNER, "aaaa2345");
  await w.add(OTHER, "cccc2345");
  assert.equal((await call(w.abuse, "banowner", { slug: "cccc2345", reason: "x", actor: "watcher", now: NOW })).status, 403);
  const res = await call(w.abuse, "banowner", { slug: "aaaa2345", reason: "spam", actor: "watcher", now: NOW });
  assert.equal(res.status, 200);
  assert.equal(res.body.deleteAfter, NOW + 7 * DAY);
  await w.runAlarms(NOW + 60_000);
  assert.equal(await publicFound(w.pastes.get("aaaa2345")), false);
  for (let i = 1; i < 5; i++) {
    const fp = "SHA256:" + String(i).repeat(64);
    assert.equal((await call(w.abuse, "bankey", { identity: fp, reason: "x", actor: "watcher", now: NOW + i })).status, 200);
  }
  const capped = await call(w.abuse, "banowner", { identity: "SHA256:" + "9".repeat(64), reason: "x", actor: "watcher", now: NOW + 9 });
  assert.equal(capped.status, 429);
});

// A state whose gate runs one callback at a time, as the runtime's does.
function gatedState(storage) {
  let tail = Promise.resolve();
  return {
    ...state(storage),
    blockConcurrencyWhile(callback) {
      const run = tail.then(callback);
      tail = run.catch(() => {});
      return run;
    },
  };
}

test("an unban that arrives during an alarm pass is not overwritten by it", async () => {
  const paste = await pasteCell(OWNER, "aaaaaaaa");
  const denylist = new Denylist(state(new FakeStorage()), denylistEnv());
  let hold = null; // while set, calls to the paste cell wait on it
  const env = {
    PASTES: {
      idFromName: (name) => name,
      get: () => ({ fetch: async (req) => { await hold; return paste.fetch(req); } }),
    },
    DENYLIST: { idFromName: (name) => name, get: () => denylist },
    IDENTITY: {
      idFromName: (name) => name,
      get: () => ({
        fetch: async (req) => (new URL(req.url).pathname.endsWith("/subnetlist")
          ? Response.json({ subnets: [] }) : Response.json([{ slug: "aaaaaaaa" }])),
      }),
    },
    PAYLOADS: { list: async () => ({ objects: [], truncated: false }), delete: async () => {} },
  };
  const storage = new FakeStorage();
  const abuse = new Abuse(gatedState(storage), env);
  let now = NOW;
  abuse.now = () => now;
  const ban = await call(abuse, "banowner", { actor: "operator", now: NOW, identity: OWNER, reason: "mistake" });
  assert.equal(ban.status, 200);

  // The pass starts and waits on its first call to the paste cell.
  let open;
  hold = new Promise((resolve) => { open = resolve; });
  const pass = abuse.alarm();
  await new Promise((resolve) => setTimeout(resolve, 10));
  const unban = call(abuse, "unbanowner", { actor: "operator", now: NOW + 1, banId: ban.body.banId });
  await new Promise((resolve) => setTimeout(resolve, 10));
  hold = null;
  open();
  await pass;
  assert.equal((await unban).status, 200);
  assert.equal((await storage.get(`b:${ban.body.banId}`)).state, "lifting");

  for (let i = 0; i < 50 && storage.alarm !== null && (await storage.get(`b:${ban.body.banId}`)).state !== "lifted"; i++) {
    now = Math.max(now, storage.alarm);
    await abuse.alarm();
  }
  const record = await storage.get(`b:${ban.body.banId}`);
  assert.equal(record.state, "lifted");
  assert.deepEqual(record.slugs, { aaaaaaaa: "released" });
  const row = await call(paste, "operatorrow");
  assert.equal(Boolean(row.body.quarantine), false, "the slug is served again");
});

test("a key first seen on a subnet is remembered with its address, and the watcher's feed shows it", async () => {
  const { abuse } = world();
  for (const [i, addr] of [["1", "203.0.113.5"], ["2", "203.0.113.6"], ["3", undefined]]) {
    const res = await call(abuse, "fresh", { subnet: "203.0.113.0/24", identity: `key:SHA256:${String(i).repeat(64)}`, at: NOW, addr });
    assert.equal(res.status, 200);
  }
  const feed = await call(abuse, `keysubnets?now=${NOW + 1}&window_min=60&min_keys=2&limit=10`);
  assert.equal(feed.status, 200);
  assert.deepEqual(feed.body.groups[0].keys.map((k) => k.addr), ["203.0.113.5", "203.0.113.6", null]);
});

const PURGING = { HOSTTHIS_CF_PURGE_TOKEN: "p", HOSTTHIS_CF_ZONE_ID: "z", HOSTTHIS_APEX_DOMAIN: "hostthis.dev" };

// Gives the world a PURGE namespace recording each enqueue; while `down` is
// set the queue cannot be reached.
function purgeQueue(w) {
  const queue = { asked: [], down: false };
  const cell = {
    async fetch(request) {
      if (queue.down) return new Response("down\n", { status: 500 });
      queue.asked.push(await request.json());
      return Response.json({ configured: true, queued: 1, sent: true });
    },
  };
  Object.assign(w.abuse.env, PURGING, { PURGE: { idFromName: (id) => id, get: () => cell } });
  return queue;
}

test("a ban queues an urgent purge of every slug it quarantines, once", async () => {
  const w = world();
  const queue = purgeQueue(w);
  await w.add(OWNER, "aaaa2345");
  await w.add(OWNER, "bbbb2345");
  assert.equal((await call(w.abuse, "banowner", { identity: OWNER, reason: "r", retentionDays: 30, actor: "operator", now: NOW })).status, 200);
  await w.runAlarms(NOW + 10 * 60_000);
  assert.deepEqual(queue.asked, [{ slugs: ["aaaa2345", "bbbb2345"], urgent: true }]);
});

test("purges a ban could not queue are offered again until the queue takes them", async () => {
  const w = world();
  const queue = purgeQueue(w);
  queue.down = true;
  await w.add(OWNER, "aaaa2345");
  await call(w.abuse, "banowner", { identity: OWNER, reason: "r", retentionDays: 30, actor: "operator", now: NOW });
  await w.runAlarms(NOW + 60_000);
  assert.deepEqual(queue.asked, []);
  queue.down = false;
  await w.runAlarms(NOW + 10 * 60_000);
  assert.deepEqual(queue.asked, [{ slugs: ["aaaa2345"], urgent: true }]);
});
