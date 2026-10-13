// The public room WebSocket under workerd (Miniflare, from worker/localrt),
// where the hibernation API is real. Skipped until `npm ci --prefix
// worker/localrt` has installed the runtime; `make test` installs it.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";

import { bytesBase64, roomWireValue } from "../src/rooms/wire.js";

const workerDir = fileURLToPath(new URL("..", import.meta.url));
const APEX = "example.test";
const APP = "abcdefgh";
const TOKEN = "live-token";
const STALE_MS = 1500;

let Miniflare = null;
try {
  ({ Miniflare } = createRequire(`${workerDir}localrt/package.json`)("miniflare"));
} catch {
  // not installed
}
const skip = Miniflare ? false : "worker/localrt is not installed";

let mf;
// The same bundle with no apex: off mode, where hostthisd's proxy fronts the room.
let mfOff;

function runtime(bindings) {
  const config = JSON.parse(readFileSync(`${workerDir}wrangler.jsonc`, "utf8")
    .split("\n").filter((line) => !line.trim().startsWith("//")).join("\n"));
  return new Miniflare({
    modules: true,
    scriptPath: `${workerDir}dist/index.js`,
    modulesRoot: workerDir,
    modulesRules: [{ type: "ESModule", include: ["**/*.js"] }, { type: "CompiledWasm", include: ["**/*.wasm"] }],
    compatibilityDate: config.compatibility_date,
    cf: false,
    bindings,
    r2Buckets: Object.fromEntries(config.r2_buckets.map((b) => [b.binding, b.bucket_name])),
    durableObjectsPersist: false,
    durableObjects: Object.fromEntries(config.durable_objects.bindings
      .map((b) => [b.name, { className: b.class_name, useSQLite: true }])),
  });
}

before(async () => {
  if (skip) return;
  mf = runtime({ HOSTTHIS_APEX_DOMAIN: APEX, HOSTTHIS_WORKER_TOKEN: TOKEN, ROOM_PING_STALE_MS: String(STALE_MS) });
  mfOff = runtime({ HOSTTHIS_WORKER_TOKEN: TOKEN, ROOM_PING_STALE_MS: String(STALE_MS) });
  await Promise.all([mf.ready, mfOff.ready]);
  const res = await mf.dispatchFetch(`http://worker.internal/paste/put?slug=${APP}`, {
    method: "POST",
    headers: { Authorization: `Bearer ${TOKEN}` },
    body: JSON.stringify({
      generation: "g1", fingerprint: "f1",
      row: { slug: APP, identity: "owner", status: "ready", kind: "markdown", size: 1, name: "", pinnedVersion: 0, createdAt: 1, updatedAt: 1 },
    }),
  });
  assert.equal(res.status, 204, await res.text());
});

after(async () => {
  await mf?.dispose();
  await mfOff?.dispose();
});

async function newRoom() {
  const res = await mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms`, { method: "POST" });
  assert.equal(res.status, 201);
  return (await res.json()).id;
}

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// An accepted client socket with its frames queued and its close recorded.
async function connect(id, headers = {}) {
  return accepted(await mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms/${id}/ws`, {
    headers: { Upgrade: "websocket", ...headers },
  }));
}

// hostthisd's join, on the off-mode runtime.
async function connectOff(id) {
  return accepted(await mfOff.dispatchFetch(`http://worker.internal/room/join?room=${encodeURIComponent(`${APP}|${id}`)}`, {
    headers: { Upgrade: "websocket", Authorization: `Bearer ${TOKEN}` },
  }));
}

async function accepted(res) {
  if (res.status !== 101) {
    return { res };
  }
  const ws = res.webSocket;
  const frames = [];
  let wake = null;
  const client = { res, ws, frames, closed: null };
  ws.addEventListener("message", (event) => {
    frames.push(event.data);
    wake?.();
  });
  ws.addEventListener("close", (event) => {
    client.closed = { code: event.code, reason: event.reason };
    wake?.();
  });
  ws.accept();
  client.next = async (ms = 2000) => {
    const deadline = Date.now() + ms;
    while (!frames.length && !client.closed && Date.now() < deadline) {
      await Promise.race([new Promise((resolve) => { wake = resolve; }), delay(deadline - Date.now())]);
    }
    return frames.shift();
  };
  client.closedWithin = async (ms = 2000) => {
    const deadline = Date.now() + ms;
    while (!client.closed && Date.now() < deadline) await delay(20);
    return client.closed;
  };
  client.snapshot = await client.next();
  assert.equal(JSON.parse(client.snapshot).type, "snapshot");
  return client;
}

// The shared room value vectors, but those nested 10000 deep, plus two keys
// whose UTF-8 order differs from their UTF-16 order.
const VALUES = [
  ...JSON.parse(readFileSync(new URL("../../testdata/room-wire-vectors.json", import.meta.url), "utf8")).vectors
    .filter((v) => !v.nest)
    .map((v) => [v.name, v.hex !== undefined ? new Uint8Array(Buffer.from(v.hex, "hex")) : new TextEncoder().encode(v.in)]),
  ["\u{1f600}", new TextEncoder().encode("1")],
  ["�", new TextEncoder().encode("2")],
];

// The keys as Go sorts a map's: by their UTF-8 bytes.
const goOrder = (entries) => [...entries].sort(([a], [b]) => Buffer.compare(Buffer.from(a), Buffer.from(b)));

const room = (wire) => `{${goOrder(Object.entries(wire)).map(([key, text]) => `${JSON.stringify(key)}:${text}`).join(",")}}`;

test("a join answers 101 from the cell, no-store, snapshot first", { skip }, async () => {
  const id = await newRoom();
  await mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms/${id}/k`, { method: "PUT", body: '{"a":1}' });
  const res = await mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms/${id}/ws`, {
    headers: { Upgrade: "websocket", Origin: `https://${APP}.${APEX}` },
  });
  assert.equal(res.status, 101);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  res.webSocket.accept();
  const first = await new Promise((resolve) => res.webSocket.addEventListener("message", (e) => resolve(e.data)));
  assert.deepEqual(JSON.parse(first), { type: "snapshot", seq: 1, state: { k: { a: 1 } } });
  res.webSocket.close(1000);
});

test("a foreign origin is refused before the upgrade", { skip }, async () => {
  const id = await newRoom();
  const { res } = await connect(id, { Origin: "https://evil.test" });
  assert.equal(res.status, 403);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
});

test("the ping frame is answered by the runtime and never relayed", { skip }, async () => {
  const id = await newRoom();
  const a = await connect(id);
  const b = await connect(id);
  a.ws.send('{"type":"ping"}');
  assert.equal(await a.next(), '{"type":"pong"}');
  a.ws.send("after");
  assert.equal(await b.next(), "after", "the ping reached no peer");
  // Another spelling is an ordinary frame.
  a.ws.send('{"type": "ping"}');
  assert.equal(await b.next(), '{"type": "ping"}');
  a.ws.close(1000);
  b.ws.close(1000);
});

test("a frame over 32 KiB closes its sender with 1009", { skip }, async () => {
  const id = await newRoom();
  const a = await connect(id);
  const b = await connect(id);
  const c = await connect(id);
  a.ws.send("x".repeat(32 * 1024));
  assert.equal((await b.next()).length, 32 * 1024);
  a.ws.send("y".repeat(32 * 1024 + 1));
  assert.equal((await a.closedWithin())?.code, 1009);
  c.ws.send("marker");
  assert.equal(await b.next(), "marker", "the oversized frame reached no peer");
  b.ws.close(1000);
  c.ws.close(1000);
});

test("a room holds 64 sockets and refuses the 65th with 429", { skip }, async () => {
  const id = await newRoom();
  const clients = [];
  for (let n = 0; n < 64; n++) {
    const client = await connect(id);
    assert.equal(client.res.status, 101, `socket ${n}`);
    clients.push(client);
  }
  const { res } = await connect(id);
  assert.equal(res.status, 429);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal(await res.text(), "room connection limit reached\n\n");
  clients.pop().ws.close(1000);
  let again;
  for (let tries = 0; tries < 50; tries++) {
    again = await connect(id);
    if (again.res.status === 101) break;
    await delay(20);
  }
  assert.equal(again.res.status, 101, "a closed socket frees its place");
  for (const client of [...clients, again]) client.ws.close(1000);
});

test("only a join at the cap evicts a stale pinger, with 4001, and never a socket that never pinged", { skip }, async () => {
  const id = await newRoom();
  const pinger = await connect(id);
  const silent = await connect(id);
  pinger.ws.send('{"type":"ping"}');
  assert.equal(await pinger.next(), '{"type":"pong"}');
  await delay(STALE_MS + 500);
  const clients = [];
  for (let n = 2; n < 64; n++) {
    clients.push(await connect(id));
  }
  assert.equal(await pinger.closedWithin(300), null, "a join under the cap evicts nothing");
  const late = await connect(id);
  assert.equal(late.res.status, 101);
  assert.deepEqual(await pinger.closedWithin(), { code: 4001, reason: "stale" });
  late.ws.send("hello");
  assert.equal(await silent.next(), "hello");
  assert.equal(silent.closed, null);
  for (const client of [silent, late, ...clients]) client.ws.close(1000);
});

test("hostthisd's internal join still answers with a socket and a snapshot", { skip }, async () => {
  const id = await newRoom();
  const res = await mf.dispatchFetch(`http://worker.internal/room/join?room=${encodeURIComponent(`${APP}|${id}`)}`, {
    headers: { Upgrade: "websocket", Authorization: `Bearer ${TOKEN}` },
  });
  assert.equal(res.status, 101);
  res.webSocket.accept();
  const first = await new Promise((resolve) => res.webSocket.addEventListener("message", (e) => resolve(e.data)));
  assert.equal(JSON.parse(first).type, "snapshot");
  res.webSocket.close(1000);
});

test("off mode relays the ping frame and frames over 32 KiB, and admits a 65th socket", { skip }, async () => {
  const id = crypto.randomUUID();
  const a = await connectOff(id);
  const b = await connectOff(id);
  a.ws.send('{"type":"ping"}');
  assert.equal(await b.next(), '{"type":"ping"}');
  const big = "y".repeat(40 * 1024);
  a.ws.send(big);
  assert.ok((await b.next()) === big, "a frame over 32 KiB reached the peer");
  assert.equal(await a.next(250), undefined, "no pong came back");
  assert.equal(a.closed, null);
  const more = [];
  for (let n = 2; n < 65; n++) {
    const client = await connectOff(id);
    assert.equal(client.res.status, 101, `socket ${n}`);
    more.push(client);
  }
  for (const client of [a, b, ...more]) client.ws.close(1000);
});

test("the snapshot and put frames carry each value byte for byte as the whole-room GET serves it", { skip }, async () => {
  const id = await newRoom();
  const put = (key, bytes) => mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms/${id}/${encodeURIComponent(key)}`,
    { method: "PUT", body: bytes });
  for (const [key, bytes] of VALUES) {
    assert.equal((await put(key, bytes)).status, 204, key);
  }
  const body = await (await mf.dispatchFetch(`http://${APP}.${APEX}/api/rooms/${id}`)).text();
  const wire = Object.fromEntries(VALUES.map(([key, bytes]) => [key, roomWireValue(bytes)]));
  assert.equal(body, `${room(wire)}\n`);
  const client = await connect(id);
  assert.equal(client.snapshot, `{"type":"snapshot","seq":${VALUES.length},"state":${body.slice(0, -1)}}`);
  let seq = VALUES.length;
  for (const [key, bytes] of VALUES) {
    assert.equal((await put(key, bytes)).status, 204, key);
    seq++;
    assert.equal(await client.next(), `{"type":"put","seq":${seq},"key":${JSON.stringify(key)},"value":${wire[key]}}`, key);
  }
  client.ws.close(1000);
});

test("off mode: the frames carry each value's stored text byte for byte, as the scan answers it", { skip }, async () => {
  const id = crypto.randomUUID();
  const op = (name, init = {}) => mfOff.dispatchFetch(
    `http://worker.internal/room/${name}?room=${encodeURIComponent(`${APP}|${id}`)}`,
    { ...init, headers: { Authorization: `Bearer ${TOKEN}` } },
  );
  const created = await op("create", {
    method: "POST",
    body: JSON.stringify({ appSlug: APP, id, createdAt: 1, updatedAt: 1, subnet: "unknown", at: Date.now(), appCap: 0 }),
  });
  assert.equal(created.status, 200, await created.text());
  // hostthisd spells a string as Go's encoder does, escaping <, > and &:
  // whatever text a writer stored is the text every reader gets.
  const wire = Object.fromEntries(VALUES.map(([key, bytes]) => [key, roomWireValue(bytes).replace(/[<>&]/g,
    (c) => `\\u00${c.charCodeAt(0).toString(16)}`)]));
  const put = (key, bytes) => op("put", {
    method: "POST",
    body: JSON.stringify({
      key, value: bytesBase64(bytes), wire: wire[key], roomCap: 256 * 1024, keyCap: 256, appCap: 0, now: Date.now(),
    }),
  });
  for (const [key, bytes] of VALUES) {
    assert.equal((await put(key, bytes)).status, 200, key);
  }
  const scan = await (await op("scan")).json();
  assert.deepEqual(scan.wire, wire);
  const client = await connectOff(id);
  assert.equal(client.snapshot, `{"type":"snapshot","seq":${VALUES.length},"state":${room(wire)}}`);
  let seq = VALUES.length;
  for (const [key, bytes] of VALUES) {
    assert.equal((await put(key, bytes)).status, 200, key);
    seq++;
    assert.equal(await client.next(), `{"type":"put","seq":${seq},"key":${JSON.stringify(key)},"value":${wire[key]}}`, key);
  }
  client.ws.close(1000);
});
