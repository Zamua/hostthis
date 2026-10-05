import assert from "node:assert/strict";
import test from "node:test";

import worker from "../src/index.js";

const ORIGIN = "http://celld.internal";
const ID_A = "a".repeat(32);
const ID_B = "b".repeat(32);

// An R2 binding double with the paging and batch limits the Worker must
// respect. Bodies arrive and leave as streams, as they do from the host.
class FakeR2 {
  constructor({ pageSize = 1000 } = {}) {
    this.objects = new Map();
    this.pageSize = pageSize;
    this.deleteBatches = [];
  }

  async put(key, value) {
    assert.ok(value instanceof ReadableStream || value instanceof Uint8Array,
      `put(${key}) got ${Object.prototype.toString.call(value)}, want a stream`);
    const bytes = value instanceof Uint8Array
      ? value
      : new Uint8Array(await new Response(value).arrayBuffer());
    this.objects.set(key, bytes);
    return { key, size: bytes.length };
  }

  async get(key) {
    const bytes = this.objects.get(key);
    if (bytes === undefined) return null;
    return { key, size: bytes.length, body: new Response(bytes).body };
  }

  async list({ prefix = "", cursor } = {}) {
    const keys = [...this.objects.keys()].filter((k) => k.startsWith(prefix)).sort();
    const start = cursor ? Number(cursor) : 0;
    const page = keys.slice(start, start + this.pageSize);
    const truncated = start + this.pageSize < keys.length;
    return {
      objects: page.map((key) => ({ key, size: this.objects.get(key).length })),
      truncated,
      cursor: truncated ? String(start + this.pageSize) : undefined,
    };
  }

  async delete(keys) {
    const batch = Array.isArray(keys) ? keys : [keys];
    assert.ok(batch.length <= 1000, `delete batch of ${batch.length} exceeds 1000`);
    this.deleteBatches.push(batch.length);
    for (const key of batch) this.objects.delete(key);
  }
}

function call(env, method, path, body) {
  const init = { method };
  if (body !== undefined) {
    init.body = body;
    init.duplex = "half";
  }
  return worker.fetch(new Request(ORIGIN + path, init), env);
}

test("a blob round-trips through PUT and GET with its size", async () => {
  const env = { PAYLOADS: new FakeR2() };
  const body = "HZ\0\x01 opaque payload";
  const put = await call(env, "PUT", `/blob/uploads/${ID_A}/0`, body);
  assert.equal(put.status, 204);

  const get = await call(env, "GET", `/blob/uploads/${ID_A}/0`);
  assert.equal(get.status, 200);
  assert.equal(get.headers.get("X-Blob-Size"), String(Buffer.byteLength(body)));
  assert.equal(await get.text(), body);
});

test("an empty blob round-trips", async () => {
  const env = { PAYLOADS: new FakeR2() };
  assert.equal((await call(env, "PUT", `/blob/uploads/${ID_A}/0`, "")).status, 204);
  const get = await call(env, "GET", `/blob/uploads/${ID_A}/0`);
  assert.equal(get.status, 200);
  assert.equal(await get.text(), "");
});

test("an absent blob is 404", async () => {
  const env = { PAYLOADS: new FakeR2() };
  assert.equal((await call(env, "GET", `/blob/uploads/${ID_A}/0`)).status, 404);
});

test("a prefix delete removes only that upload, across pages and batches", async () => {
  const env = { PAYLOADS: new FakeR2({ pageSize: 700 }) };
  for (let n = 0; n < 1500; n++) env.PAYLOADS.objects.set(`uploads/${ID_A}/${n}`, new Uint8Array(1));
  env.PAYLOADS.objects.set(`uploads/${ID_A}x/0`, new Uint8Array(1));
  env.PAYLOADS.objects.set(`uploads/${ID_B}/0`, new Uint8Array(1));

  const res = await call(env, "DELETE", `/blob?prefix=${encodeURIComponent(`uploads/${ID_A}/`)}`);
  assert.equal(res.status, 204);
  assert.deepEqual([...env.PAYLOADS.objects.keys()].sort(), [`uploads/${ID_A}x/0`, `uploads/${ID_B}/0`]);
  assert.deepEqual(env.PAYLOADS.deleteBatches, [1000, 500]);
});

test("deleting an absent prefix succeeds", async () => {
  const env = { PAYLOADS: new FakeR2() };
  const res = await call(env, "DELETE", `/blob?prefix=${encodeURIComponent(`uploads/${ID_A}/`)}`);
  assert.equal(res.status, 204);
});

test("an invalid key is refused with 400", async () => {
  const env = { PAYLOADS: new FakeR2() };
  for (const path of [
    "/blob/",
    `/blob/uploads/${ID_A}/`,
    `/blob/uploads//0`,
    `/blob/uploads/%2E%2E%2Fx`,
    `/blob/uploads/a%5Cb`,
    `/blob/uploads/a%00b`,
    `/blob/uploads/%E0%A4%A`,
  ]) {
    assert.equal((await call(env, "PUT", path, "x")).status, 400, `PUT ${path}`);
    assert.equal((await call(env, "GET", path)).status, 400, `GET ${path}`);
  }
  assert.equal(env.PAYLOADS.objects.size, 0);
});

test("a delete prefix that is empty, unbounded, or outside uploads/ is refused", async () => {
  const env = { PAYLOADS: new FakeR2() };
  env.PAYLOADS.objects.set(`uploads/${ID_A}/0`, new Uint8Array(1));
  env.PAYLOADS.objects.set("other/0", new Uint8Array(1));
  for (const query of [
    "",
    "?prefix=",
    "?prefix=%2F",
    "?prefix=uploads",
    "?prefix=uploads%2F",
    `?prefix=uploads%2F${ID_A}`,
    "?prefix=other%2F",
    "?prefix=uploads%2F..%2F",
    "?prefix=..%2F",
  ]) {
    assert.equal((await call(env, "DELETE", `/blob${query}`)).status, 400, `DELETE /blob${query}`);
  }
  assert.equal(env.PAYLOADS.objects.size, 2);
});
