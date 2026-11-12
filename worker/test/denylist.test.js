import assert from "node:assert/strict";
import test from "node:test";

import {
  DENYLIST_CAP, Denylist, SNAPSHOT_MAX_AGE_MS, canonicalEntry, denylistPoll, snapshotKey,
} from "../src/cells/denylist.js";
import { FakeStorage, state } from "./harness.js";

const KEY = "SHA256:" + "ab".repeat(32);

// A payload bucket holding text, with switches that make a call throw.
function textBucket() {
  const b = {
    objects: new Map(),
    fail: { get: 0, put: false, delete: false },
    gets: 0,
    async get(key) {
      b.gets++;
      if (b.fail.get > 0) {
        b.fail.get--;
        throw new Error("bucket down");
      }
      const text = b.objects.get(key);
      return text === undefined ? null : { body: new Response(text).body };
    },
    async put(key, text) {
      if (b.fail.put) throw new Error("bucket down");
      b.objects.set(key, text);
    },
    async delete(key) {
      if (b.fail.delete) throw new Error("bucket down");
      b.objects.delete(key);
    },
  };
  return b;
}

const NAMESPACE = { idFromName: (name) => name };
const SNAPSHOT_KEY = snapshotKey({ DENYLIST: NAMESPACE });

function cell(bucket = textBucket()) {
  return new Denylist(state(new FakeStorage()), { PAYLOADS: bucket, DENYLIST: NAMESPACE });
}

// A Worker env whose DENYLIST binding reaches c, counting the calls.
function pollEnv(c, bucket) {
  const env = {
    PAYLOADS: bucket,
    cellCalls: 0,
    DENYLIST: {
      idFromName: (name) => name,
      get: () => ({
        fetch(request) {
          env.cellCalls++;
          return c.fetch(request);
        },
      }),
    },
  };
  return env;
}

async function polled(env, now) {
  const res = await denylistPoll(env, now);
  return { status: res.status, entries: (await res.json()).entries.map((e) => e.entry) };
}

async function op(c, name, body) {
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  const res = await c.fetch(new Request(`https://cell/denylist/${name}`, init));
  return { status: res.status, body: await res.json() };
}

test("canonicalEntry accepts a key fingerprint and masks a network", () => {
  assert.equal(canonicalEntry(KEY), KEY);
  assert.equal(canonicalEntry("203.0.113.77/24"), "203.0.113.0/24");
  assert.equal(canonicalEntry("10.1.2.3/8"), "10.0.0.0/8");
  assert.equal(canonicalEntry("2001:DB8:aa:bb::1/48"), "2001:db8:aa::/48");
  assert.equal(canonicalEntry("2001:db8:0:0:1:0:0:1/128"), "2001:db8::1:0:0:1/128");
  assert.equal(canonicalEntry("::/16"), "::/16");
});

test("canonicalEntry refuses anything else", () => {
  for (const bad of [
    "", "SHA256:" + "AB".repeat(32), "SHA256:abc", "key:" + KEY,
    "203.0.113.0", "203.0.113.0/7", "203.0.113.0/33", "256.0.0.0/24", "1.2.3/24",
    "2001:db8::/15", "2001:db8::/129", "2001:db8:::1/64", "1:2:3:4:5:6:7:8:9/64", "2001:db8::g/64",
    "01.2.3.4/24", " 203.0.113.0/24",
  ]) {
    assert.equal(canonicalEntry(bad), null, bad);
  }
});

test("add, list and remove round-trip in canonical form", async () => {
  const c = cell();
  assert.equal((await op(c, "add", { entry: KEY, reason: "abuse" })).status, 200);
  assert.equal((await op(c, "add", { entry: "198.51.100.9/24", reason: "" })).status, 200);
  const { body } = await op(c, "list");
  assert.deepEqual(body.entries.map((e) => [e.entry, e.reason]).sort(),
    [["198.51.100.0/24", ""], [KEY, "abuse"]]);
  assert.ok(body.entries.every((e) => Number.isSafeInteger(e.addedAt) && e.addedAt > 0));
  assert.equal((await op(c, "remove", { entry: "198.51.100.200/24" })).status, 200);
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.entry), [KEY]);
});

test("adding a present entry replaces its reason and keeps one row", async () => {
  const c = cell();
  await op(c, "add", { entry: KEY, reason: "first" });
  await op(c, "add", { entry: KEY, reason: "second" });
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.reason), ["second"]);
});

test("removing an absent entry succeeds", async () => {
  const res = await op(cell(), "remove", { entry: KEY });
  assert.equal(res.status, 200);
  assert.equal(res.body.removed, false);
});

test("a malformed entry or reason is refused", async () => {
  const c = cell();
  for (const body of [{ entry: "nope", reason: "" }, { entry: KEY, reason: "x".repeat(201) }, { entry: KEY }]) {
    const res = await op(c, "add", body);
    assert.equal(res.status, 400, JSON.stringify(body));
    assert.equal(res.body.error, "invalid-entry");
  }
  assert.equal((await op(c, "remove", { entry: "nope" })).status, 400);
  assert.deepEqual((await op(c, "list")).body.entries, []);
});

test("the list stops at its cap, and replacing an entry still works there", async () => {
  const c = cell();
  for (let i = 0; i < DENYLIST_CAP; i++) {
    await op(c, "add", { entry: `10.${i >> 8}.${i & 255}.0/24`, reason: "" });
  }
  const full = await op(c, "add", { entry: KEY, reason: "" });
  assert.equal(full.status, 507);
  assert.equal(full.body.error, "denylist-full");
  assert.equal((await op(c, "add", { entry: "10.0.0.0/24", reason: "again" })).status, 200);
});

test("an expiring entry lists until its time and then stops counting", async () => {
  const c = cell();
  let now = 1_800_000_000_000;
  c.now = () => now;
  const res = await op(c, "add", { entry: "203.0.113.0/24", reason: "", expiresAt: now + 1000 });
  assert.equal(res.status, 200);
  assert.equal(res.body.expiresAt, now + 1000);
  await op(c, "add", { entry: KEY, reason: "" });
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.entry).sort(), ["203.0.113.0/24", KEY].sort());
  now += 1000;
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.entry), [KEY]);
});

test("an expiry in the past or of the wrong type is refused", async () => {
  const c = cell();
  c.now = () => 1_800_000_000_000;
  for (const expiresAt of [1_800_000_000_000, "soon", 1.5]) {
    assert.equal((await op(c, "add", { entry: KEY, reason: "", expiresAt })).status, 400, String(expiresAt));
  }
});

test("expired entries free their slots under the cap", async () => {
  const c = cell();
  let now = 1_800_000_000_000;
  c.now = () => now;
  for (let i = 0; i < DENYLIST_CAP; i++) {
    await op(c, "add", { entry: `10.${i >> 8}.${i & 255}.0/24`, reason: "", expiresAt: now + 1000 });
  }
  assert.equal((await op(c, "add", { entry: KEY, reason: "" })).status, 507);
  now += 1000;
  assert.equal((await op(c, "add", { entry: KEY, reason: "" })).status, 200);
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.entry), [KEY]);
});

test("a poll reads the snapshot and reaches the cell only without one", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  assert.deepEqual(await polled(env), { status: 200, entries: [] });
  assert.equal(env.cellCalls, 1, "no snapshot yet: the cell answers and writes one");
  assert.deepEqual(await polled(env), { status: 200, entries: [] });
  assert.equal(env.cellCalls, 1, "the snapshot answers");
});

test("a ban and an unban reach the very next poll", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  await polled(env);
  await op(c, "add", { entry: KEY, reason: "abuse" });
  assert.deepEqual((await polled(env)).entries, [KEY]);
  await op(c, "remove", { entry: KEY });
  assert.deepEqual((await polled(env)).entries, []);
  assert.equal(env.cellCalls, 1, "neither poll after a change needed the cell");
});

test("a snapshot that cannot be used sends the poll to the cell, never to an empty list", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  await op(c, "add", { entry: KEY, reason: "" });
  for (const [name, damage] of [
    ["missing", () => bucket.objects.delete(SNAPSHOT_KEY)],
    ["not JSON", () => bucket.objects.set(SNAPSHOT_KEY, "{")],
    ["wrong shape", () => bucket.objects.set(SNAPSHOT_KEY, JSON.stringify({ writtenAt: Date.now() }))],
    ["no time", () => bucket.objects.set(SNAPSHOT_KEY, JSON.stringify({ entries: [] }))],
    ["from the future", () => bucket.objects.set(SNAPSHOT_KEY, JSON.stringify({ writtenAt: Date.now() + 60_000, entries: [] }))],
    ["bucket read failing", () => { bucket.fail.get = 3; }],
  ]) {
    damage();
    const before = env.cellCalls;
    assert.deepEqual(await polled(env), { status: 200, entries: [KEY] }, name);
    assert.equal(env.cellCalls, before + 1, name);
  }
});

test("a snapshot past its age is rebuilt from the cell", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  await op(c, "add", { entry: KEY, reason: "" });
  const written = JSON.parse(bucket.objects.get(SNAPSHOT_KEY)).writtenAt;
  assert.deepEqual((await polled(env, written + SNAPSHOT_MAX_AGE_MS - 1)).entries, [KEY]);
  assert.equal(env.cellCalls, 0);
  assert.deepEqual((await polled(env, written + SNAPSHOT_MAX_AGE_MS)).entries, [KEY]);
  assert.equal(env.cellCalls, 1);
});

test("a snapshot never serves an entry past its expiry", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  const now = Date.now();
  await op(c, "add", { entry: KEY, reason: "", expiresAt: now + 60_000 });
  assert.deepEqual((await polled(env, now + 59_000)).entries, [KEY]);
  assert.deepEqual((await polled(env, now + 60_000)).entries, []);
  assert.equal(env.cellCalls, 0);
});

test("a ban whose snapshot write fails removes the snapshot, so polls see the ban", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  const env = pollEnv(c, bucket);
  await polled(env);
  bucket.fail.put = true;
  assert.equal((await op(c, "add", { entry: KEY, reason: "" })).status, 200);
  assert.equal(bucket.objects.has(SNAPSHOT_KEY), false);
  assert.deepEqual(await polled(env), { status: 200, entries: [KEY] });
});

test("a ban that can neither write nor remove the snapshot reports the failure", async () => {
  const bucket = textBucket();
  const c = cell(bucket);
  bucket.fail.put = true;
  bucket.fail.delete = true;
  await assert.rejects(op(c, "add", { entry: KEY, reason: "" }), /bucket down/);
  // The poll that reaches the cell still gets the list.
  bucket.objects.delete(SNAPSHOT_KEY);
  assert.deepEqual(await polled(pollEnv(c, bucket)), { status: 200, entries: [KEY] });
});
