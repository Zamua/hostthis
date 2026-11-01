import assert from "node:assert/strict";
import test from "node:test";

import { Identity, Paste, Room } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";

const APP = "appslug1";
const ID = "11111111-1111-4111-8111-111111111111";

// One app: its Paste cell, one Room cell, and the owner's Identity cell.
function world() {
  const pasteStorage = new FakeStorage(new Map([["row", { slug: APP, status: "ready", kind: "site", identity: "key:owner" }]]));
  const identityStorage = new FakeStorage();
  const roomStorage = new FakeStorage();
  const env = {};
  const ns = (make) => ({ idFromName: (n) => n, get: () => ({ fetch: (req) => make().fetch(req) }) });
  env.PASTES = ns(() => new Paste(state(pasteStorage), env));
  env.IDENTITY = ns(() => new Identity(state(identityStorage), env));
  const room = () => new Room(state(roomStorage), env);
  const call = async (cell, op, body) => {
    const res = await cell.fetch(new Request(`https://cell/x/${op}`, { method: "POST", body: JSON.stringify(body) }));
    return { status: res.status, body: await res.json() };
  };
  return { pasteStorage, roomStorage, room, paste: () => new Paste(state(pasteStorage), env), call };
}

async function created(w) {
  const res = await w.call(w.room(), "create", {
    appSlug: APP, id: ID, createdAt: 1, updatedAt: 1, subnet: "10.0.0.0/24", at: 1, appCap: 1 << 20, roomLimit: 500,
  });
  assert.equal(res.body.created, true);
}

test("removing a room releases its bytes, stops it counting and empties the cell", async () => {
  const w = world();
  await created(w);
  const put = await w.call(w.room(), "put", { key: "k", value: btoa("x".repeat(300)), wire: '"x"', roomCap: 1 << 18, keyCap: 256, appCap: 1 << 20, now: 2 });
  assert.equal(put.status, 200);
  assert.ok((w.pasteStorage.data.get("roomAllocated") ?? 0) > 0);
  assert.equal(w.pasteStorage.data.get("roomTotal"), 1);

  assert.deepEqual((await w.call(w.room(), "remove", { now: 3 })).body, { removed: true });
  assert.equal(w.pasteStorage.data.get("roomAllocated"), 0);
  assert.equal(w.pasteStorage.data.get("roomTotal"), 0);
  assert.equal(w.pasteStorage.data.has(`roomAllocation:${ID}`), false);
  assert.equal(w.roomStorage.data.size, 0);

  assert.deepEqual((await w.call(w.room(), "remove", { now: 4 })).body, { removed: false, reason: "absent" });
  assert.equal(w.pasteStorage.data.get("roomTotal"), 0);
});

test("the app cell counts a removal once and refuses one still charged", async () => {
  const w = world();
  await created(w);
  const p = w.paste();
  assert.equal((await p.roomRemoved({ room: ID })).status, 200);
  assert.deepEqual(await (await p.roomRemoved({ room: ID })).json(), { counted: false });
  assert.equal(w.pasteStorage.data.get("roomTotal"), 0);
  w.pasteStorage.data.set("roomAllocation:other", { version: 1, allocated: 5, target: 5 });
  assert.equal((await p.roomRemoved({ room: "other" })).status, 409);
});
