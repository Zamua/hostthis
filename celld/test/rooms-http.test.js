import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import worker, { Paste, Room } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";

const APEX = "example.com";
const APP = "abcdefgh";
const OTHER = "bcdefghj";
const TOKEN = "t0ken";
const FORWARDED = "X-Hostthis-Forwarded-Host";
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const MISSING_ROOM = "22222222-2222-4222-8222-222222222222";
const CLASSES = { PASTES: Paste, ROOMS: Room };

// The public Worker over real Paste and Room cells in memory. Every cell
// request is recorded with who made it, the router or a cell; a join is
// recorded and answered by joinAnswer, since node has no WebSocketPair.
function world(vars = {}) {
  const storages = new Map();
  const calls = [];
  const joins = [];
  const sends = [];
  const storageOf = (ns, name) => {
    const key = `${ns}/${name}`;
    if (!storages.has(key)) storages.set(key, new FakeStorage());
    return storages.get(key);
  };
  const w = {
    calls,
    joins,
    sends,
    storageOf,
    joinAnswer: () => new Response("room connection limit reached\n", { status: 429 }),
  };
  const env = {
    HOSTTHIS_APEX_DOMAIN: APEX,
    ...vars,
    PUSH_FETCH: async (endpoint) => {
      sends.push(endpoint);
      return new Response(null, { status: 201 });
    },
  };
  const cellEnv = { ...env };
  for (const [target, by] of [[env, "router"], [cellEnv, "cell"]]) {
    for (const ns of Object.keys(CLASSES)) {
      target[ns] = {
        idFromName(name) { return name; },
        get(name) {
          return {
            async fetch(request) {
              const url = new URL(request.url);
              calls.push({ by, ns, name, path: url.pathname, query: [...url.searchParams.keys()].sort() });
              if (ns === "ROOMS" && url.pathname === "/room/join") {
                joins.push({ name, request });
                return w.joinAnswer();
              }
              return new CLASSES[ns](state(storageOf(ns, name)), cellEnv).fetch(request);
            },
          };
        },
      };
    }
  }
  w.env = env;
  w.fetch = (url, init = {}) => worker.fetch(new Request(url, init), env);
  w.app = (slug = APP) => storageOf("PASTES", slug).data.set("row", { slug, status: "ready", kind: "markdown" });
  w.room = async (slug = APP) => {
    const res = await w.fetch(`http://${slug}.${APEX}/api/rooms`, { method: "POST" });
    assert.equal(res.status, 201, await res.clone().text());
    return (await res.json()).id;
  };
  w.doc = (slug, id) => storageOf("ROOMS", `${slug}|${id}`).data.get("state");
  return w;
}

const url = (path, slug = APP) => `http://${slug}.${APEX}${path}`;

async function goError(res, status, text) {
  assert.equal(res.status, status);
  assert.equal(res.headers.get("Content-Type"), "text/plain; charset=utf-8");
  assert.equal(res.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal(await res.text(), text);
}

const notFound = (res) => goError(res, 404, "404 page not found\n");

// The public router's 404 for a path outside the rooms surface.
async function plainNotFound(res) {
  assert.equal(res.status, 404);
  assert.equal(res.headers.get("Cache-Control"), null);
  assert.equal(await res.text(), "404 page not found\n");
}

test("creating a room mints a UUIDv4 under the host's app", async () => {
  const w = world();
  w.app();
  const res = await w.fetch(url("/api/rooms"), { method: "POST" });
  assert.equal(res.status, 201);
  assert.equal(res.headers.get("Content-Type"), "application/json; charset=utf-8");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  const { id } = await res.json();
  assert.match(id, UUID_V4);
  const doc = w.doc(APP, id);
  assert.equal(doc.meta.appSlug, APP);
  assert.equal(doc.meta.id, id);
  const ledger = w.storageOf("PASTES", APP).data.get("roomLedger");
  assert.deepEqual(ledger.map((e) => [e.id, e.subnet]), [[id, "unknown"]]);
  // The trailing-slash spelling is the same collection.
  assert.equal((await w.fetch(url("/api/rooms/"), { method: "POST" })).status, 201);
});

test("room creation needs a live app and a POST", async () => {
  const w = world();
  await notFound(await w.fetch(url("/api/rooms"), { method: "POST" }));
  assert.deepEqual(w.calls.map((c) => `${c.ns}${c.path}`), ["PASTES/paste/exists"]);
  w.app();
  for (const method of ["GET", "PUT", "DELETE", "HEAD", "OPTIONS"]) {
    const res = await w.fetch(url("/api/rooms"), { method });
    assert.equal(res.status, 405, method);
    assert.equal(res.headers.get("Allow"), "POST");
    assert.equal(res.headers.get("Cache-Control"), "no-store");
    if (method !== "HEAD") assert.equal(await res.text(), "method not allowed\n\n");
  }
});

test("an id the cell already holds is never handed out: creation retries a fresh one", async () => {
  const w = world();
  w.app();
  const ids = Array.from({ length: 7 }, (_, n) => `${n}1111111-1111-4111-8111-111111111111`);
  const taken = (id) => w.storageOf("ROOMS", `${APP}|${id}`).data.set("state", {
    meta: { appSlug: APP, id, createdAt: 1, updatedAt: 1 }, kv: {}, wire: {}, bytes: 0, seq: 0, budgetVersion: 0,
  });
  const mint = crypto.randomUUID;
  let next = 0;
  crypto.randomUUID = () => ids[next++];
  try {
    taken(ids[0]);
    let res = await w.fetch(url("/api/rooms"), { method: "POST" });
    assert.equal(res.status, 201);
    assert.deepEqual(await res.json(), { id: ids[1] });
    for (const id of ids.slice(2)) taken(id);
    await goError(await w.fetch(url("/api/rooms"), { method: "POST" }), 500, "internal error\n\n");
    assert.equal(next, 7, "five tries, then a fault");
  } finally {
    crypto.randomUUID = mint;
  }
});

function seedLedger(w, entries) {
  w.storageOf("PASTES", APP).data.set("roomLedger", entries);
}

test("creation is limited to 60 per subnet and 300 per app per hour", async () => {
  const now = Date.now();
  const w = world({ HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" });
  w.app();
  const from = (ip) => ({ method: "POST", headers: { "CF-Connecting-IP": ip } });
  seedLedger(w, Array.from({ length: 60 }, (_, n) => ({ id: `s${n}`, subnet: "203.0.113.0/24", at: now })));
  const limited = await w.fetch(url("/api/rooms"), from("203.0.113.77"));
  assert.equal(limited.headers.get("Retry-After"), "3600");
  await goError(limited, 429, "room creation rate limit reached\n\n");
  assert.equal((await w.fetch(url("/api/rooms"), from("198.51.100.1"))).status, 201);
  // An hour later the subnet's rows have aged out.
  seedLedger(w, Array.from({ length: 60 }, (_, n) => ({ id: `s${n}`, subnet: "203.0.113.0/24", at: now - 3_600_001 })));
  assert.equal((await w.fetch(url("/api/rooms"), from("203.0.113.77"))).status, 201);
  seedLedger(w, Array.from({ length: 300 }, (_, n) => ({ id: `a${n}`, subnet: `10.0.${n}.0/24`, at: now })));
  assert.equal((await w.fetch(url("/api/rooms"), from("192.0.2.1"))).status, 429);
});

test("the creation ledger counts the subnet of the configured client address", async () => {
  const subnetOf = async (vars, headers, forwarded = false) => {
    const w = world(vars);
    w.app();
    const target = forwarded ? "http://celld.internal/api/rooms" : url("/api/rooms");
    const res = await w.fetch(target, { method: "POST", headers });
    assert.equal(res.status, 201);
    return w.storageOf("PASTES", APP).data.get("roomLedger")[0].subnet;
  };
  const headers = {
    "CF-Connecting-IP": "203.0.113.7",
    "X-Forwarded-For": "198.51.100.200, 2001:db8:1234:5678::1",
    "X-Hostthis-Client-IP": "192.0.2.9",
  };
  assert.equal(await subnetOf({}, headers), "unknown");
  assert.equal(await subnetOf({ HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" }, headers), "203.0.113.0/24");
  assert.equal(await subnetOf({ HOSTTHIS_CLIENT_IP_SOURCE: "xff-rightmost" }, headers), "2001:db8:1234::/48");
  assert.equal(await subnetOf({ HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" }, {}), "unknown");
  // A direct request's X-Hostthis-Client-IP is never trusted; a forwarded one's is.
  assert.equal(await subnetOf({ HOSTTHIS_CLIENT_IP_SOURCE: "x-hostthis-client-ip" }, headers), "unknown");
  assert.equal(await subnetOf(
    { HOSTTHIS_CELLD_TOKEN: TOKEN, HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" },
    { ...headers, Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${APP}.${APEX}` },
    true,
  ), "192.0.2.0/24");
});

async function put(w, id, key, value, slug = APP) {
  return w.fetch(url(`/api/rooms/${id}/${key}`, slug), { method: "PUT", body: value });
}

test("a scan re-encodes every value from its bytes", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  assert.equal((await put(w, id, "obj", '{ "a" : [1, true] }')).status, 204);
  assert.equal((await put(w, id, "text", "hello")).status, 204);
  assert.equal((await put(w, id, "big", "1e400")).status, 204);
  assert.equal((await put(w, id, "bad", new Uint8Array([0x61, 0xe2, 0x82]))).status, 204);
  assert.equal((await put(w, id, "badjson", new Uint8Array([0x22, 0x61, 0xe2, 0x82, 0xff, 0x22]))).status, 204);
  assert.equal((await put(w, id, "empty", "")).status, 204);
  const res = await w.fetch(url(`/api/rooms/${id}`));
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Content-Type"), "application/json; charset=utf-8");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  const bytes = new Uint8Array(await res.arrayBuffer());
  const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  assert.ok(text.endsWith("}\n"));
  assert.ok(text.includes('"big":1e400'), "a JSON value is embedded as stored");
  assert.ok(text.includes('"obj":{ "a" : [1, true] }'), "whitespace and all");
  const scanned = JSON.parse(text);
  assert.deepEqual(scanned, {
    obj: { a: [1, true] }, text: "hello", big: Infinity, bad: "a\ufffd\ufffd", badjson: "a\ufffd\ufffd\ufffd",
    empty: "",
  });
  // The text stored at write time, which the frames carry too.
  const { wire } = w.doc(APP, id);
  const members = Object.keys(wire).sort().map((key) => `${JSON.stringify(key)}:${wire[key]}`);
  assert.equal(text, `{${members.join(",")}}\n`);
});

test("a scan serves each value's stored text, keys in Go's order", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  for (const key of ["%F0%9F%98%80", "%EF%BF%BD", "b", "1"]) {
    assert.equal((await put(w, id, key, key === "b" ? "<" : "1")).status, 204);
  }
  // Text hostthisd stored keeps its spelling.
  w.doc(APP, id).wire.b = '"\\u003c"';
  const res = await w.fetch(url(`/api/rooms/${id}`));
  assert.equal(await res.text(), '{"1":1,"b":"\\u003c","\ufffd":1,"\u{1f600}":1}\n');
});

test("an empty room scans to {} and a missing one to 404", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const res = await w.fetch(url(`/api/rooms/${id}`));
  assert.equal(await res.text(), "{}\n");
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}`)));
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/`)));
  for (const method of ["POST", "PUT", "DELETE", "HEAD"]) {
    const res = await w.fetch(url(`/api/rooms/${id}`), { method });
    assert.equal(res.status, 405, method);
    assert.equal(res.headers.get("Allow"), "GET");
  }
});

test("a malformed room id is a 400 on every room path", async () => {
  const w = world();
  w.app();
  for (const path of ["/api/rooms/nope", "/api/rooms/nope/key", "/api/rooms/nope/ws", "/api/rooms/nope/push/test",
    "/api/rooms/11111111-1111-3111-8111-111111111111"]) {
    for (const method of ["GET", "PUT", "POST", "DELETE"]) {
      await goError(await w.fetch(url(path), { method }), 400, "malformed room id\n\n");
    }
  }
  assert.deepEqual(w.calls, []);
});

test("a value reads back verbatim, labeled JSON only when it parses", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  await put(w, id, "j", '{"a":1}');
  await put(w, id, "t", "<script>");
  await put(w, id, "b", new Uint8Array([0, 0xff, 1]));
  const get = (key) => w.fetch(url(`/api/rooms/${id}/${key}`));
  let res = await get("j");
  assert.equal(res.headers.get("Content-Type"), "application/json; charset=utf-8");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal(await res.text(), '{"a":1}');
  res = await get("t");
  assert.equal(res.headers.get("Content-Type"), "application/octet-stream");
  assert.equal(await res.text(), "<script>");
  res = await get("b");
  assert.deepEqual(new Uint8Array(await res.arrayBuffer()), new Uint8Array([0, 0xff, 1]));
  await notFound(await get("missing"));
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/j`)));
});

test("keys come from the decoded path, bytes and all", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  assert.equal((await put(w, id, "a%2Fb", "1")).status, 204);
  assert.equal(await (await w.fetch(url(`/api/rooms/${id}/a/b`))).text(), "1");
  // A key is the text of its UTF-8 bytes; one whose bytes are not UTF-8 is
  // refused on every verb before any cell call.
  assert.equal((await put(w, id, "caf%C3%A9%E2%82%AC", "2")).status, 204);
  assert.ok(Object.hasOwn(w.doc(APP, id).kv, "caf\u00e9\u20ac"));
  assert.equal(await (await w.fetch(url(`/api/rooms/${id}/caf%C3%A9%E2%82%AC`))).text(), "2");
  const before = w.calls.length;
  for (const key of ["k%FF", "k%E2%82", "%C0%80", "%ED%A0%80"]) {
    for (const method of ["GET", "PUT", "DELETE"]) {
      const init = method === "PUT" ? { method, body: "v" } : { method };
      await goError(await w.fetch(url(`/api/rooms/${id}/${key}`), init), 400, "invalid key\n\n");
    }
  }
  assert.equal(w.calls.length, before);
  assert.equal((await put(w, id, "x".repeat(256), "3")).status, 204);
  await goError(await put(w, id, "x".repeat(257), "3"), 400, "invalid key\n\n");
  await goError(await w.fetch(url(`/api/rooms/${id}/${"%E2%82%AC".repeat(86)}`)), 400, "invalid key\n\n");
  const res = await w.fetch(url(`/api/rooms/${id}/k`), { method: "POST" });
  assert.equal(res.status, 405);
  assert.equal(res.headers.get("Allow"), "GET, PUT, DELETE");
});

test("writes hit the per-room and per-app caps as hostthisd's do", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  assert.equal((await put(w, id, "k", "x".repeat(256 * 1024))).status, 204);
  await goError(await put(w, id, "k2", "y"), 413, "room is at its data cap\n\n");
  await goError(await put(w, id, "k", "x".repeat(256 * 1024 + 1)), 413, "room is at its data cap\n\n");
  // A body far past the cap is read only to one byte past it.
  await goError(await put(w, id, "k", new Uint8Array(4 * 1024 * 1024)), 413, "room is at its data cap\n\n");
  assert.equal(w.doc(APP, id).bytes, 256 * 1024);

  const id2 = await w.room();
  const kv = Object.fromEntries(Array.from({ length: 256 }, (_, n) => [`k${n}`, ""]));
  const doc = w.doc(APP, id2);
  w.storageOf("ROOMS", `${APP}|${id2}`).data.set("state", { ...doc, kv, wire: {} });
  await goError(await put(w, id2, "one-more", "v"), 413, "room is at its data cap\n\n");
  assert.equal((await put(w, id2, "k0", "")).status, 204);

  const id3 = await w.room();
  w.storageOf("PASTES", APP).data.set("roomAllocated", 64 * 1024 * 1024);
  await goError(await put(w, id3, "k", "v"), 507, "app room storage is at capacity\n\n");
  await goError(await w.fetch(url("/api/rooms"), { method: "POST" }), 507, "app room storage is at capacity\n\n");
});

test("a delete is idempotent and 404s only a missing room", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  await put(w, id, "k", "v");
  const del = (key, room = id) => w.fetch(url(`/api/rooms/${room}/${key}`), { method: "DELETE" });
  let res = await del("k");
  assert.equal(res.status, 204);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal((await del("k")).status, 204);
  assert.equal(w.doc(APP, id).seq, 3);
  await notFound(await del("k", MISSING_ROOM));
});

test("rooms are addressed by the host's app and the parsed id alone", async () => {
  const w = world();
  w.app(APP);
  w.app(OTHER);
  const a1 = await w.room(APP);
  const a2 = await w.room(APP);
  await put(w, a1, "k", "secret");
  // Another app holding the same id addresses a different room.
  await notFound(await w.fetch(url(`/api/rooms/${a1}/k`, OTHER)));
  await notFound(await w.fetch(url(`/api/rooms/${a1}`, OTHER)));
  await notFound(await put(w, a1, "k", "overwrite", OTHER));
  // Another room of the same app holds its own keys.
  await notFound(await w.fetch(url(`/api/rooms/${a2}/k`)));
  // The id is lowercased before it addresses anything.
  assert.equal(await (await w.fetch(url(`/api/rooms/${a1.toUpperCase()}/k`))).text(), "secret");
  for (const call of w.calls.filter((c) => c.ns === "ROOMS")) {
    assert.match(call.name, /^[a-z2-9]{8}\|[0-9a-f-]{36}$/);
  }
  assert.ok(w.calls.some((c) => c.name === `${OTHER}|${a1}`));
  assert.equal(w.doc(OTHER, a1), undefined);
});

test("the push key is the app's VAPID public key", async () => {
  const w = world();
  await notFound(await w.fetch(url("/api/push/key")));
  w.app();
  const res = await w.fetch(url("/api/push/key"));
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  const { key } = await res.json();
  assert.equal(Buffer.from(key, "base64url").length, 65);
  assert.equal((await (await w.fetch(url("/api/push/key"))).json()).key, key);
  const post = await w.fetch(url("/api/push/key"), { method: "POST" });
  assert.equal(post.status, 405);
  assert.equal(post.headers.get("Allow"), "GET");
  assert.equal(post.headers.get("Cache-Control"), "no-store");
  // Only the exact path is the key.
  await plainNotFound(await w.fetch(url("/api/push/key/")));
});

async function devicePoint() {
  const pair = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]);
  return Buffer.from(await crypto.subtle.exportKey("raw", pair.publicKey)).toString("base64url");
}

function subscription(n, p256dh) {
  return {
    endpoint: `https://fcm.googleapis.com/fcm/send/${n}`,
    keys: { p256dh, auth: Buffer.alloc(16, n).toString("base64url") },
  };
}

test("push subscriptions are added, listed and removed", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const subs = url(`/api/rooms/${id}/push/subscriptions`);
  const p256dh = await devicePoint();
  let res = await w.fetch(subs, { method: "PUT", body: JSON.stringify(subscription(1, p256dh)) });
  assert.equal(res.status, 204);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal(w.storageOf("ROOMS", `${APP}|${id}`).data.get("push").subject, "https://example.com");
  res = await w.fetch(subs);
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  const listed = await res.json();
  assert.deepEqual(listed.map((s) => s.endpoint), ["https://fcm.googleapis.com/fcm/send/1"]);
  assert.match(listed[0].added, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d*[1-9])?Z$/);
  res = await w.fetch(subs, { method: "DELETE", body: JSON.stringify({ endpoint: subscription(1).endpoint }) });
  assert.equal(res.status, 204);
  assert.equal(await (await w.fetch(subs)).text(), "[]\n");
  await goError(await w.fetch(subs, { method: "DELETE", body: "{}" }), 400, "push: invalid\n\n");
  res = await w.fetch(subs, { method: "POST" });
  assert.equal(res.status, 405);
  assert.equal(res.headers.get("Allow"), "GET, PUT, DELETE");
});

test("a subscription is validated before any cell sees it", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const subs = url(`/api/rooms/${id}/push/subscriptions`);
  const p256dh = await devicePoint();
  const before = w.calls.length;
  await goError(await w.fetch(subs, { method: "PUT", body: "{" }), 400, "malformed JSON body\n\n");
  await goError(await w.fetch(subs, { method: "PUT", body: '{"endpoint":5}' }), 400, "malformed JSON body\n\n");
  await goError(await w.fetch(subs, { method: "PUT", body: "" }), 400, "malformed JSON body\n\n");
  await goError(await w.fetch(subs, { method: "PUT", body: JSON.stringify({ ...subscription(1, p256dh), endpoint: "https://evil.com/x" }) }),
    400, "push: invalid: endpoint host is not a known push service\n\n");
  // Only the existence check ran.
  assert.deepEqual(w.calls.slice(before).map((c) => c.path), Array(4).fill("/paste/exists"));
  // Go's decoder: trailing data is ignored and field names fold.
  const folded = `{"ENDPOINT":"${subscription(2).endpoint}","Keys":{"P256DH":"${p256dh}","AUTH":"${subscription(2).keys.auth}"}} trailing`;
  assert.equal((await w.fetch(subs, { method: "PUT", body: folded })).status, 204);
});

test("the 17th subscription is refused 413", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const subs = url(`/api/rooms/${id}/push/subscriptions`);
  const p256dh = await devicePoint();
  for (let n = 0; n < 16; n++) {
    assert.equal((await w.fetch(subs, { method: "PUT", body: JSON.stringify(subscription(n, p256dh)) })).status, 204);
  }
  await goError(await w.fetch(subs, { method: "PUT", body: JSON.stringify(subscription(16, p256dh)) }),
    413, "room is at its push subscription cap\n\n");
});

test("a schedule round-trips in Go's shape", async () => {
  const w = world({ HOSTTHIS_PUBLIC_SCHEME: "http" });
  w.app();
  const id = await w.room();
  const sched = url(`/api/rooms/${id}/push/schedule`);
  let res = await w.fetch(sched);
  assert.equal(await res.text(), '{"tz":"","items":[]}\n');
  const doc = {
    tz: "America/New_York",
    items: [
      { id: "morning", at: "08:00", days: [1, 2], title: "Rota", bodyKey: "push:{date}", url: "", tag: "t", extra: 1 },
      { id: "once", when: "2099-09-12T08:00:00-04:00", title: "Rota", body: "Deep clean" },
    ],
  };
  res = await w.fetch(sched, { method: "PUT", body: JSON.stringify(doc) });
  assert.equal(res.status, 204);
  assert.equal(w.storageOf("ROOMS", `${APP}|${id}`).data.get("push").subject, "http://example.com");
  res = await w.fetch(sched);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await res.json(), {
    tz: "America/New_York",
    items: [
      { id: "morning", at: "08:00", days: [1, 2], title: "Rota", bodyKey: "push:{date}", tag: "t" },
      { id: "once", when: "2099-09-12T08:00:00-04:00", title: "Rota", body: "Deep clean" },
    ],
  });
  await goError(await w.fetch(sched, { method: "PUT", body: JSON.stringify({ tz: "UTC", items: [{ id: "a", title: "" }] }) }),
    400, "push: invalid: title must be 1 to 64 bytes (item 0)\n\n");
  await goError(await w.fetch(sched, { method: "PUT", body: '{"tz":"UTC","items":[{"id":"a","days":[1.0]}]}' }),
    400, "malformed JSON body\n\n");
  res = await w.fetch(sched, { method: "DELETE" });
  assert.equal(res.status, 405);
  assert.equal(res.headers.get("Allow"), "GET, PUT");
});

test("a push write the Room cell refuses is a 400 naming its rule, and the cell alone judges a zone", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const sched = url(`/api/rooms/${id}/push/schedule`);
  const comma = { tz: "UTC", items: [{ id: "x", when: "2099-09-12T08:00:00,5Z", title: "t", body: "b" }] };
  for (const [body, want] of [
    [{ tz: "Mars/Olympus", items: [] }, "push: invalid: invalid-tz\n\n"],
    [{ tz: "Local", items: [] }, "push: invalid: invalid-tz\n\n"],
    [comma, "push: invalid: invalid-when\n\n"],
  ]) {
    await goError(await w.fetch(sched, { method: "PUT", body: JSON.stringify(body) }), 400, want);
  }
  for (const tz of ["utc", "america/new_york"]) {
    assert.equal((await w.fetch(sched, { method: "PUT", body: JSON.stringify({ tz, items: [] }) })).status, 204, tz);
  }
  const sub = { ...subscription(1, await devicePoint()), endpoint: "https://fcm.googleapis.com:99999/fcm/send/1" };
  await goError(await w.fetch(url(`/api/rooms/${id}/push/subscriptions`), { method: "PUT", body: JSON.stringify(sub) }),
    400, "push: invalid: invalid-endpoint\n\n");
});

test("a test send runs inline and is limited to one a minute", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  const p256dh = await devicePoint();
  await w.fetch(url(`/api/rooms/${id}/push/subscriptions`), { method: "PUT", body: JSON.stringify(subscription(1, p256dh)) });
  const send = () => w.fetch(url(`/api/rooms/${id}/push/test`), { method: "POST" });
  let res = await send();
  assert.equal(res.status, 202);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await res.json(), { sent: 1, pruned: 0 });
  assert.deepEqual(w.sends, ["https://fcm.googleapis.com/fcm/send/1"]);
  res = await send();
  assert.equal(res.status, 429);
  assert.ok(Number(res.headers.get("Retry-After")) > 0 && Number(res.headers.get("Retry-After")) <= 60);
  await goError(res, 429, "push test rate limit reached\n\n");
  res = await w.fetch(url(`/api/rooms/${id}/push/test`));
  assert.equal(res.status, 405);
  assert.equal(res.headers.get("Allow"), "POST");
});

test("push routes 404 an unknown app first, then refuse other push paths", async () => {
  const w = world();
  for (const sub of ["", "/", "/subscriptions", "/nope", "/test/x"]) {
    await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/push${sub}`), { method: "POST" }));
  }
  w.app();
  for (const sub of ["", "/", "/nope", "/test/x", "/subscriptions/x"]) {
    await goError(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/push${sub}`), { method: "GET" }), 400, "reserved key\n\n");
  }
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/push/subscriptions`)));
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/push/schedule`)));
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/push/test`), { method: "POST" }));
});

// A complete browser handshake.
const WS = {
  Upgrade: "websocket",
  Connection: "Upgrade",
  "Sec-WebSocket-Version": "13",
  "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
};

test("the socket route checks app, room, upgrade and origin before the join", async () => {
  const w = world();
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/ws`), { headers: WS }));
  w.app();
  const id = await w.room();
  await notFound(await w.fetch(url(`/api/rooms/${MISSING_ROOM}/ws`), { headers: WS }));
  let res = await w.fetch(url(`/api/rooms/${id}/ws`));
  assert.equal(res.status, 426);
  assert.equal(res.headers.get("Upgrade"), "websocket");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  res = await w.fetch(url(`/api/rooms/${id}/ws`), { method: "POST", headers: WS });
  assert.equal(res.status, 405);
  for (const origin of ["https://evil.com", "https://example.com.evil.com", "null"]) {
    res = await w.fetch(url(`/api/rooms/${id}/ws`), { headers: { ...WS, Origin: origin } });
    assert.equal(res.status, 403, origin);
    assert.equal(res.headers.get("Cache-Control"), "no-store");
  }
  assert.deepEqual(w.joins, []);
});

test("the join is a new request built from the slug and the id alone", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  for (const origin of [undefined, `https://${APP}.${APEX}`, `https://zzzzzzzz.${APEX}`, `https://${APEX}`]) {
    const headers = { ...WS, Cookie: "c=1", "Sec-WebSocket-Protocol": "x", "X-Op": "put" };
    if (origin) headers.Origin = origin;
    const res = await w.fetch(url(`/api/rooms/${id.toUpperCase()}/ws?op=put&key=k`), { headers });
    await goError(res, 429, "room connection limit reached\n\n");
  }
  assert.equal(w.joins.length, 4);
  for (const { name, request } of w.joins) {
    assert.equal(name, `${APP}|${id}`);
    assert.equal(request.url, `https://cell/room/join?room=${APP}%7C${id}`);
    assert.equal(request.method, "GET");
    assert.deepEqual([...request.headers], [["upgrade", "websocket"]]);
    assert.equal(request.body, null);
  }
});

// Every op a cell answers, read from the op tables.
function cellOps() {
  const src = fileURLToPath(new URL("../src", import.meta.url));
  const text = readdirSync(src, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".js"))
    .map((entry) => readFileSync(join(entry.parentPath, entry.name), "utf8"))
    .join("\n");
  const ops = new Set();
  for (const table of text.matchAll(/^const (?:[A-Z]+_OPS|CELL_ROUTES) = \{\n([\s\S]*?)\n\};/gm)) {
    for (const key of table[1].matchAll(/^ {2}(\w+):/gm)) ops.add(key[1]);
  }
  return [...ops];
}

// The query each cell request may carry, by op path.
const CELL_QUERY = {
  "/paste/roomcounts": ["now", "subnet", "window"],
  "/room/get": ["key"],
  "/room/join": ["room"],
};

test("no client query, header or body chooses a cell op", async () => {
  const ops = cellOps();
  assert.ok(ops.length >= 40 && ops.includes("put") && ops.includes("pushsign"), `op tables not read: ${ops}`);
  const id = "33333333-3333-4333-8333-333333333333";
  const seed = async () => {
    const w = world();
    w.app();
    w.joinAnswer = () => new Response(null, { status: 200 });
    w.storageOf("ROOMS", `${APP}|${id}`).data.set("state", {
      meta: { appSlug: APP, id, createdAt: 1, updatedAt: 1 }, kv: {}, wire: {}, bytes: 0, seq: 0, budgetVersion: 0,
    });
    await put(w, id, "k", "v");
    w.calls.length = 0;
    return w;
  };
  const paths = (id) => [
    "/api/rooms", "/api/push/key", `/api/rooms/${id}`, `/api/rooms/${id}/k`, `/api/rooms/${id}/ws`,
    `/api/rooms/${id}/push/subscriptions`, `/api/rooms/${id}/push/schedule`, `/api/rooms/${id}/push/test`,
    `/api/rooms/${MISSING_ROOM}/k`, `/api/rooms/${id}/__cell`,
  ];
  const run = async (w, path, method, query) => {
    const init = { method, headers: { ...WS, "X-Op": query } };
    if (method === "PUT" || method === "POST") init.body = JSON.stringify({ op: query, key: "other", tz: "UTC", items: [] });
    await w.fetch(url(path + (query ? `?op=${query}&room=x&slug=y&key=other` : "")), init);
    return w.calls.splice(0).map((c) => `${c.by} ${c.ns}${c.path}?${c.query.join("&")}`);
  };
  for (const method of ["GET", "PUT", "POST", "DELETE"]) {
    const plain = await seed();
    const want = [];
    for (const path of paths(id)) want.push(await run(plain, path, method, ""));
    for (const op of ops) {
      const w = await seed();
      const got = [];
      for (const path of paths(id)) got.push(await run(w, path, method, op));
      assert.deepEqual(got, want, `${method} ?op=${op}`);
    }
    for (const call of want.flat().filter((c) => c.startsWith("router "))) {
      const [path, query] = call.replace(/^router [A-Z]+/, "").split("?");
      assert.deepEqual(query ? query.split("&") : [], CELL_QUERY[path] ?? [], call);
    }
  }
});

test("path mode serves the same routes under /p/<slug>/ on the apex", async () => {
  const w = world({ HOSTTHIS_URL_MODE: "path" });
  w.app();
  let res = await w.fetch(`http://${APEX}/p/${APP}/api/rooms`, { method: "POST" });
  assert.equal(res.status, 201);
  const { id } = await res.json();
  await put(w, id, "k", "v");
  assert.equal(await (await w.fetch(`http://${APEX}/p/${APP}/api/rooms/${id}/k`)).text(), "v");
  await plainNotFound(await w.fetch(`http://${APEX}/p/nope/api/rooms`, { method: "POST" }));
  await plainNotFound(await w.fetch(`http://${APEX}/api/rooms`, { method: "POST" }));
  const sub = world();
  sub.app();
  await plainNotFound(await sub.fetch(`http://${APEX}/p/${APP}/api/rooms`, { method: "POST" }));
  assert.deepEqual(sub.calls, []);
});

test("an unclean path redirects and a malformed escape is a 400, before any cell", async () => {
  const w = world();
  w.app();
  let res = await w.fetch(url("/api//rooms?x=1"), { method: "POST" });
  assert.equal(res.status, 307);
  assert.equal(res.headers.get("Location"), "/api/rooms?x=1");
  res = await w.fetch(url("/api/rooms/%zz"));
  assert.equal(res.status, 400);
  assert.equal(await res.text(), "400 Bad Request");
  assert.deepEqual(w.calls, []);
});

test("a host that is not an app serves nothing", async () => {
  const w = world();
  w.app();
  for (const host of [APEX, `www.${APEX}`, `x.${APP}.${APEX}`, `abc12345.${APEX}`]) {
    await w.fetch(`http://${host}/api/rooms`, { method: "POST" }).then(async (res) => {
      assert.equal(res.status, 404, host);
      assert.equal(await res.text(), "404 page not found\n");
    });
  }
  assert.deepEqual(w.calls, []);
});

test("off mode leaves every rooms path on the internal router", async () => {
  for (const apex of [undefined, ""]) {
    const w = world({ HOSTTHIS_APEX_DOMAIN: apex });
    w.app();
    for (const path of ["/api/rooms", "/api/push/key", `/api/rooms/${MISSING_ROOM}/ws`]) {
      const res = await w.fetch(url(path), { method: "POST", headers: WS });
      assert.equal(res.status, 404, path);
      assert.equal(await res.text(), "not found\n");
    }
    assert.deepEqual(w.calls, []);
  }
});

test("a forwarded request is served for the forwarded host", async () => {
  const w = world({ HOSTTHIS_CELLD_TOKEN: TOKEN });
  w.app();
  const auth = { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${APP}.${APEX}` };
  const res = await w.fetch("http://celld.internal/api/rooms", { method: "POST", headers: auth });
  assert.equal(res.status, 201);
  const { id } = await res.json();
  // The forwarded host is the request's own host for the origin rule.
  const forwardedLocal = world({ HOSTTHIS_CELLD_TOKEN: TOKEN, HOSTTHIS_APEX_DOMAIN: "localhost:8787" });
  forwardedLocal.app();
  const roomRes = await forwardedLocal.fetch("http://celld.internal/api/rooms", {
    method: "POST", headers: { ...auth, [FORWARDED]: `${APP}.localhost:8787` },
  });
  const local = (await roomRes.json()).id;
  const join = await forwardedLocal.fetch(`http://celld.internal/api/rooms/${local}/ws`, {
    headers: { ...auth, [FORWARDED]: `${APP}.localhost:8787`, ...WS, Origin: `http://${APP}.localhost:8787` },
  });
  assert.equal(join.status, 429);
  assert.equal(forwardedLocal.joins.length, 1);
  assert.match(id, UUID_V4);
});

test("an unknown room is 404 and a cell fault is 500, never a 101", async () => {
  const w = world();
  w.app();
  const id = await w.room();
  w.joinAnswer = () => new Response("expected websocket\n", { status: 426 });
  await goError(await w.fetch(url(`/api/rooms/${id}/ws`), { headers: WS }), 500, "internal error\n\n");
  w.joinAnswer = () => {
    throw new Error("cell unavailable");
  };
  await goError(await w.fetch(url(`/api/rooms/${id}/ws`), { headers: WS }), 500, "internal error\n\n");
});
