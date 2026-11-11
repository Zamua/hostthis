import assert from "node:assert/strict";
import test from "node:test";

import { Identity, Paste, Room } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";

const APP = "appslug1";
const ID = "11111111-1111-4111-8111-111111111111";

// One app: its Paste cell, one Room cell kept in memory between calls (so its
// quarantine answer is cached as in the runtime), and the owner's Identity cell.
function world() {
  const pasteStorage = new FakeStorage(new Map([["row", { slug: APP, status: "ready", kind: "site", identity: "key:owner" }]]));
  const identityStorage = new FakeStorage();
  const roomStorage = new FakeStorage();
  const env = {};
  const policyCalls = { n: 0 };
  const ns = (make) => ({ idFromName: (n) => n, get: () => ({ fetch: (req) => make().fetch(req) }) });
  env.PASTES = ns(() => ({
    fetch: (req) => {
      if (new URL(req.url).pathname.endsWith("/roompolicy")) {
        policyCalls.n++;
      }
      return new Paste(state(pasteStorage), env).fetch(req);
    },
  }));
  env.IDENTITY = ns(() => new Identity(state(identityStorage), env));
  const closed = [];
  const sockets = [{ close: (code, reason) => closed.push([code, reason]) }];
  const room = new Room({ ...state(roomStorage), getWebSockets: () => sockets }, env);
  const call = async (op, body) => {
    const res = await room.fetch(new Request(`https://cell/x/${op}`, { method: "POST", body: JSON.stringify(body ?? {}) }));
    const text = await res.text();
    let parsed = text;
    try {
      parsed = JSON.parse(text);
    } catch {
      // plain text answer
    }
    return { status: res.status, body: parsed };
  };
  const paste = (op, body) => new Paste(state(pasteStorage), env).fetch(
    new Request(`https://cell/paste/${op}?slug=${APP}`, { method: "POST", body: JSON.stringify(body) }));
  return { room, call, paste, closed, policyCalls, pasteStorage };
}

async function seeded(w) {
  const made = await w.call("create", {
    appSlug: APP, id: ID, createdAt: 1, updatedAt: 1, subnet: "10.0.0.0/24", at: 1, appCap: 1 << 20, roomLimit: 500,
  });
  assert.equal(made.body.created, true);
  const put = await w.call("put", { key: "k", value: btoa("hello"), wire: '"hello"', roomCap: 1 << 18, keyCap: 256, appCap: 1 << 20, now: 2 });
  assert.equal(put.status, 200);
}

// The cached answer is dropped, as after QUARANTINE_TTL_MS or an eviction.
function forget(w) {
  w.room.quarantine = undefined;
}

test("a quarantined app's room reads, writes and push routes answer as an absent room, and its sockets close", async () => {
  const w = world();
  await seeded(w);
  assert.equal((await w.call("scan")).status, 200);

  assert.equal((await w.paste("quarantine", { reason: "abuse", actor: "operator", at: 5 })).status, 200);
  forget(w);
  for (const op of ["meta", "get", "scan", "put", "del", "pushsublist", "pushscheduleget", "pushsubput", "pushsubdel", "pushscheduleput", "pushtest"]) {
    const res = await w.call(op, { key: "k", value: btoa("x"), wire: '"x"', roomCap: 1 << 18, keyCap: 256, appCap: 1 << 20, now: 6 });
    assert.deepEqual([op, res.status, res.body], [op, 404, "not found\n"]);
  }
  assert.ok(w.closed.length > 0);
  assert.deepEqual(w.closed[0], [1008, "unavailable"]);
  const join = await w.room.fetch(new Request("https://cell/x/join", { headers: { Upgrade: "websocket" } }));
  assert.equal(join.status, 404);

  // The value survived: a release brings the room back as it was.
  assert.equal((await w.paste("release", {})).status, 200);
  forget(w);
  const back = await w.call("scan");
  assert.equal(back.status, 200);
  assert.deepEqual(Object.keys(back.body.values), ["k"]);
});

test("a room asks its app about quarantine at most once a minute, and removal still works while quarantined", async () => {
  const w = world();
  await seeded(w);
  const before = w.policyCalls.n;
  for (let i = 0; i < 20; i++) {
    assert.equal((await w.call("scan")).status, 200);
  }
  assert.ok(w.policyCalls.n - before <= 1, `asked ${w.policyCalls.n - before} times for 20 reads`);

  assert.equal((await w.paste("quarantine", { reason: "abuse", actor: "operator", at: 5 })).status, 200);
  forget(w);
  assert.equal((await w.call("scan")).status, 404);
  assert.deepEqual((await w.call("remove", { now: 9 })).body, { removed: true });
});

test("a message on an open socket of a quarantined app's room closes the sockets and is not relayed", async () => {
  const w = world();
  await seeded(w);
  assert.equal((await w.paste("quarantine", { reason: "abuse", actor: "operator", at: 5 })).status, 200);
  forget(w);
  const sent = [];
  await w.room.webSocketMessage({ send: (m) => sent.push(m), close: () => {} }, '{"type":"hello"}');
  assert.deepEqual(sent, []);
  assert.deepEqual(w.closed[0], [1008, "unavailable"]);
});
