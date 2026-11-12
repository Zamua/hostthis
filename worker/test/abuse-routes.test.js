import assert from "node:assert/strict";
import test from "node:test";

import { abuseRoute } from "../src/abuse-routes.js";
import worker from "../src/index.js";

const ORIGIN = "http://worker.internal";
const FULL = "full-token";
const WATCHER = "watcher-token";

// An ABUSE namespace recording what reaches the cell and answering ok.
function env(extra = {}) {
  const calls = [];
  const cell = {
    async fetch(request) {
      const text = request.method === "POST" ? await request.text() : "";
      calls.push({ path: new URL(request.url).pathname, body: text ? JSON.parse(text) : null });
      return Response.json({ ok: true });
    },
  };
  const ns = { idFromName: (id) => id, get: () => cell };
  return { calls, env: { HOSTTHIS_WORKER_TOKEN: FULL, HOSTTHIS_WATCHER_TOKEN: WATCHER, ABUSE: ns, DENYLIST: ns, ...extra } };
}

function req(path, token, body) {
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const init = body === undefined ? { headers } : { method: "POST", headers, body: JSON.stringify(body) };
  return new Request(ORIGIN + path, init);
}

test("the watcher token opens its three routes and nothing else", async () => {
  const { env: e, calls } = env();
  assert.equal((await worker.fetch(req("/watcher/quarantine", WATCHER, { slug: "abcd2345", reason: "x" }), e)).status, 200);
  assert.equal((await worker.fetch(req("/watcher/ban-key", WATCHER, { identity: "SHA256:x", reason: "x" }), e)).status, 200);
  assert.equal((await worker.fetch(req("/watcher/ban-owner", WATCHER, { slug: "abcd2345", reason: "x", retentionDays: 90 }), e)).status, 200);
  for (const path of ["/abuse/list", "/abuse/release", "/abuse/room-delete", "/abuse/ban-owner", "/abuse/unban-owner", "/denylist/list", "/paste/get?slug=abcd2345", "/watcher/release", "/watcher/room-delete", "/watcher/unban-owner"]) {
    const res = await worker.fetch(req(path, WATCHER, path.includes("release") ? { slug: "abcd2345" } : undefined), e);
    assert.ok([401, 404].includes(res.status), `${path} answered ${res.status}`);
  }
  assert.deepEqual(calls.map((c) => c.path), ["/abuse/quarantine", "/abuse/bankey", "/abuse/banowner"]);
  assert.equal(calls[2].body.retentionDays, undefined, "the watcher cannot set the retention");
  assert.equal(calls[2].body.actor, "watcher");
});

test("no token, or a wrong one, reaches neither route family", async () => {
  const { env: e, calls } = env();
  for (const token of [undefined, "nope"]) {
    assert.equal((await worker.fetch(req("/watcher/quarantine", token, { slug: "abcd2345" }), e)).status, 401);
    assert.equal((await worker.fetch(req("/abuse/list", token), e)).status, 401);
  }
  assert.equal(calls.length, 0);
});

test("the full token reaches the operator routes as the operator", async () => {
  const { env: e, calls } = env();
  assert.equal((await worker.fetch(req("/abuse/release", FULL, { slug: "abcd2345" }), e)).status, 200);
  assert.equal(calls[0].body.actor, "operator");
});

test("the actor and time come from the route, never the body", async () => {
  const { env: e, calls } = env();
  await abuseRoute(req("/watcher/quarantine", WATCHER, { slug: "abcd2345", reason: "r", actor: "operator", now: 1 }),
    e, null, new URL(ORIGIN + "/watcher/quarantine"), "watcher", { now: () => 42 });
  assert.deepEqual(calls[0].body, { actor: "watcher", now: 42, slug: "abcd2345", reason: "r" });
});

const PURGING = { HOSTTHIS_CF_PURGE_TOKEN: "p", HOSTTHIS_CF_ZONE_ID: "z", HOSTTHIS_APEX_DOMAIN: "hostthis.dev" };

// A PURGE namespace recording each enqueue and answering as told.
function purgeQueue(answer) {
  const asked = [];
  const cell = {
    async fetch(request) {
      asked.push(await request.json());
      return answer();
    },
  };
  return { asked, ns: { idFromName: (id) => id, get: () => cell } };
}

test("a quarantine and a release queue an urgent purge of the slug", async () => {
  for (const [path, sent] of [["/abuse/quarantine", true], ["/abuse/release", false], ["/watcher/quarantine", true]]) {
    const queue = purgeQueue(() => Response.json({ configured: true, queued: 1, sent }));
    const { env: e } = env({ ...PURGING, PURGE: queue.ns });
    const actor = path.startsWith("/watcher/") ? "watcher" : "operator";
    const res = await abuseRoute(req(path, FULL, { slug: "abcd2345", reason: "r" }), e, null, new URL(ORIGIN + path), actor);
    const body = await res.json();
    assert.deepEqual([body.purged, body.purgeQueued], [sent, true], path);
    assert.deepEqual(queue.asked, [{ slugs: ["abcd2345"], urgent: true }], path);
  }
});

test("a change whose purge could not be queued still answers, and says so", async () => {
  const queue = purgeQueue(() => new Response("down\n", { status: 500 }));
  const { env: e, calls } = env({ ...PURGING, PURGE: queue.ns });
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e, null,
    new URL(ORIGIN + "/abuse/quarantine"), "operator");
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.deepEqual([body.ok, body.purged, body.purgeQueued], [true, false, false]);
  assert.equal(calls.length, 1, "the quarantine itself was made");
});

test("a refused change queues no purge", async () => {
  const queue = purgeQueue(() => Response.json({ configured: true, queued: 1, sent: true }));
  const { env: e } = env({ ...PURGING, PURGE: queue.ns });
  e.ABUSE = { idFromName: (id) => id, get: () => ({ fetch: async () => Response.json({ error: "no-such-paste" }, { status: 404 }) }) };
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e, null,
    new URL(ORIGIN + "/abuse/quarantine"), "operator");
  assert.equal(res.status, 404);
  assert.deepEqual(queue.asked, []);
});

test("without a purge credential the answer says so", async () => {
  const { env: e } = env();
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e, null,
    new URL(ORIGIN + "/abuse/quarantine"), "operator");
  const body = await res.json();
  assert.deepEqual([body.purged, body.purgeQueued], [false, false]);
});

test("GET /watcher/refused-bans validates its query and answers ISO times, GET only", async () => {
  const seen = [];
  const cell = { async fetch(request) {
    seen.push(new URL(request.url).pathname + new URL(request.url).search);
    return Response.json({ entries: [{ at: 1_800_000_000_000, action: "ban-key", target: "abcdef0123", reason: "r", already_banned: false }] });
  } };
  const ns = { idFromName: (id) => id, get: () => cell };
  const e = { HOSTTHIS_WORKER_TOKEN: FULL, HOSTTHIS_WATCHER_TOKEN: WATCHER, ABUSE: ns, DENYLIST: ns };
  const clock = () => Date.parse("2027-01-15T08:00:00Z");
  const res = await abuseRoute(req("/watcher/refused-bans?since=2027-01-14T00:00:00Z&limit=10", WATCHER), e, undefined,
    new URL(ORIGIN + "/watcher/refused-bans?since=2027-01-14T00:00:00Z&limit=10"), "watcher", { now: clock });
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.now, "2027-01-15T08:00:00.000Z");
  assert.deepEqual(body.entries, [{ at: "2027-01-15T08:00:00.000Z", action: "ban-key", target: "abcdef0123", reason: "r", already_banned: false }]);
  assert.equal(seen[0], `/abuse/refused?since=${Date.parse("2027-01-14T00:00:00Z")}&limit=10`);
  // Defaults: the last 24 hours, 50 entries.
  await abuseRoute(req("/watcher/refused-bans", WATCHER), e, undefined, new URL(ORIGIN + "/watcher/refused-bans"), "watcher", { now: clock });
  assert.equal(seen[1], `/abuse/refused?since=${clock() - 86_400_000}&limit=50`);
  for (const q of ["?since=yesterday", "?limit=0", "?limit=201", "?limit=x"]) {
    const bad = await abuseRoute(req("/watcher/refused-bans" + q, WATCHER), e, undefined, new URL(ORIGIN + "/watcher/refused-bans" + q), "watcher", { now: clock });
    assert.equal(bad.status, 400, q);
  }
  const post = await abuseRoute(new Request(ORIGIN + "/watcher/refused-bans", { method: "POST", headers: { Authorization: `Bearer ${WATCHER}` }, body: "{}" }), e, undefined, new URL(ORIGIN + "/watcher/refused-bans"), "watcher", { now: clock });
  assert.equal(post.status, 405);
  assert.equal(seen.length, 2, "bad requests never reach the cell");
  const viaWorker = await worker.fetch(req("/watcher/refused-bans", WATCHER), e);
  assert.equal(viaWorker.status, 200);
  const operator = await worker.fetch(req("/abuse/refused-bans", FULL), e);
  assert.equal(operator.status, 404);
});

test("GET /watcher/key-subnets validates its query, answers ISO times, GET only", async () => {
  const seen = [];
  const cell = { async fetch(request) {
    seen.push(new URL(request.url).pathname + new URL(request.url).search);
    return Response.json({ groups: [{ subnet: "203.0.113.0/24", first: 1_800_000_000_000, last: 1_800_000_060_000,
      keys: [{ key: "abcdef0123", owner: "01234567", at: 1_800_000_000_000, banned: false }] }] });
  } };
  const ns = { idFromName: (id) => id, get: () => cell };
  const e = { HOSTTHIS_WORKER_TOKEN: FULL, HOSTTHIS_WATCHER_TOKEN: WATCHER, ABUSE: ns, DENYLIST: ns };
  const now = Date.parse("2027-01-15T08:00:00Z");
  const call = (q, method = "GET") => abuseRoute(new Request(ORIGIN + "/watcher/key-subnets" + q, { method, headers: { Authorization: `Bearer ${WATCHER}` } }),
    e, undefined, new URL(ORIGIN + "/watcher/key-subnets" + q), "watcher", { now: () => now });
  const ok = await call("");
  assert.equal(ok.status, 200);
  const body = await ok.json();
  assert.equal(body.now, "2027-01-15T08:00:00.000Z");
  assert.deepEqual(body.groups[0], { subnet: "203.0.113.0/24", first: new Date(1_800_000_000_000).toISOString(),
    last: new Date(1_800_000_060_000).toISOString(),
    keys: [{ key: "abcdef0123", owner: "01234567", at: new Date(1_800_000_000_000).toISOString(), banned: false }] });
  assert.equal(seen[0], `/abuse/keysubnets?now=${now}&window_min=60&min_keys=3&limit=50`);
  await call("?window_min=1440&min_keys=2&limit=10");
  assert.equal(seen[1], `/abuse/keysubnets?now=${now}&window_min=1440&min_keys=2&limit=10`);
  for (const q of ["?window_min=0", "?window_min=1441", "?min_keys=0", "?min_keys=x", "?limit=0", "?limit=201"]) {
    assert.equal((await call(q)).status, 400, q);
  }
  assert.equal((await call("", "POST")).status, 405);
  assert.equal(seen.length, 2);
  assert.equal((await worker.fetch(req("/watcher/key-subnets", WATCHER), e)).status, 200);
});
