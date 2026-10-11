import assert from "node:assert/strict";
import test from "node:test";

import { Room } from "../src/index.js";
import {
  PING_FRAME, PONG_FRAME, ROOM_CLIENT_FRAME_LIMIT, ROOM_PING_STALE_MS, ROOM_SOCKET_CAP,
} from "../src/rooms/policy.js";
import { FakeStorage, state } from "./harness.js";

globalThis.WebSocketRequestResponsePair ??= class {
  constructor(request, response) {
    this.request = request;
    this.response = response;
  }
};

class FakeSocket {
  constructor(answered = null) {
    this.answered = answered;
    this.readyState = 1;
    this.closed = null;
    this.sent = [];
  }

  close(code, reason) {
    this.closed = [code, reason];
    this.readyState = 2;
  }

  send(data) {
    this.sent.push(data);
  }
}

// A Room over hibernation state holding the given sockets.
function room(sockets, env = {}) {
  const pairs = [];
  const cell = new Room({
    ...state(new FakeStorage()),
    getWebSockets: () => sockets,
    getWebSocketAutoResponseTimestamp: (ws) => (ws.answered === null ? null : new Date(ws.answered)),
    setWebSocketAutoResponse: (pair) => pairs.push(pair),
    acceptWebSocket() {
      throw new Error("a refused join must not accept a socket");
    },
  }, env);
  return { cell, pairs };
}

const join = () => new Request("https://cell/room/join?room=abcdefgh%7Cx", { headers: { Upgrade: "websocket" } });
const NOW = Date.parse("2026-10-11T12:00:00Z");

test("the runtime answers the reserved ping frame for the cell", () => {
  const { pairs } = room([]);
  assert.equal(pairs.length, 1);
  assert.equal(pairs[0].request, '{"type":"ping"}');
  assert.equal(pairs[0].response, '{"type":"pong"}');
  assert.equal(PING_FRAME, '{"type":"ping"}');
  assert.equal(PONG_FRAME, '{"type":"pong"}');
});

test("the 65th socket is refused before any accept", async () => {
  assert.equal(ROOM_SOCKET_CAP, 64);
  const sockets = Array.from({ length: 64 }, () => new FakeSocket());
  const res = await room(sockets).cell.fetch(join());
  assert.equal(res.status, 429);
  assert.equal(await res.text(), "room connection limit reached\n");
  assert.ok(sockets.every((ws) => ws.closed === null), "a socket that never pinged is kept");
});

test("a closing socket does not hold a place", () => {
  const sockets = Array.from({ length: 64 }, () => new FakeSocket());
  sockets[0].readyState = 2;
  assert.equal(room(sockets).cell.admit(NOW), null);
});

test("a socket whose last ping answer is stale is evicted at the join", () => {
  assert.equal(ROOM_PING_STALE_MS, 60_000);
  const sockets = Array.from({ length: 64 }, () => new FakeSocket(NOW - 1000));
  const stale = new FakeSocket(NOW - 60_001);
  sockets[10] = stale;
  assert.equal(room(sockets).cell.admit(NOW), null);
  assert.deepEqual(stale.closed, [1000, "stale"]);
  assert.ok(sockets.filter((ws) => ws !== stale).every((ws) => ws.closed === null));
});

test("eviction runs at every join, under the cap too", () => {
  const fresh = new FakeSocket(NOW - 60_000);
  const stale = new FakeSocket(NOW - 120_000);
  const silent = new FakeSocket();
  assert.equal(room([fresh, stale, silent]).cell.admit(NOW), null);
  assert.equal(fresh.closed, null);
  assert.deepEqual(stale.closed, [1000, "stale"]);
  assert.equal(silent.closed, null);
});

test("the stale window is a test hook in env", () => {
  const ws = new FakeSocket(NOW - 2000);
  room([ws], { ROOM_PING_STALE_MS: "1500" }).cell.admit(NOW);
  assert.deepEqual(ws.closed, [1000, "stale"]);
});

test("a client frame over 32 KiB closes its socket with 1009 and reaches no peer", async () => {
  assert.equal(ROOM_CLIENT_FRAME_LIMIT, 32 * 1024);
  const cases = [
    ["x".repeat(32 * 1024), true],
    ["x".repeat(32 * 1024 + 1), false],
    ["€".repeat(10922) + "xx", true],
    ["€".repeat(10923), false],
    ["\u{1f600}".repeat(8192), true],
    ["\u{1f600}".repeat(8192) + "x", false],
    [new ArrayBuffer(32 * 1024), true],
    [new ArrayBuffer(32 * 1024 + 1), false],
  ];
  for (const [frame, relayed] of cases) {
    const sender = new FakeSocket();
    const peer = new FakeSocket();
    await room([sender, peer]).cell.webSocketMessage(sender, frame);
    const what = `${typeof frame === "string" ? `string of ${frame.length}` : `buffer of ${frame.byteLength}`}`;
    assert.equal(peer.sent.length, relayed ? 1 : 0, what);
    assert.deepEqual(sender.closed, relayed ? null : [1009, "frame too large"], what);
  }
});

test("a reserved server frame still closes its sender with 1008", async () => {
  const sender = new FakeSocket();
  const peer = new FakeSocket();
  await room([sender, peer]).cell.webSocketMessage(sender, '{"type":"put","seq":1}');
  assert.deepEqual(sender.closed, [1008, "reserved control frame"]);
  assert.equal(peer.sent.length, 0);
});
