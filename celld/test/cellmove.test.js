import assert from "node:assert/strict";
import test from "node:test";

import worker, { Identity, IntentLog, Paste, Room, Subnet } from "../src/index.js";
import { FakeStorage, ROOM_ID, state } from "./harness.js";

const CLASSES = { IDENTITY: Identity, PASTES: Paste, ROOMS: Room, SUBNETS: Subnet };

// RFC 8291 Appendix A subscriber keys.
const UA_PUBLIC = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4";
const UA_AUTH = "BTBZMqHH6r4Tts7J_aSIgg";

function cellState(storage, name) {
  return { ...state(storage), id: { name, toString() { return "c".repeat(64); } } };
}

// Every namespace resolves names to cells over one shared map of storages, so
// cell-to-cell calls land on the same state a test inspects.
function world() {
  const storages = new Map();
  const env = {};
  const storageOf = (ns, name) => {
    const key = `${ns}/${name}`;
    if (!storages.has(key)) {
      storages.set(key, new FakeStorage());
    }
    return storages.get(key);
  };
  const cell = (ns, name) => new CLASSES[ns](cellState(storageOf(ns, name), name), env);
  for (const ns of Object.keys(CLASSES)) {
    env[ns] = {
      idFromName(name) { return name; },
      get(name) { return { fetch: (request) => cell(ns, name).fetch(request) }; },
    };
  }
  return { env, cell, storageOf };
}

async function call(cell, path, body, method = body === undefined ? "GET" : "POST") {
  const init = { method };
  if (body !== undefined) {
    init.body = typeof body === "string" ? body : JSON.stringify(body);
  }
  const res = await cell.fetch(new Request(`https://cell${path}`, init));
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    // not every answer is JSON
  }
  return { status: res.status, body: json, text };
}

async function ok(cell, path, body, method) {
  const res = await call(cell, path, body, method);
  assert.ok(res.status < 300, `${path}: ${res.status} ${res.text}`);
  return res.body;
}

function b64(text) {
  return Buffer.from(text).toString("base64");
}

function manifest(uploadId, files) {
  return {
    Files: Object.fromEntries(files.map((path, n) => [path, {
      Key: `uploads/${uploadId}/${n}`, Size: 10 + n, CompressedSize: 5 + n,
      ContentType: "text/html; charset=utf-8", Kind: "",
    }])),
  };
}

// An owner with a confirmed site of two versions, an outstanding create intent,
// and a subnet note; the site's app cell with VAPID keys and a room's ledger and
// allocation; and that room with values, a subscription, and a schedule.
async function seededWorld() {
  const w = world();
  const owner = () => w.cell("IDENTITY", "owner");
  const site = () => w.cell("PASTES", "slugone1");
  const room = () => w.cell("ROOMS", "room-1");
  const reserve = (slug, size) => ok(owner(), "/identity/reserve?scope=owner", {
    slug, generation: `gen-${slug}`, size, userCap: 1000, status: "pending", now: 9, updatedAt: 9,
    intent: {
      id: `create:${slug}:gen-${slug}`, kind: "create_paste", subject: slug,
      fingerprint: `fp-${slug}`, reached: ["identity"], guard: b64("guard"), startedAt: 9,
    },
  });

  await reserve("slugone1", 3);
  await ok(site(), "/paste/put?slug=slugone1", {
    generation: "gen-slugone1", fingerprint: "fp-slugone1",
    row: {
      slug: "slugone1", identity: "owner", status: "pending", kind: "site", size: 3,
      createdAt: 9, updatedAt: 9, uploadId: "u1", name: "",
      manifest: manifest("u1", ["index.html"]),
    },
  });
  await ok(owner(), "/identity/confirm?scope=owner", {
    slug: "slugone1", generation: "gen-slugone1", status: "ready", intentId: "create:slugone1:gen-slugone1",
  });
  await ok(site(), "/paste/append?slug=slugone1", {
    opId: "append-1", generation: "gen-slugone1", userCap: 1000, kind: "site", size: 4, now: 10,
    uploadId: "u2", manifest: manifest("u2", ["index.html", "a/b.css"]),
  });
  await ok(site(), "/paste/pushkey?slug=slugone1", undefined, "POST");
  await reserve("slugtwo2", 2);
  await ok(owner(), "/identity/notesubnet?scope=owner", { subnet: "10.0.0.0/24", now: 5 });

  await ok(room(), "/room/create?room=room-1", {
    appSlug: "slugone1", id: ROOM_ID, createdAt: 1, updatedAt: 1, appCap: 1000, subnet: "10.0.0.0/24", at: 5,
  });
  for (const key of ["greeting", "__proto__"]) {
    await ok(room(), "/room/put?room=room-1", {
      key, value: b64(`"${key}"`), wire: JSON.stringify(key), roomCap: 1000, keyCap: 10, appCap: 1000, now: 9,
    });
  }
  await ok(room(), "/room/pushsubput?room=room-1", {
    endpoint: "https://fcm.googleapis.com/fcm/send/1", keys: { p256dh: UA_PUBLIC, auth: UA_AUTH }, now: 5,
  });
  await ok(room(), "/room/pushscheduleput?room=room-1", {
    tz: "UTC", items: [{ id: "daily", at: "09:00", days: [1, 3, 5], title: "Hi", body: "there" }],
  });

  return { w, owner, site, room };
}

async function moveTo(source, target) {
  const image = await call(source, "/__cell/image");
  assert.equal(image.status, 200, image.text);
  const restored = await call(target, "/__cell/restore", image.text);
  assert.equal(restored.status, 200, restored.text);
  return { image: image.body, restored: restored.body };
}

test("a cell answers the name it was addressed by, or null", async () => {
  for (const [ns, Cls] of [...Object.entries(CLASSES), ["INTENT_LOGS", IntentLog]]) {
    const named = new Cls(cellState(new FakeStorage(), "owner"), {});
    assert.deepStrictEqual((await call(named, "/__cell/name")).body, { name: "owner" }, ns);
    const unnamed = new Cls(cellState(new FakeStorage(), undefined), {});
    assert.deepStrictEqual((await call(unnamed, "/__cell/name")).body, { name: null }, ns);
  }
});

test("seeded identity, paste, and room cells move whole into blank cells", async () => {
  const { owner, site, room } = await seededWorld();
  const there = world();
  for (const [ns, name, source] of [["IDENTITY", "owner", owner], ["PASTES", "slugone1", site], ["ROOMS", "room-1", room]]) {
    const from = source();
    const to = there.cell(ns, name);
    const { image, restored } = await moveTo(from, to);
    assert.equal(image.format, "hostthis.cell-image");
    assert.equal(image.version, 1);
    assert.equal(restored.restored, true);
    assert.equal(restored.entries, from.state.storage.data.size, ns);
    assert.deepStrictEqual(to.state.storage.data, from.state.storage.data, ns);
    assert.equal(to.state.storage.alarm, from.state.storage.alarm, ns);
    assert.deepStrictEqual((await call(to, "/__cell/image")).body, image, ns);
  }

  const owner2 = there.cell("IDENTITY", "owner");
  assert.ok(owner().state.storage.data.has("intent:create:slugtwo2:gen-slugtwo2"));
  assert.notEqual(owner().state.storage.alarm, null);
  assert.deepStrictEqual(await ok(owner2, "/identity/list?scope=owner"), await ok(owner(), "/identity/list?scope=owner"));

  const site2 = there.cell("PASTES", "slugone1");
  assert.ok(site().state.storage.data.has("pushVapid"));
  assert.deepStrictEqual(await ok(site2, "/paste/get?slug=slugone1"), await ok(site(), "/paste/get?slug=slugone1"));
  assert.deepStrictEqual(await ok(site2, "/paste/versions?slug=slugone1"), await ok(site(), "/paste/versions?slug=slugone1"));
  assert.deepStrictEqual(await ok(site2, "/paste/pushkey?slug=slugone1", undefined, "POST"),
    await ok(site(), "/paste/pushkey?slug=slugone1", undefined, "POST"));

  const room2 = there.cell("ROOMS", "room-1");
  assert.ok(Object.hasOwn(room().state.storage.data.get("state").kv, "__proto__"));
  assert.notEqual(room().state.storage.alarm, null);
  for (const op of ["scan", "pushsublist", "pushscheduleget"]) {
    assert.deepStrictEqual(await ok(room2, `/room/${op}?room=room-1`), await ok(room(), `/room/${op}?room=room-1`), op);
  }
});

test("subnet and intent-log cells move too", async () => {
  const w = world();
  await ok(w.cell("SUBNETS", "10.0.0.0/24"), "/subnet/admit?subnet=10.0.0.0/24",
    { identity: "owner", now: 5, window: 100, limit: 3 });
  const there = world();
  await moveTo(w.cell("SUBNETS", "10.0.0.0/24"), there.cell("SUBNETS", "10.0.0.0/24"));
  assert.deepStrictEqual(there.storageOf("SUBNETS", "10.0.0.0/24").data, w.storageOf("SUBNETS", "10.0.0.0/24").data);

  const legacy = new FakeStorage(new Map([["intent:x", { kind: "create_paste" }]]));
  const blank = new FakeStorage();
  await moveTo(new IntentLog(cellState(legacy, "x"), {}), new IntentLog(cellState(blank, "x"), {}));
  assert.deepStrictEqual(blank.data, legacy.data);
  assert.equal((await call(new IntentLog(cellState(blank, "x"), {}), "/anything")).status, 410);
});

test("every structured-clone value type round-trips to the same type", async () => {
  const bytes = new Uint8Array([0, 1, 254, 255]);
  const tagged = { $t: "bytes", v: "not a tag" };
  const protoKey = JSON.parse('{"__proto__": {"x": 1}, "y": 2}');
  const values = new Map([
    ["bytes", bytes],
    ["buffer", bytes.slice().buffer],
    ["floats", new Float64Array([1.5, -0, Number.NaN])],
    ["view", new DataView(new Uint8Array([9, 8, 7]).buffer)],
    ["date", new Date(1_700_000_000_123)],
    ["bad-date", new Date(Number.NaN)],
    ["map", new Map([[1, "one"], ["two", new Set([2n])], [{ k: true }, [new Date(0)]]])],
    ["set", new Set(["a", 1, null])],
    ["bigint", -12345678901234567890n],
    ["numbers", [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, -0, 0, 1.25]],
    ["undefined-inside", { a: undefined, b: [undefined, 1] }],
    ["undefined", undefined],
    ["nested", { deep: { list: [bytes, { when: new Date(5) }] } }],
    ["looks-tagged", tagged],
    ["proto-key", protoKey],
    ["empty", {}],
    ["string", "text"],
    ["true", true],
    ["null", null],
  ]);
  const source = new FakeStorage(values);
  const target = new FakeStorage();
  await moveTo(new Paste(cellState(source, "s"), {}), new Paste(cellState(target, "s"), {}));
  assert.deepStrictEqual(target.data, source.data);
  assert.ok(Object.hasOwn(target.data.get("proto-key"), "__proto__"));
  assert.equal(Object.getPrototypeOf(target.data.get("proto-key")), Object.prototype);
  assert.ok(Object.is(target.data.get("numbers")[3], -0));
});

test("an image of a value it cannot carry fails loudly and names the key", async () => {
  const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign"]);
  const keyed = new FakeStorage(new Map([["ok", 1], ["key", { inner: pair.privateKey }]]));
  const res = await call(new Paste(cellState(keyed, "s"), {}), "/__cell/image");
  assert.equal(res.status, 500);
  assert.equal(res.body.error, "unsupported-value");
  assert.equal(res.body.key, "key");

  const fn = { list: async () => new Map([["fn", () => 1]]), getAlarm: async () => null };
  const res2 = await call(new Room({ ...state(fn), id: { name: "r" } }, {}), "/__cell/image");
  assert.equal(res2.status, 500);
  assert.equal(res2.body.key, "fn");
});

test("restore sets a past alarm to fire at once and keeps a future one", async (t) => {
  const realNow = Date.now;
  Date.now = () => 50_000;
  t.after(() => { Date.now = realNow; });
  for (const [at, want] of [[5, 50_000], [90_000, 90_000]]) {
    const source = new FakeStorage(new Map([["row", { a: 1 }]]), false, at);
    const target = new FakeStorage();
    const { image } = await moveTo(new Paste(cellState(source, "s"), {}), new Paste(cellState(target, "s"), {}));
    assert.equal(image.alarm, at);
    assert.equal(target.alarm, want);
  }
  const alarmOnly = new FakeStorage(new Map(), false, 7);
  const target = new FakeStorage();
  const { restored } = await moveTo(new Room(cellState(alarmOnly, "r"), {}), new Room(cellState(target, "r"), {}));
  assert.deepStrictEqual(restored, { restored: true, entries: 0 });
  assert.equal(target.alarm, 50_000);
});

test("a cell holding anything refuses a restore and is left as it was", async () => {
  const image = await call(new Paste(cellState(new FakeStorage(new Map([["row", 1]]), false, 9), "s"), {}), "/__cell/image");
  for (const held of [new FakeStorage(new Map([["other", 2]])), new FakeStorage(new Map(), false, 123)]) {
    const before = { data: structuredClone(held.data), alarm: held.alarm };
    const res = await call(new Paste(cellState(held, "s"), {}), "/__cell/restore", image.text);
    assert.equal(res.status, 409);
    assert.deepStrictEqual(res.body, { restored: false, reason: "not-blank" });
    assert.deepStrictEqual(held.data, before.data);
    assert.equal(held.alarm, before.alarm);
    assert.equal(held.commits, 0);
  }
});

test("restore writes in batches of at most 128 keys", async () => {
  const source = new FakeStorage(new Map(Array.from({ length: 300 }, (_, i) => [`k${i}`, i])));
  const target = new FakeStorage();
  const { restored } = await moveTo(new Identity(cellState(source, "o"), {}), new Identity(cellState(target, "o"), {}));
  assert.equal(restored.entries, 300);
  assert.deepStrictEqual(target.data, source.data);
});

test("a restore the storage refuses leaves the cell blank", async () => {
  const image = await call(new Paste(cellState(new FakeStorage(new Map([["a", 1], ["big", 2]]), false, 9), "s"), {}),
    "/__cell/image");
  const target = new FakeStorage();
  target.tooBig = (key) => key === "big";
  const res = await call(new Paste(cellState(target, "s"), {}), "/__cell/restore", image.text);
  assert.equal(res.status, 500);
  assert.equal(res.body.error, "restore-failed");
  assert.equal(target.data.size, 0);
  assert.equal(target.alarm, null);
});

test("a malformed image is refused before anything is written", async () => {
  const good = { format: "hostthis.cell-image", version: 1, kv: [["a", 1]], alarm: null };
  const bad = [
    "not json",
    { ...good, format: "other" },
    { ...good, version: 2 },
    { ...good, kv: { a: 1 } },
    { ...good, kv: [["a"]] },
    { ...good, kv: [[1, 1]] },
    { ...good, kv: [["a", 1], ["a", 2]] },
    { ...good, kv: [["a", { $t: "nope" }]] },
    { ...good, kv: [["a", { $t: "bytes", v: "!!" }]] },
    { ...good, kv: [["a", { $t: "number", v: "12" }]] },
    { ...good, kv: [["a", { $t: "bigint", v: "1.5" }]] },
    { ...good, kv: [["a", { $t: "view", k: "Function", v: "" }]] },
    { ...good, alarm: "soon" },
  ];
  for (const image of bad) {
    const target = new FakeStorage();
    const res = await call(new Paste(cellState(target, "s"), {}), "/__cell/restore", image);
    assert.equal(res.status, 400, JSON.stringify(image));
    assert.equal(res.body.error, "invalid-image", JSON.stringify(image));
    assert.equal(target.data.size, 0);
    assert.equal(target.alarm, null);
  }
});

test("cell routes answer only their own method", async () => {
  const paste = new Paste(cellState(new FakeStorage(), "s"), {});
  assert.equal((await call(paste, "/__cell/image", {})).status, 405);
  assert.equal((await call(paste, "/__cell/name", {})).status, 405);
  assert.equal((await call(paste, "/__cell/restore")).status, 405);
  assert.equal((await call(paste, "/__cell/get")).status, 404);
});

test("the Worker's router never forwards a cell route", async () => {
  const forwarded = [];
  const env = {};
  for (const ns of Object.keys(CLASSES)) {
    const Cls = CLASSES[ns];
    env[ns] = {
      idFromName(name) { return name; },
      get(name) {
        return {
          fetch(request) {
            forwarded.push(new URL(request.url).pathname);
            return new Cls(cellState(new FakeStorage(new Map([["row", 1]])), name), {}).fetch(request);
          },
        };
      },
    };
  }
  for (const path of ["/__cell/image", "/__cell/name", "/paste/../__cell/image?slug=s", "/x/%2e%2e/__cell/name"]) {
    const res = await worker.fetch(new Request(`https://w${path}`), env);
    assert.equal(res.status, 404, path);
  }
  const restore = await worker.fetch(new Request("https://w/__cell/restore", { method: "POST", body: "{}" }), env);
  assert.equal(restore.status, 404);
  assert.deepStrictEqual(forwarded, []);

  for (const path of ["/paste/__cell/image?slug=s", "/identity/__cell/image?scope=s",
    "/room/__cell/image?room=s", "/subnet/__cell/image?subnet=s"]) {
    const res = await worker.fetch(new Request(`https://w${path}`), env);
    assert.notEqual(res.status, 200, path);
    assert.ok(!(await res.text()).includes("hostthis.cell-image"), path);
  }
  assert.deepStrictEqual(forwarded, []);
});

function socket({ closed = false } = {}) {
  const calls = [];
  return {
    calls,
    close(...args) {
      if (closed) {
        throw new TypeError("WebSocket already closed");
      }
      calls.push(args);
    },
  };
}

test("a room answers a peer's close frame", async () => {
  const room = new Room(cellState(new FakeStorage(), "r"), {});
  for (const [code, reason, want] of [
    [1000, "bye", [1000, "bye"]],
    [4001, "app", [4001, "app"]],
    [1001, "away", [1000]],
    [1005, "", [1000]],
  ]) {
    const ws = socket();
    await room.webSocketClose(ws, code, reason, true);
    assert.deepStrictEqual(ws.calls, [want], String(code));
  }
  await room.webSocketClose(socket({ closed: true }), 1000, "bye", true);

  const errored = socket();
  await room.webSocketError(errored, new Error("reset"));
  assert.equal(errored.calls.length, 1);
  await room.webSocketError(socket({ closed: true }), new Error("reset"));
});

// Pages the source, then restores the pages as parts with the paste's row last.
async function moveInParts(source, target, limit) {
  const pages = [];
  let start = "";
  do {
    const page = await ok(source, `/__cell/image?start=${encodeURIComponent(start)}&limit=${limit}`);
    pages.push(page);
    start = page.next;
  } while (start !== null);
  const kvs = pages.map((page) => page.kv.filter(([key]) => key !== "row"));
  kvs[kvs.length - 1].push(...pages.flatMap((page) => page.kv.filter(([key]) => key === "row")));
  const answers = [];
  for (const [index, kv] of kvs.entries()) {
    answers.push(await ok(target, "/__cell/restore", {
      format: "hostthis.cell-image", version: 1, kv, alarm: pages[index].alarm,
      part: { id: "move-1", index, count: kvs.length },
    }));
  }
  return { pages, answers };
}

test("an image pages in key order and names the next key", async () => {
  const { site } = await seededWorld();
  const whole = await ok(site(), "/__cell/image");
  assert.equal(whole.next, null);
  const first = await ok(site(), "/__cell/image?limit=2");
  assert.deepStrictEqual(first.kv, whole.kv.slice(0, 2));
  assert.equal(first.next, whole.kv[2][0]);
  assert.equal(first.alarm, whole.alarm);
  const rest = await ok(site(), `/__cell/image?start=${encodeURIComponent(first.next)}&limit=1000`);
  assert.deepStrictEqual(rest.kv, whole.kv.slice(2));
  assert.equal(rest.next, null);
  for (const limit of ["0", "-1", "1.5", "x"]) {
    assert.equal((await call(site(), `/__cell/image?limit=${limit}`)).status, 400, limit);
  }
});

test("a cell moved in parts is the same cell, and serves only once the last part lands", async () => {
  const { site } = await seededWorld();
  const there = world();
  const target = () => there.cell("PASTES", "slugone1");
  const { pages, answers } = await moveInParts(site(), target(), 2);
  assert.ok(pages.length > 2);
  assert.deepStrictEqual(answers.map((a) => a.complete), pages.map((_, i) => i === pages.length - 1));
  assert.deepStrictEqual(there.storageOf("PASTES", "slugone1").data, site().state.storage.data);
  assert.equal(there.storageOf("PASTES", "slugone1").alarm, site().state.storage.alarm);
  assert.deepStrictEqual(await ok(target(), "/paste/get?slug=slugone1"), await ok(site(), "/paste/get?slug=slugone1"));
});

test("parts refuse out of order, a stranger's move, and a key the cell holds, and a retry resumes", async () => {
  const { site } = await seededWorld();
  const { kv, alarm } = await ok(site(), "/__cell/image");
  const blank = new FakeStorage();
  const target = new Paste(cellState(blank, "slugone1"), {});
  const part = (index, entries, id = "move-1") => ({
    format: "hostthis.cell-image", version: 1, kv: entries, alarm, part: { id, index, count: 3 },
  });
  const restore = (body) => call(target, "/__cell/restore", body);

  assert.deepStrictEqual((await restore(part(1, kv.slice(1, 2)))).body, { restored: false, reason: "out-of-order" });
  assert.equal(blank.data.size, 0);
  assert.equal((await restore(part(0, kv.slice(0, 1)))).status, 200);
  assert.equal(blank.alarm, null);
  const held = new Map(blank.data);
  assert.deepStrictEqual((await restore(part(1, kv.slice(1, 2), "move-2"))).body, { restored: false, reason: "out-of-order" });
  assert.deepStrictEqual((await restore(part(2, kv.slice(1)))).body, { restored: false, reason: "out-of-order" });
  assert.deepStrictEqual((await restore(part(1, kv.slice(0, 2)))).body, { restored: false, reason: "overlap" });
  assert.deepStrictEqual((await restore(part(0, kv.slice(1)))).body, { restored: false, reason: "not-blank" });
  assert.deepStrictEqual(blank.data, held);

  blank.tooBig = (key) => key === kv[1][0];
  assert.equal((await restore(part(1, kv.slice(1, 2)))).status, 500);
  assert.deepStrictEqual(blank.data, held);
  blank.tooBig = null;
  assert.equal((await restore(part(1, kv.slice(1, 2)))).status, 200);
  assert.deepStrictEqual((await restore(part(2, kv.slice(2)))).body, { restored: true, entries: kv.length - 2, part: 2, complete: true });
  assert.deepStrictEqual(blank.data, site().state.storage.data);
  assert.equal(blank.alarm, site().state.storage.alarm);
});

test("an image may not carry the restore progress key", async () => {
  const target = new Paste(cellState(new FakeStorage(), "slugone1"), {});
  const res = await call(target, "/__cell/restore", {
    format: "hostthis.cell-image", version: 1, kv: [["__cell:restore", { id: "x", next: 1, count: 2 }]], alarm: null,
  });
  assert.equal(res.status, 400);
  for (const part of [{}, { id: "", index: 0, count: 1 }, { id: "m", index: 1, count: 1 }, { id: "m", index: 0, count: 0 }]) {
    const bad = await call(target, "/__cell/restore", { format: "hostthis.cell-image", version: 1, kv: [], alarm: null, part });
    assert.equal(bad.status, 400, JSON.stringify(part));
  }
});
