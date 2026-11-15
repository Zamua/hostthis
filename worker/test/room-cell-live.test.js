// The Room cell's storage contract under workerd (Miniflare, from
// worker/localrt), where requests to one cell really interleave: isolation,
// the byte ceilings under racing writers, the dense per-room sequence and the
// creation ledger's prune. Skipped until `npm ci --prefix worker/localrt` has
// installed the runtime; `make test` installs it.
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";

const workerDir = fileURLToPath(new URL("..", import.meta.url));
const TOKEN = "live-token";
const ROOM_CAP = 256 * 1024;
const KEY_CAP = 256;
const NOW = Date.UTC(2026, 0, 2, 3, 4, 5);

let Miniflare = null;
try {
  ({ Miniflare } = createRequire(`${workerDir}localrt/package.json`)("miniflare"));
} catch {
  // not installed
}
const skip = Miniflare ? false : "worker/localrt is not installed";

let mf;

before(async () => {
  if (skip) return;
  const config = JSON.parse(readFileSync(`${workerDir}wrangler.jsonc`, "utf8")
    .split("\n").filter((line) => !line.trim().startsWith("//")).join("\n"));
  mf = new Miniflare({
    modules: true,
    scriptPath: `${workerDir}dist/index.js`,
    modulesRoot: workerDir,
    modulesRules: [{ type: "ESModule", include: ["**/*.js"] }, { type: "CompiledWasm", include: ["**/*.wasm"] }],
    compatibilityDate: config.compatibility_date,
    cf: false,
    bindings: { HOSTTHIS_WORKER_TOKEN: TOKEN },
    r2Buckets: Object.fromEntries(config.r2_buckets.map((b) => [b.binding, b.bucket_name])),
    durableObjectsPersist: false,
    durableObjects: Object.fromEntries(config.durable_objects.bindings
      .map((b) => [b.name, { className: b.class_name, useSQLite: true }])),
  });
  await mf.ready;
});

after(async () => {
  await mf?.dispose();
});

let apps = 0;
// A fresh app slug per test, so no two tests share a ledger or a budget.
const newApp = () => `cell${(apps++).toString(26).padStart(4, "0").replace(/./g, (c) => "abcdefghijklmnopqrstuvwxyz"[parseInt(c, 26)])}`;

async function op(path, key, body) {
  const by = path.startsWith("/paste/") ? "slug" : "room";
  const res = await mf.dispatchFetch(`http://worker.internal${path}${path.includes("?") ? "&" : "?"}${by}=${encodeURIComponent(key)}`, {
    method: body === undefined ? "GET" : "POST",
    headers: { Authorization: `Bearer ${TOKEN}` },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  return { status: res.status, body: text && res.headers.get("Content-Type")?.includes("json") ? JSON.parse(text) : text };
}

async function create(app, { id = randomUUID(), subnet = "10.0.0.0/24", at = NOW } = {}) {
  const res = await op("/room/create", `${app}|${id}`, {
    appSlug: app, id, createdAt: at, updatedAt: at, subnet, at, appCap: 0, roomLimit: 500,
  });
  assert.equal(res.status, 200, JSON.stringify(res.body));
  assert.notEqual(res.body.created, false);
  return id;
}

const put = (app, id, key, bytes, appCap = 0) => op("/room/put", `${app}|${id}`, {
  key, value: Buffer.from(bytes).toString("base64"), wire: JSON.stringify(Buffer.from(bytes).toString("latin1")),
  roomCap: ROOM_CAP, keyCap: KEY_CAP, appCap, now: NOW,
});
const del = (app, id, key) => op("/room/del", `${app}|${id}`, { key, now: NOW });
const get = (app, id, key) => op(`/room/get?key=${encodeURIComponent(key)}`, `${app}|${id}`);

async function scan(app, id) {
  const res = await op("/room/scan", `${app}|${id}`);
  assert.equal(res.status, 200, JSON.stringify(res.body));
  const values = Object.values(res.body.values ?? {});
  return { seq: res.body.seq, keys: values.length, bytes: values.reduce((n, v) => n + Buffer.from(v ?? "", "base64").length, 0) };
}

const counts = (app, subnet, window) => op(`/paste/roomcounts?subnet=${encodeURIComponent(subnet)}&now=${NOW}&window=${window}`, app);

test("a room's keys are its own: another room, or the same id under another app, sees none", { skip }, async () => {
  const app = newApp();
  const other = newApp();
  const a = await create(app);
  const b = await create(app);
  await create(other, { id: a });
  assert.equal((await put(app, a, "secret", "A-only")).status, 200);
  assert.equal((await get(app, b, "secret")).status, 404);
  assert.equal((await get(other, a, "secret")).status, 404);
  assert.equal((await scan(app, b)).keys, 0);
  assert.equal((await scan(other, a)).keys, 0);
  assert.equal((await put(app, b, "secret", "B-only")).status, 200);
  assert.equal(Buffer.from((await get(app, a, "secret")).body.value, "base64").toString(), "A-only");
});

test("a room is created under a slug that names no app: liveness is the public surface's check", { skip }, async () => {
  const app = newApp();
  const id = await create(app);
  assert.equal((await op("/room/meta", `${app}|${id}`)).status, 200);
});

test("racing writers never persist past the per-room byte cap", { skip }, async () => {
  const app = newApp();
  const id = await create(app);
  const k = 3;
  const body = Math.floor(ROOM_CAP / k);
  const results = await Promise.all(Array.from({ length: 8 }, (_, i) => put(app, id, `k${i}`, new Uint8Array(body))));
  const landed = results.filter((r) => r.status === 200).length;
  assert.ok(landed * body <= ROOM_CAP, `${landed} values of ${body} bytes landed`);
  assert.ok((await scan(app, id)).bytes <= ROOM_CAP);
});

test("sibling rooms growing at once never persist past the app's byte cap", { skip }, async () => {
  const app = newApp();
  const appCap = 100;
  const rooms = [];
  for (let i = 0; i < 4; i++) {
    const id = await create(app);
    assert.equal((await put(app, id, "k", new Uint8Array(10), appCap)).status, 200);
    rooms.push(id);
  }
  const results = await Promise.all(rooms.map((id) => put(app, id, "k", new Uint8Array(40), appCap)));
  for (const r of results) assert.ok(r.status === 200 || r.status === 507, `${r.status} ${JSON.stringify(r.body)}`);
  let total = 0;
  for (const id of rooms) total += (await scan(app, id)).bytes;
  assert.ok(total <= appCap, `${total} bytes persisted`);
});

test("every committed mutation takes the next seq, an absent key's delete included", { skip }, async () => {
  const app = newApp();
  const id = await create(app);
  assert.equal((await scan(app, id)).seq, 0);
  const steps = [
    () => put(app, id, "k1", "v1"),
    () => put(app, id, "k2", "v2"),
    () => put(app, id, "k1", "v1b"),
    () => del(app, id, "k2"),
    () => del(app, id, "never-existed"),
  ];
  for (const [i, step] of steps.entries()) {
    const res = await step();
    assert.equal(res.status, 200, JSON.stringify(res.body));
    assert.equal(res.body.seq, i + 1);
  }
  assert.equal((await scan(app, id)).seq, steps.length);
});

test("concurrent writers take unique seqs that together are 1..N", { skip }, async () => {
  const app = newApp();
  const id = await create(app);
  const writers = 4;
  const each = 20;
  const seqs = (await Promise.all(Array.from({ length: writers }, async (_, w) => {
    const out = [];
    for (let i = 0; i < each; i++) {
      const res = await put(app, id, `k${w}-${i}`, "v");
      assert.equal(res.status, 200, JSON.stringify(res.body));
      out.push(res.body.seq);
    }
    return out;
  }))).flat().sort((a, b) => a - b);
  assert.deepEqual(seqs, Array.from({ length: writers * each }, (_, i) => i + 1));
  assert.equal((await scan(app, id)).seq, writers * each);
});

test("a scan during writes holds exactly the mutations its seq names, and seq never goes back", { skip }, async () => {
  const app = newApp();
  const id = await create(app);
  const writers = 3;
  const each = 25;
  let done = false;
  const writing = Promise.all(Array.from({ length: writers }, async (_, w) => {
    for (let i = 0; i < each; i++) {
      const res = await put(app, id, `k${w}-${i}`, "v");
      assert.equal(res.status, 200, JSON.stringify(res.body));
    }
  })).finally(() => { done = true; });
  let last = 0;
  while (!done) {
    const s = await scan(app, id);
    assert.ok(s.seq >= last, `seq went from ${last} to ${s.seq}`);
    assert.equal(s.keys, s.seq, `a snapshot at seq ${s.seq} holds ${s.keys} keys`);
    last = s.seq;
  }
  await writing;
  const s = await scan(app, id);
  assert.equal(s.seq, writers * each);
  assert.equal(s.keys, writers * each);
});

test("a count past the window drops the aged-out creations it skips", { skip }, async () => {
  const app = newApp();
  const subnet = "1.2.3.0/24";
  const hour = 3600_000;
  await create(app, { subnet, at: NOW - 2 * hour });
  await create(app, { subnet, at: NOW - 2 * hour });
  await create(app, { subnet });
  assert.equal((await counts(app, subnet, 4 * hour)).body.perSubnet, 3);
  assert.equal((await counts(app, subnet, hour)).body.perSubnet, 1);
  assert.equal((await counts(app, subnet, 4 * hour)).body.perSubnet, 1);
});
