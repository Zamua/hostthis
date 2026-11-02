import assert from "node:assert/strict";
import test from "node:test";

import { Feed, FEED_RETENTION_MS } from "../src/cells/feed.js";
import { Paste } from "../src/cells/paste.js";
import { FakeStorage, state } from "./harness.js";

// A FEED binding that records each append instead of reaching a cell.
function recordingEnv() {
  const notes = [];
  const FEED = {
    idFromName: (name) => name,
    get: () => ({
      fetch: async (req) => {
        notes.push(await req.json());
        return Response.json({ at: 0 });
      },
    }),
  };
  return { env: { FEED }, notes };
}

function paste(seed, env) {
  const p = new Paste(state(new FakeStorage(new Map(seed))), env);
  p.slug = "app1";
  return p;
}

async function index(p, since = 0) {
  const res = await p.roomIndex(new URL(`https://cell/paste/roomindex?since=${since}`));
  return (await res.json()).rooms;
}

async function decide(p, room, version, targetBytes) {
  return p.roomDecide({ app: "app1", room, version, targetBytes, appCap: 0 });
}

test("a room created with no ceiling lands in the index", async () => {
  const p = paste();
  p.now = () => 1000;
  await p.roomCreated({ id: "r1", subnet: "10.0.0.0/24", at: 900, fresh: true });
  assert.deepEqual(await index(p), [{ room: "r1", createdAt: 900, touchedAt: 1000 }]);
});

test("a size change touches the room, and since filters by touch", async () => {
  const p = paste();
  p.now = () => 1000;
  await p.roomCreated({ id: "r1", subnet: "s", at: 1000, fresh: true });
  await p.roomCreated({ id: "r2", subnet: "s", at: 1000, fresh: true });
  p.now = () => 5000;
  assert.equal((await decide(p, "r2", 1, 10)).status, 200);
  assert.deepEqual((await index(p, 2000)).map((r) => r.room), ["r2"]);
  assert.deepEqual(await index(p, 2000), [{ room: "r2", createdAt: 1000, touchedAt: 5000 }]);
});

test("a room older than the index enters it on its next size change", async () => {
  const p = paste([["roomAllocation:old", { version: 1, allocated: 5, target: 5 }], ["roomAllocated", 5]]);
  p.now = () => 7000;
  assert.equal((await decide(p, "old", 2, 8)).status, 200);
  assert.deepEqual(await index(p), [{ room: "old", createdAt: 7000, touchedAt: 7000 }]);
});

test("a removed room leaves the index", async () => {
  const p = paste();
  await p.roomCreated({ id: "r1", subnet: "s", at: 1, fresh: true });
  assert.equal((await p.roomRemoved({ room: "r1" })).status, 200);
  assert.deepEqual(await index(p), []);
});

test("backfill adds only missing rooms and keeps a present room's touch", async () => {
  const p = paste();
  p.now = () => 9000;
  await p.roomCreated({ id: "kept", subnet: "s", at: 9000, fresh: true });
  const rooms = [{ room: "kept", createdAt: 1, touchedAt: 1 }, { room: "new", createdAt: 2, touchedAt: 3 }];
  assert.deepEqual(await (await p.roomIndexAdd({ rooms })).json(), { added: 1, present: 1 });
  assert.deepEqual(await (await p.roomIndexAdd({ rooms })).json(), { added: 0, present: 2 });
  assert.deepEqual(await index(p), [
    { room: "kept", createdAt: 9000, touchedAt: 9000 },
    { room: "new", createdAt: 2, touchedAt: 3 },
  ]);
});

test("drop removes only the named index entries, and nothing of the rooms", async () => {
  const p = paste([["roomAllocation:a", { version: 1, allocated: 5, target: 5 }], ["roomAllocated", 5]]);
  await p.roomIndexAdd({ rooms: ["a", "b"].map((room) => ({ room, createdAt: 1, touchedAt: 1 })) });
  assert.deepEqual(await (await p.roomIndexDrop({ rooms: ["a", "missing"] })).json(), { dropped: 1 });
  assert.deepEqual((await index(p)).map((r) => r.room), ["b"]);
  assert.deepEqual(await p.state.storage.get("roomAllocation:a"), { version: 1, allocated: 5, target: 5 });
  assert.equal(await p.state.storage.get("roomAllocated"), 5);
  assert.equal((await p.roomIndexDrop({ rooms: [""] })).status, 400);
});

test("the index pages by key", async () => {
  const p = paste();
  await p.roomIndexAdd({ rooms: ["a", "b", "c"].map((room) => ({ room, createdAt: 1, touchedAt: 1 })) });
  const first = await (await p.roomIndex(new URL("https://cell/paste/roomindex?limit=2"))).json();
  assert.deepEqual(first.rooms.map((r) => r.room), ["a", "b"]);
  const rest = await (await p.roomIndex(new URL(`https://cell/paste/roomindex?limit=2&start=${encodeURIComponent(first.next)}`))).json();
  assert.deepEqual([rest.rooms.map((r) => r.room), rest.next], [["c"], null]);
});

test("room activity reaches the feed at most once per ten minutes", async () => {
  const { env, notes } = recordingEnv();
  const p = paste([], env);
  p.now = () => 0;
  await p.roomCreated({ id: "r1", subnet: "s", at: 0, fresh: true });
  p.now = () => 60_000;
  await p.roomCreated({ id: "r2", subnet: "s", at: 60_000, fresh: true });
  p.now = () => 10 * 60_000;
  await decide(p, "r1", 1, 4);
  assert.deepEqual(notes, [{ slug: "app1", kind: "rooms" }, { slug: "app1", kind: "rooms" }]);
});

test("a request that changes the row tells the feed once, and a read does not", async () => {
  const { env, notes } = recordingEnv();
  const row = { slug: "app1", generation: "g", updatedAt: 1, status: "ready", kind: "html" };
  const p = paste([["row", row], ["quarantine", { reason: "x", actor: "operator", at: 5 }]], env);
  const call = (op, body) => p.fetch(new Request(`https://cell/paste/${op}?slug=app1`, body === undefined ? {} : {
    method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body),
  }));
  assert.equal((await call("operatorrow")).status, 200);
  assert.deepEqual(notes, []);
  assert.equal((await call("release", {})).status, 200);
  assert.deepEqual(notes, [{ slug: "app1", kind: "paste" }]);
});

test("the feed lists entries since a time, oldest first, and prunes past retention", async () => {
  const feed = new Feed(state(new FakeStorage(new Map())));
  for (const [at, slug] of [[1000, "a"], [2000, "b"], [3000, "c"]]) {
    feed.now = () => at;
    await feed.append({ slug, kind: "paste" });
  }
  const page = await (await feed.since(new URL("https://cell/feed/since?at=1500&limit=1"))).json();
  assert.deepEqual(page.entries, [{ slug: "b", kind: "paste", at: 2000 }]);
  const rest = await (await feed.since(new URL(`https://cell/feed/since?at=1500&start=${encodeURIComponent(page.next)}`))).json();
  assert.deepEqual(rest.entries.map((e) => e.slug), ["c"]);
  feed.now = () => 2500 + FEED_RETENTION_MS;
  await feed.append({ slug: "d", kind: "rooms" });
  const all = await (await feed.since(new URL("https://cell/feed/since?at=0"))).json();
  assert.deepEqual(all.entries.map((e) => e.slug), ["c", "d"]);
});

test("the feed refuses an unknown kind", async () => {
  const feed = new Feed(state(new FakeStorage(new Map())));
  assert.equal((await feed.append({ slug: "a", kind: "other" })).status, 400);
});
