import assert from "node:assert/strict";
import test from "node:test";

import {
  Paste, Room, encryptPush, signVapid, localDate, nextDue, zonedToUTC,
} from "../src/index.js";

const ROOM_ID = "11111111-1111-4111-8111-111111111111";
const HOUR = 3600 * 1000;

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

class FakeStorage {
  constructor(seed = new Map(), alarm = null) {
    this.data = new Map([...seed].map(([key, value]) => [key, clone(value)]));
    this.alarm = alarm;
  }

  async get(key) {
    return clone(this.data.get(key));
  }

  async put(keyOrEntries, value) {
    const entries = typeof keyOrEntries === "string"
      ? [[keyOrEntries, value]]
      : keyOrEntries instanceof Map ? [...keyOrEntries] : Object.entries(keyOrEntries);
    for (const [key, entry] of entries) {
      this.data.set(String(key), clone(entry));
    }
  }

  async delete(keyOrKeys) {
    for (const key of Array.isArray(keyOrKeys) ? keyOrKeys : [keyOrKeys]) {
      this.data.delete(String(key));
    }
  }

  async getAlarm() {
    return this.alarm;
  }

  async setAlarm(at) {
    this.alarm = at;
  }

  async deleteAlarm() {
    this.alarm = null;
  }

  async transaction(callback) {
    const tx = new FakeStorage(this.data, this.alarm);
    const result = await callback(tx);
    this.data = tx.data;
    this.alarm = tx.alarm;
    return result;
  }
}

function state(storage) {
  return {
    storage,
    acceptWebSocket() {},
    getWebSockets() { return []; },
    blockConcurrencyWhile(callback) { return callback(); },
  };
}

function b64u(bytes) {
  return Buffer.from(bytes).toString("base64url");
}

function unb64u(text) {
  return new Uint8Array(Buffer.from(text, "base64url"));
}

function utf8(text) {
  return new TextEncoder().encode(text);
}

// RFC 8291 Appendix A.
const RFC = {
  plaintext: "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24",
  asPublic: "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8",
  asPrivate: "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw",
  uaPublic: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
  salt: "DGv6ra1nlYgDCS1FRnbzlw",
  auth: "BTBZMqHH6r4Tts7J_aSIgg",
  output: "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
    "mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
    "pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN",
};

async function ecdhPair(publicB64u, privateB64u) {
  const point = unb64u(publicB64u);
  const jwk = { kty: "EC", crv: "P-256", x: b64u(point.slice(1, 33)), y: b64u(point.slice(33)) };
  const params = { name: "ECDH", namedCurve: "P-256" };
  return {
    publicKey: await crypto.subtle.importKey("jwk", jwk, params, true, []),
    privateKey: await crypto.subtle.importKey("jwk", { ...jwk, d: privateB64u }, params, false, ["deriveBits"]),
  };
}

test("encryptPush reproduces the RFC 8291 test vector", async () => {
  const out = await encryptPush(
    { p256dh: RFC.uaPublic, auth: RFC.auth },
    unb64u(RFC.plaintext),
    { serverKeys: await ecdhPair(RFC.asPublic, RFC.asPrivate), salt: unb64u(RFC.salt) },
  );
  assert.equal(b64u(out), RFC.output);
  // 86-byte header, 41-byte plaintext, delimiter, 16-byte tag.
  assert.equal(out.length, 144);
});

test("encryptPush generates a fresh salt and key pair per message", async () => {
  const sub = { p256dh: RFC.uaPublic, auth: RFC.auth };
  const a = await encryptPush(sub, utf8("hi"));
  const b = await encryptPush(sub, utf8("hi"));
  assert.notDeepEqual(b64u(a.slice(0, 16)), b64u(b.slice(0, 16)));
  assert.notDeepEqual(b64u(a.slice(21, 86)), b64u(b.slice(21, 86)));
  assert.equal(a.length, 86 + 2 + 1 + 16);
});

function decodeJWT(token) {
  const [header, payload, signature] = token.split(".");
  const json = (part) => JSON.parse(new TextDecoder().decode(unb64u(part)));
  return { header: json(header), payload: json(payload), signature: unb64u(signature), signed: `${header}.${payload}` };
}

test("signVapid produces an ES256 JWT that verifies against the public key", async () => {
  const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const token = await signVapid(pair.privateKey, { aud: "https://push.example", exp: 1700000000, sub: "https://apex.example" });
  const jwt = decodeJWT(token);
  assert.deepStrictEqual(jwt.header, { typ: "JWT", alg: "ES256" });
  assert.deepStrictEqual(jwt.payload, { aud: "https://push.example", exp: 1700000000, sub: "https://apex.example" });
  assert.equal(jwt.signature.length, 64);
  assert.ok(await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" }, pair.publicKey, jwt.signature, utf8(jwt.signed),
  ));
});

test("Paste pushkey generates one VAPID pair and pushsign never leaks the private key", async () => {
  const storage = new FakeStorage();
  const paste = new Paste(state(storage));
  const first = await (await paste.fetch(new Request("https://cell/paste/pushkey?slug=app", { method: "POST" }))).json();
  const second = await (await paste.fetch(new Request("https://cell/paste/pushkey?slug=app", { method: "POST" }))).json();
  assert.equal(first.key, second.key);
  const point = unb64u(first.key);
  assert.equal(point.length, 65);
  assert.equal(point[0], 4);

  const now = 1_800_000_000_000;
  const res = await paste.fetch(new Request("https://cell/paste/pushsign?slug=app", {
    method: "POST",
    body: JSON.stringify({ aud: "https://push.example", sub: "https://apex.example", now }),
  }));
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.deepStrictEqual(Object.keys(body).sort(), ["key", "token"]);
  assert.equal(body.key, first.key);
  const jwt = decodeJWT(body.token);
  assert.equal(jwt.payload.aud, "https://push.example");
  assert.equal(jwt.payload.sub, "https://apex.example");
  assert.equal(jwt.payload.exp, Math.floor((now + 12 * HOUR) / 1000));
  const jwk = { kty: "EC", crv: "P-256", x: b64u(point.slice(1, 33)), y: b64u(point.slice(33)) };
  const publicKey = await crypto.subtle.importKey("jwk", jwk, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
  assert.ok(await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, publicKey, jwt.signature, utf8(jwt.signed)));
  assert.equal(JSON.stringify(body).includes(storage.data.get("pushVapid").privateJwk.d), false);

  for (const aud of ["push.example", "http://push.example", "https://push.example/path", 7]) {
    const bad = await paste.fetch(new Request("https://cell/paste/pushsign?slug=app", {
      method: "POST", body: JSON.stringify({ aud }),
    }));
    assert.equal(bad.status, 400, String(aud));
  }
});

function roomDoc(kv = {}) {
  return {
    meta: { appSlug: "appslug1", id: ROOM_ID, createdAt: 1, updatedAt: 1 },
    kv, wire: {}, bytes: 0, seq: 0, budgetVersion: 0,
  };
}

function subscription(n = 1, overrides = {}) {
  return {
    endpoint: `https://fcm.googleapis.com/fcm/send/${n}`,
    keys: { p256dh: RFC.uaPublic, auth: RFC.auth },
    ...overrides,
  };
}

function harness({ room = roomDoc(), push, service = {}, env: extra = {} } = {}) {
  const seed = new Map();
  if (room) {
    seed.set("state", room);
  }
  if (push) {
    seed.set("push", push);
  }
  const roomStorage = new FakeStorage(seed);
  const pasteStorage = new FakeStorage(new Map([["roomAllocated", 0]]));
  const paste = new Paste(state(pasteStorage));
  const transport = { failBefore: 0, calls: [] };
  const sends = [];
  const env = {
    PASTES: {
      idFromName(name) { return name; },
      get() {
        return {
          async fetch(request) {
            transport.calls.push(new URL(request.url).pathname);
            if (transport.failBefore-- > 0) {
              throw new Error("coordinator unavailable");
            }
            return paste.fetch(request);
          },
        };
      },
    },
    PUSH_FETCH: async (url, init) => {
      sends.push({ url, init });
      if (service.fetch) {
        return service.fetch(url, init);
      }
      const status = service.status?.(url) ?? 201;
      return new Response(null, { status });
    },
    ...extra,
  };
  return {
    roomStorage, pasteStorage, transport, sends,
    room: () => new Room(state(roomStorage), env),
    push: () => roomStorage.data.get("push"),
    call: async (op, body) => {
      const res = await new Room(state(roomStorage), env).fetch(new Request(`https://cell/room/${op}?room=x`, {
        method: "POST", body: JSON.stringify(body ?? {}),
      }));
      const text = await res.text();
      const json = res.headers.get("content-type")?.includes("json");
      return { status: res.status, body: json ? JSON.parse(text) : text };
    },
  };
}

test("Room pushsubput validates, dedupes by endpoint, and caps at 16", async () => {
  const h = harness();
  assert.equal((await h.call("pushsubput", { ...subscription(1), now: 5000 })).status, 204);
  assert.equal((await h.call("pushsubput", { ...subscription(1), now: 9000 })).status, 204);
  let list = await h.call("pushsublist");
  assert.equal(list.status, 200);
  assert.deepStrictEqual(list.body.subscriptions, [
    { endpoint: "https://fcm.googleapis.com/fcm/send/1", added: new Date(5000).toISOString() },
  ]);

  const bad = [
    ["http endpoint", subscription(2, { endpoint: "http://push.example/x" })],
    ["not a url", subscription(2, { endpoint: "push.example" })],
    ["missing keys", { endpoint: "https://push.example/2" }],
    ["short p256dh", subscription(2, { keys: { p256dh: b64u(new Uint8Array(64)), auth: RFC.auth } })],
    ["compressed point", subscription(2, { keys: { p256dh: b64u(new Uint8Array(65)), auth: RFC.auth } })],
    ["short auth", subscription(2, { keys: { p256dh: RFC.uaPublic, auth: b64u(new Uint8Array(15)) } })],
    ["bad base64", subscription(2, { keys: { p256dh: "!!!", auth: RFC.auth } })],
    ["unknown host", subscription(2, { endpoint: "https://push.example/send/2" })],
    ["suffix spoof", subscription(2, { endpoint: "https://fcm.googleapis.com.evil.example/send/2" })],
    ["prefix spoof", subscription(2, { endpoint: "https://notfcm.googleapis.com/send/2" })],
    ["apple apex only", subscription(2, { endpoint: "https://apple.com/send/2" })],
  ];
  for (const [name, body] of bad) {
    assert.equal((await h.call("pushsubput", body)).status, 400, name);
  }
  const long = subscription(2, { endpoint: `https://fcm.googleapis.com/${"x".repeat(1024)}` });
  assert.deepStrictEqual(await h.call("pushsubput", long), { status: 400, body: { error: "invalid-endpoint" } });
  for (const host of ["web.push.apple.com", "updates.push.services.mozilla.com", "db5p.notify.windows.com"]) {
    assert.equal((await h.call("pushsubput", subscription(2, { endpoint: `https://${host}/send/2` }))).status, 204, host);
    assert.equal((await h.call("pushsubdel", { endpoint: `https://${host}/send/2` })).status, 204);
  }

  for (let n = 2; n <= 16; n++) {
    assert.equal((await h.call("pushsubput", subscription(n))).status, 204);
  }
  assert.equal((await h.call("pushsubput", subscription(17))).status, 413);
  assert.equal((await h.call("pushsubput", subscription(3))).status, 204, "refresh at cap");
  list = await h.call("pushsublist");
  assert.equal(list.body.subscriptions.length, 16);

  assert.equal((await h.call("pushsubdel", { endpoint: "https://fcm.googleapis.com/fcm/send/3" })).status, 204);
  assert.equal((await h.call("pushsubdel", { endpoint: "https://fcm.googleapis.com/fcm/send/3" })).status, 204);
  assert.equal((await h.call("pushsubdel", {})).status, 400);
  list = await h.call("pushsublist");
  assert.equal(list.body.subscriptions.length, 15);
  assert.equal(h.roomStorage.data.get("state").kv.push, undefined);
});

test("Room push ops on a missing room are 404", async () => {
  const h = harness({ room: null });
  for (const op of ["pushsubput", "pushsubdel", "pushsublist", "pushscheduleput", "pushscheduleget", "pushtest"]) {
    assert.equal((await h.call(op, subscription())).status, 404, op);
  }
});

function schedule(items, tz = "America/New_York") {
  return { tz, items };
}

function recurring(overrides = {}) {
  return { id: "morning", at: "08:00", days: [1, 2, 3], title: "Rota", body: "Deep clean", ...overrides };
}

test("Room pushscheduleput validates the whole document or nothing", async () => {
  const h = harness();
  assert.deepStrictEqual(await h.call("pushscheduleget"), { status: 200, body: { tz: "", items: [] } });

  const ok = schedule([
    recurring(),
    { id: "once", when: "2026-09-12T08:00:00-04:00", title: "Rota", bodyKey: "push:{date}", url: "/?room=x#/", tag: "rota" },
  ]);
  assert.equal((await h.call("pushscheduleput", { ...ok, now: 0 })).status, 204);
  const got = await h.call("pushscheduleget");
  assert.deepStrictEqual(got.body, ok);
  const wide = "\u00e9";
  assert.equal(utf8(wide).length, 2);

  const cases = [
    ["unknown tz", schedule([recurring()], "Mars/Olympus")],
    ["missing tz", { items: [recurring()] }],
    ["missing tz with empty items", { items: [] }],
    ["items not array", schedule({})],
    ["bad at", schedule([recurring({ at: "8:00" })])],
    ["bad at hour", schedule([recurring({ at: "24:00" })])],
    ["empty days", schedule([recurring({ days: [] })])],
    ["day out of range", schedule([recurring({ days: [7] })])],
    ["duplicate day", schedule([recurring({ days: [1, 1] })])],
    ["at and when", schedule([recurring({ when: "2026-09-12T08:00:00Z" })])],
    ["neither at nor when", schedule([recurring({ at: undefined, days: undefined })])],
    ["when without offset", schedule([{ id: "o", when: "2026-09-12T08:00:00", title: "t", body: "b" }])],
    ["when garbage", schedule([{ id: "o", when: "2026-13-45T08:00:00Z", title: "t", body: "b" }])],
    ["bad id char", schedule([recurring({ id: "a b" })])],
    ["empty id", schedule([recurring({ id: "" })])],
    ["long id", schedule([recurring({ id: "a".repeat(33) })])],
    ["duplicate id", schedule([recurring(), recurring({ id: "morning", at: "09:00" })])],
    ["missing title", schedule([recurring({ title: undefined })])],
    ["long title", schedule([recurring({ title: "t".repeat(65) })])],
    ["title over 64 bytes", schedule([recurring({ title: wide.repeat(33) })])],
    ["long url", schedule([recurring({ url: "/".repeat(513) })])],
    ["url over 512 bytes", schedule([recurring({ url: wide.repeat(257) })])],
    ["long tag", schedule([recurring({ tag: "t".repeat(65) })])],
    ["tag over 64 bytes", schedule([recurring({ tag: wide.repeat(33) })])],
    ["body over 1024 bytes", schedule([recurring({ body: wide.repeat(513) })])],
    ["reserved ws bodyKey", schedule([recurring({ body: undefined, bodyKey: "ws" })])],
    ["body and bodyKey", schedule([recurring({ bodyKey: "k" })])],
    ["no body", schedule([recurring({ body: undefined })])],
    ["long body", schedule([recurring({ body: "b".repeat(1025) })])],
    ["empty bodyKey", schedule([recurring({ body: undefined, bodyKey: "" })])],
    ["long bodyKey", schedule([recurring({ body: undefined, bodyKey: "k".repeat(257) })])],
    ["reserved bodyKey", schedule([recurring({ body: undefined, bodyKey: "push/x" })])],
    ["reserved bare bodyKey", schedule([recurring({ body: undefined, bodyKey: "push" })])],
    ["second item invalid", schedule([recurring(), recurring({ id: "two", at: "99:00" })])],
  ];
  for (const [name, body] of cases) {
    assert.equal((await h.call("pushscheduleput", { ...body, now: 0 })).status, 400, name);
    assert.deepStrictEqual((await h.call("pushscheduleget")).body, ok, `${name} applied partially`);
  }
  const past = { id: "o", when: "2026-09-12T08:00:00-04:00", title: "t", body: "b" };
  const late = await h.call("pushscheduleput", { ...schedule([past]), now: Date.parse("2026-09-12T12:00:00Z") });
  assert.deepStrictEqual(late, { status: 400, body: { error: "invalid-when" } });
  assert.equal((await h.call("pushscheduleput", { ...schedule([past]), now: Date.parse("2026-09-12T11:59:59Z") })).status, 204);
  assert.equal((await h.call("pushscheduleput", { ...schedule([{ ...past, title: wide.repeat(32) }]), now: 0 })).status, 204);
  const many = schedule(Array.from({ length: 17 }, (_, i) => recurring({ id: `i${i}` })));
  assert.equal((await h.call("pushscheduleput", { ...many, now: 0 })).status, 413);

  assert.equal((await h.call("pushscheduleput", schedule([]))).status, 204);
  assert.deepStrictEqual((await h.call("pushscheduleget")).body, { tz: "America/New_York", items: [] });
  assert.equal(h.roomStorage.alarm, null);
  assert.equal((await h.call("pushscheduleput", { items: [] })).status, 400);
});

test("next due crosses DST in America/New_York", () => {
  const tz = "America/New_York";
  const everyday = { at: "08:00", days: [0, 1, 2, 3, 4, 5, 6] };
  // 2026-03-07 08:00 EST is 13:00Z; the next morning is EDT, 12:00Z.
  assert.equal(nextDue(everyday, tz, Date.parse("2026-03-07T13:00:00Z")), Date.parse("2026-03-08T12:00:00Z"));
  // 2026-10-31 08:00 EDT is 12:00Z; the next morning is EST, 13:00Z.
  assert.equal(nextDue(everyday, tz, Date.parse("2026-10-31T12:00:00Z")), Date.parse("2026-11-01T13:00:00Z"));
  // Same wall clock, one second earlier, is still today.
  assert.equal(nextDue(everyday, tz, Date.parse("2026-03-07T12:59:59Z")), Date.parse("2026-03-07T13:00:00Z"));
  // Weekday filter: from Saturday 2026-09-05, Monday only.
  assert.equal(nextDue({ at: "08:00", days: [1] }, tz, Date.parse("2026-09-05T15:00:00Z")),
    Date.parse("2026-09-07T12:00:00Z"));
  // Only today's weekday and the time already passed: a full week ahead.
  assert.equal(nextDue({ at: "08:00", days: [6] }, tz, Date.parse("2026-09-05T15:00:00Z")),
    Date.parse("2026-09-12T12:00:00Z"));
  // A skipped wall clock resolves forward; a repeated one to its first occurrence.
  assert.equal(zonedToUTC(2026, 3, 8, 2, 30, tz), Date.parse("2026-03-08T07:30:00Z"));
  assert.equal(zonedToUTC(2026, 11, 1, 1, 30, tz), Date.parse("2026-11-01T05:30:00Z"));
  assert.equal(nextDue({ when: "2026-09-12T08:00:00-04:00" }, tz, 0), Date.parse("2026-09-12T12:00:00Z"));
  assert.equal(localDate(Date.parse("2026-09-06T03:30:00Z"), tz), "2026-09-05");
  assert.equal(localDate(Date.parse("2026-09-06T03:30:00Z"), "UTC"), "2026-09-06");
});

test("Room schedule arms the shared alarm at the earliest due instant", async () => {
  const h = harness();
  const now = Date.parse("2026-09-05T15:00:00Z");
  const body = schedule([
    recurring({ id: "mon", days: [1] }),
    recurring({ id: "sun", days: [0] }),
    { id: "once", when: "2026-09-20T08:00:00-04:00", title: "t", body: "b" },
  ]);
  assert.equal((await h.call("pushscheduleput", { ...body, now })).status, 204);
  assert.equal(h.roomStorage.alarm, Date.parse("2026-09-06T12:00:00Z"));
  assert.equal(h.push().nextFireAt, Date.parse("2026-09-06T12:00:00Z"));
});

function putBody(text) {
  return {
    key: "key", value: Buffer.from(text).toString("base64"), wire: JSON.stringify(text),
    roomCap: 100, keyCap: 10, appCap: 10, now: 9,
  };
}

test("Room budget completion does not disarm a pending push fire", async () => {
  const h = harness();
  const now = Date.parse("2026-09-05T15:00:00Z");
  const due = Date.parse("2026-09-06T12:00:00Z");
  assert.equal((await h.call("pushscheduleput", { ...schedule([recurring({ days: [0] })]), now })).status, 204);
  assert.equal(h.roomStorage.alarm, due);

  // Growth: the pending budget arms the alarm for now, and its completion clears pending.
  const res = await h.room().put(putBody("aaaaaa"));
  assert.equal(res.status, 200);
  assert.equal(h.roomStorage.data.get("state").pending, undefined);
  assert.equal(h.roomStorage.alarm, due, "clearPending disarmed the push timer");

  // Shrink: same, through the release path.
  assert.equal((await h.room().put(putBody("aa"))).status, 200);
  assert.equal(h.roomStorage.alarm, due);

  // Delete: same.
  assert.equal((await h.room().del({ key: "key", now: 9 })).status, 200);
  assert.equal(h.roomStorage.alarm, due);
});

test("Room push save does not disarm a pending budget retry", async () => {
  const h = harness();
  h.transport.failBefore = 1;
  const before = Date.now();
  assert.equal((await h.room().put(putBody("aaaaaa"))).status, 502);
  assert.ok(h.roomStorage.data.get("state").pending);
  const retryAt = h.roomStorage.alarm;
  assert.ok(retryAt >= before && retryAt <= Date.now() + 1000);

  const now = Date.parse("2026-09-05T15:00:00Z");
  assert.equal((await h.call("pushscheduleput", { ...schedule([recurring({ days: [0] })]), now })).status, 204);
  assert.equal(h.roomStorage.alarm, retryAt, "push save pushed the budget retry out");
  assert.equal((await h.call("pushsubput", subscription())).status, 204);
  assert.equal(h.roomStorage.alarm, retryAt);

  // The alarm resumes the budget, then leaves the push deadline armed.
  await h.room().alarm();
  assert.equal(h.roomStorage.data.get("state").pending, undefined);
  assert.equal(h.roomStorage.data.get("state").bytes, 6);
  assert.equal(h.roomStorage.alarm, Date.parse("2026-09-06T12:00:00Z"));
});

test("Room alarm retry keeps the earlier of budget retry and push due", async () => {
  const h = harness();
  const now = Date.parse("2026-09-05T15:00:00Z");
  assert.equal((await h.call("pushscheduleput", { ...schedule([recurring({ days: [0] })]), now })).status, 204);
  h.transport.failBefore = 1;
  assert.equal((await h.room().put(putBody("aaaaaa"))).status, 502);
  assert.ok(h.roomStorage.alarm <= Date.now() + 1000);
});

async function arrange(h, { now, items, subs = [subscription(1)], kv = {}, tz = "America/New_York" }) {
  const doc = roomDoc(kv);
  h.roomStorage.data.set("state", doc);
  for (const sub of subs) {
    assert.equal((await h.call("pushsubput", { ...sub, now })).status, 204);
  }
  assert.equal((await h.call("pushscheduleput", { ...schedule(items, tz), now })).status, 204);
}

test("Room alarm fires due items and sends encrypted Web Push", async () => {
  const h = harness();
  const put = Date.parse("2026-09-05T15:00:00Z");
  await arrange(h, {
    now: put,
    items: [recurring({ id: "sun", days: [0], url: "/x", tag: "rota" })],
  });
  const due = Date.parse("2026-09-06T12:00:00Z");
  await h.room().alarm({ now: due + 1000 });
  assert.equal(h.sends.length, 1);
  const [send] = h.sends;
  assert.equal(send.url, "https://fcm.googleapis.com/fcm/send/1");
  assert.equal(send.init.method, "POST");
  assert.equal(send.init.headers["Content-Encoding"], "aes128gcm");
  assert.equal(send.init.headers["Content-Type"], "application/octet-stream");
  assert.equal(send.init.headers.TTL, "86400");
  assert.equal(send.init.headers.Urgency, "normal");
  assert.equal(send.init.redirect, "manual");
  assert.ok(send.init.signal instanceof AbortSignal);
  const auth = send.init.headers.Authorization;
  assert.match(auth, /^vapid t=[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+, k=[A-Za-z0-9_-]+$/);
  const jwt = decodeJWT(auth.slice("vapid t=".length, auth.indexOf(",")));
  assert.equal(jwt.payload.aud, "https://fcm.googleapis.com");
  assert.equal(h.transport.calls.filter((c) => c.endsWith("/pushsign")).length, 1);
  // 86-byte header, JSON payload, delimiter, 16-byte tag.
  const payload = JSON.stringify({ title: "Rota", body: "Deep clean", url: "/x", tag: "rota" });
  assert.equal(send.init.body.length, 86 + utf8(payload).length + 1 + 16);
  // Recurring item recomputed a week out; counter recorded under the local date.
  const push = h.push();
  assert.equal(push.nextFireAt, Date.parse("2026-09-13T12:00:00Z"));
  assert.equal(h.roomStorage.alarm, push.nextFireAt);
  assert.deepStrictEqual(push.subscriptions[0].sent, { "2026-09-06": 1 });
});

test("Room alarm resolves bodyKey with the local date and skips a missing key", async () => {
  const h = harness();
  const put = Date.parse("2026-09-05T15:00:00Z");
  const value = Buffer.from("Kitchen: Ana").toString("base64");
  await arrange(h, {
    now: put,
    kv: { "push:2026-09-06": value },
    items: [
      recurring({ id: "a", days: [0], body: undefined, bodyKey: "push:{date}" }),
      recurring({ id: "b", days: [0], body: undefined, bodyKey: "absent" }),
    ],
  });
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:30Z") });
  assert.equal(h.sends.length, 1);
  const payload = JSON.stringify({ title: "Rota", body: "Kitchen: Ana" });
  assert.equal(h.sends[0].init.body.length, 86 + utf8(payload).length + 17);
});

test("Room alarm skips stale fires and removes one-shot items either way", async () => {
  const h = harness();
  const put = Date.parse("2026-09-05T15:00:00Z");
  await arrange(h, {
    now: put,
    items: [
      { id: "stale", when: "2026-09-06T08:00:00-04:00", title: "t", body: "old" },
      { id: "fresh", when: "2026-09-06T08:20:00-04:00", title: "t", body: "new" },
      { id: "later", when: "2026-09-07T08:00:00-04:00", title: "t", body: "later" },
      recurring({ id: "rec", days: [0], body: "rec" }),
    ],
  });
  await h.room().alarm({ now: Date.parse("2026-09-06T12:21:00Z") });
  assert.equal(h.sends.length, 1);
  const payload = JSON.stringify({ title: "t", body: "new" });
  assert.equal(h.sends[0].init.body.length, 86 + utf8(payload).length + 17);
  const got = (await h.call("pushscheduleget")).body;
  assert.deepStrictEqual(got.items.map((i) => i.id), ["later", "rec"]);
  assert.equal(h.push().nextFireAt, Date.parse("2026-09-07T12:00:00Z"));
  assert.equal(h.roomStorage.alarm, Date.parse("2026-09-07T12:00:00Z"));
});

test("Room alarm prunes a subscription the push service reports gone", async () => {
  const h = harness({ service: { status: (url) => (url.endsWith("/2") ? 410 : url.endsWith("/3") ? 404 : url.endsWith("/4") ? 500 : 201) } });
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    subs: [subscription(1), subscription(2), subscription(3), subscription(4)],
    items: [recurring({ id: "sun", days: [0] })],
  });
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 4);
  const list = (await h.call("pushsublist")).body.subscriptions.map((s) => s.endpoint);
  assert.deepStrictEqual(list, ["https://fcm.googleapis.com/fcm/send/1", "https://fcm.googleapis.com/fcm/send/4"]);
});

test("Room alarm skips a subscription past its daily cap and resets by local date", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    items: [recurring({ id: "sun", days: [0] })],
  });
  const push = h.push();
  push.subscriptions[0].sent = { "2026-09-06": 8 };
  h.roomStorage.data.set("push", push);
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 0);
  assert.equal(h.push().nextFireAt, Date.parse("2026-09-13T12:00:00Z"));

  const stale = h.push();
  stale.subscriptions[0].sent = { "2026-09-06": 8 };
  stale.schedule.items[0].due = Date.parse("2026-09-13T12:00:00Z");
  h.roomStorage.data.set("push", stale);
  await h.room().alarm({ now: Date.parse("2026-09-13T12:00:00Z") });
  assert.equal(h.sends.length, 1);
  assert.deepStrictEqual(h.push().subscriptions[0].sent, { "2026-09-13": 1 });
});

test("Room alarm skips a payload over 2 KiB", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    kv: { big: Buffer.from("b".repeat(2048)).toString("base64") },
    items: [recurring({ id: "sun", days: [0], body: undefined, bodyKey: "big" })],
  });
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 0);
});

test("Room alarm without a schedule does nothing and leaves no alarm", async () => {
  const h = harness();
  await h.room().alarm();
  assert.equal(h.sends.length, 0);
  assert.equal(h.roomStorage.alarm, null);
  assert.equal(h.push(), undefined);
});

test("Room alarm survives a failed VAPID signing and a throwing push service", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    items: [recurring({ id: "sun", days: [0] })],
  });
  h.transport.failBefore = 1;
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 0);
  assert.equal(h.push().nextFireAt, Date.parse("2026-09-13T12:00:00Z"));
  assert.equal((await h.call("pushsublist")).body.subscriptions.length, 1);
});

test("Room push keeps a gone answer for a subscription younger than a minute", async () => {
  const h = harness({ service: { status: () => 410 } });
  const now = Date.parse("2026-09-05T15:00:00Z");
  assert.equal((await h.call("pushsubput", { ...subscription(1), now })).status, 204);
  const fresh = await h.call("pushtest", { now: now + 5_000 });
  assert.deepStrictEqual(fresh, { status: 200, body: { sent: 0, pruned: 0 } });
  assert.equal(h.push().subscriptions.length, 1);
  assert.deepStrictEqual(h.push().subscriptions[0].sent, { "2026-09-05": 1 });

  const aged = await h.call("pushtest", { now: now + 70_000 });
  assert.deepStrictEqual(aged, { status: 200, body: { sent: 0, pruned: 1 } });
  assert.equal(h.push().subscriptions.length, 0);
});

test("Room pushtest sends inline, counts, prunes, and rate limits per minute", async () => {
  const h = harness({ service: { status: (url) => (url.endsWith("/2") ? 410 : 201) } });
  const now = Date.parse("2026-09-05T15:00:00Z");
  const added = now - 120_000;
  assert.equal((await h.call("pushsubput", { ...subscription(1), now: added })).status, 204);
  assert.equal((await h.call("pushsubput", { ...subscription(2), now: added })).status, 204);
  const first = await h.call("pushtest", { now });
  assert.deepStrictEqual(first, { status: 200, body: { sent: 1, pruned: 1 } });
  assert.equal(h.sends.length, 2);
  assert.deepStrictEqual(h.push().subscriptions.map((s) => s.endpoint), ["https://fcm.googleapis.com/fcm/send/1"]);
  assert.deepStrictEqual(h.push().subscriptions[0].sent, { "2026-09-05": 1 });

  const limited = await h.call("pushtest", { now: now + 20_000 });
  assert.equal(limited.status, 429);
  assert.equal(limited.body.retryAfter, 40);
  assert.equal(h.sends.length, 2);

  const again = await h.call("pushtest", { now: now + 60_000 });
  assert.deepStrictEqual(again, { status: 200, body: { sent: 1, pruned: 0 } });
  assert.equal(h.roomStorage.alarm, null);
});

test("Room alarm resolves {date} and the counter from the due instant, not the run instant", async () => {
  const h = harness();
  const value = Buffer.from("Late shift").toString("base64");
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    kv: { "push:2026-09-05": value },
    items: [recurring({ id: "night", at: "23:55", days: [6], body: undefined, bodyKey: "push:{date}" })],
  });
  const due = Date.parse("2026-09-06T03:55:00Z");
  assert.equal(h.push().nextFireAt, due);
  // Six minutes late: local midnight has passed, the item still belongs to Saturday.
  await h.room().alarm({ now: due + 6 * 60 * 1000 });
  assert.equal(h.sends.length, 1);
  const payload = JSON.stringify({ title: "Rota", body: "Late shift" });
  assert.equal(h.sends[0].init.body.length, 86 + utf8(payload).length + 17);
  assert.deepStrictEqual(h.push().subscriptions[0].sent, { "2026-09-05": 1 });
});

test("Room alarm treats a redirect as a failure and drains a response body", async () => {
  const responses = [];
  const h = harness({
    service: {
      fetch: (url) => {
        const res = new Response("ack", { status: url.endsWith("/2") ? 302 : 201, headers: { Location: "https://elsewhere.example/" } });
        responses.push(res);
        return res;
      },
    },
  });
  assert.equal((await h.call("pushsubput", subscription(1))).status, 204);
  assert.equal((await h.call("pushsubput", subscription(2))).status, 204);
  const outcome = await h.call("pushtest", { now: Date.parse("2026-09-05T15:00:00Z") });
  assert.deepStrictEqual(outcome, { status: 200, body: { sent: 1, pruned: 0 } });
  assert.equal(h.push().subscriptions.length, 2);
  assert.ok(responses.every((res) => res.bodyUsed), "response body left unread");
});

test("Room fire sends concurrently and a stalled endpoint times out without blocking the rest", async () => {
  // Endpoint 1 resolves only once endpoint 2 has been called: a sequential
  // loop would deadlock here until the timeout.
  const gate = {};
  gate.opened = new Promise((resolve) => { gate.open = resolve; });
  const h = harness({
    env: { PUSH_SEND_TIMEOUT_MS: 5000 },
    service: {
      fetch: async (url) => {
        if (url.endsWith("/1")) {
          await gate.opened;
        } else {
          gate.open();
        }
        return new Response(null, { status: 201 });
      },
    },
  });
  assert.equal((await h.call("pushsubput", subscription(1))).status, 204);
  assert.equal((await h.call("pushsubput", subscription(2))).status, 204);
  const outcome = await h.call("pushtest", { now: Date.parse("2026-09-05T15:00:00Z") });
  assert.deepStrictEqual(outcome, { status: 200, body: { sent: 2, pruned: 0 } });

  // A never-resolving endpoint is aborted by the timeout; the other sends land.
  const stalled = harness({
    env: { PUSH_SEND_TIMEOUT_MS: 20 },
    service: {
      fetch: (url, init) => new Promise((resolve, reject) => {
        if (url.endsWith("/1")) {
          init.signal.addEventListener("abort", () => reject(init.signal.reason));
        } else {
          resolve(new Response(null, { status: 201 }));
        }
      }),
    },
  });
  await arrange(stalled, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    subs: [subscription(1), subscription(2)],
    items: [recurring({ id: "a", days: [0] }), recurring({ id: "b", days: [0], body: "second" })],
  });
  await stalled.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(stalled.sends.length, 4);
  const push = stalled.push();
  assert.equal(push.subscriptions.length, 2);
  // Attempts count whether or not they completed.
  assert.deepStrictEqual(push.subscriptions.map((s) => s.sent), [{ "2026-09-06": 2 }, { "2026-09-06": 2 }]);
  assert.equal(push.nextFireAt, Date.parse("2026-09-13T12:00:00Z"));
});

test("Room fire decides the daily cap before dispatch", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    items: [recurring({ id: "a", days: [0] }), recurring({ id: "b", days: [0], body: "second" })],
  });
  const push = h.push();
  push.subscriptions[0].sent = { "2026-09-06": 7 };
  h.roomStorage.data.set("push", push);
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 1);
  assert.deepStrictEqual(h.push().subscriptions[0].sent, { "2026-09-06": 8 });
});

test("Room alarm that runs early keeps the push timer armed", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    items: [recurring({ id: "sun", days: [0] })],
  });
  const due = Date.parse("2026-09-06T12:00:00Z");
  // The runtime clears a fired alarm before the handler runs.
  h.roomStorage.alarm = null;
  await h.room().alarm({ now: due - 1000 });
  assert.equal(h.sends.length, 0);
  assert.equal(h.roomStorage.alarm, due);
  assert.equal(h.push().nextFireAt, due);
});

test("Room alarm with a vanished room document advances the schedule and sends nothing", async () => {
  const h = harness();
  await arrange(h, {
    now: Date.parse("2026-09-05T15:00:00Z"),
    items: [
      recurring({ id: "sun", days: [0] }),
      recurring({ id: "key", days: [0], body: undefined, bodyKey: "push:{date}" }),
    ],
  });
  h.roomStorage.data.delete("state");
  await h.room().alarm({ now: Date.parse("2026-09-06T12:00:00Z") });
  assert.equal(h.sends.length, 0);
  assert.equal(h.push().nextFireAt, Date.parse("2026-09-13T12:00:00Z"));
  assert.equal(h.roomStorage.alarm, Date.parse("2026-09-13T12:00:00Z"));
});

test("Room VAPID subject comes only from the request, never from the cell env", async () => {
  const h = harness({ env: { PUSH_SUBJECT: "https://env.example" } });
  assert.equal((await h.call("pushsubput", subscription(1))).status, 204);
  assert.equal((await h.call("pushtest", { now: Date.parse("2026-09-05T15:00:00Z") })).status, 200);
  const auth = h.sends[0].init.headers.Authorization;
  const jwt = decodeJWT(auth.slice("vapid t=".length, auth.indexOf(",")));
  assert.equal(jwt.payload.sub, undefined);

  const given = harness({ env: { PUSH_SUBJECT: "https://env.example" } });
  assert.equal((await given.call("pushsubput", { ...subscription(1), subject: "https://apex.example" })).status, 204);
  assert.equal((await given.call("pushtest", { now: Date.parse("2026-09-05T15:00:00Z") })).status, 200);
  const header = given.sends[0].init.headers.Authorization;
  assert.equal(decodeJWT(header.slice("vapid t=".length, header.indexOf(","))).payload.sub, "https://apex.example");
});
