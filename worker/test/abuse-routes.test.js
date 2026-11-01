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

test("the watcher token opens its two routes and nothing else", async () => {
  const { env: e, calls } = env();
  assert.equal((await worker.fetch(req("/watcher/quarantine", WATCHER, { slug: "abcd2345", reason: "x" }), e)).status, 200);
  assert.equal((await worker.fetch(req("/watcher/ban-key", WATCHER, { identity: "SHA256:x", reason: "x" }), e)).status, 200);
  for (const path of ["/abuse/list", "/abuse/release", "/abuse/room-delete", "/denylist/list", "/paste/get?slug=abcd2345", "/watcher/release", "/watcher/room-delete"]) {
    const res = await worker.fetch(req(path, WATCHER, path.includes("release") ? { slug: "abcd2345" } : undefined), e);
    assert.ok([401, 404].includes(res.status), `${path} answered ${res.status}`);
  }
  assert.deepEqual(calls.map((c) => c.path), ["/abuse/quarantine", "/abuse/bankey"]);
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

test("a quarantine purges the slug's host now and again after the race window", async () => {
  const { env: e } = env({ HOSTTHIS_CF_PURGE_TOKEN: "p", HOSTTHIS_CF_ZONE_ID: "z", HOSTTHIS_APEX_DOMAIN: "hostthis.dev" });
  const purges = [];
  const fetcher = async (url, init) => { purges.push([url, JSON.parse(init.body).hosts]); return new Response("{}"); };
  const waits = [];
  const ctx = { waitUntil: (p) => waits.push(p) };
  let slept = 0;
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e, ctx,
    new URL(ORIGIN + "/abuse/quarantine"), "operator", { fetcher, sleep: async (ms) => { slept = ms; } });
  assert.equal((await res.json()).purged, true);
  await Promise.all(waits);
  assert.equal(slept, 15_000);
  assert.deepEqual(purges, [
    ["https://api.cloudflare.com/client/v4/zones/z/purge_cache", ["abcd2345.hostthis.dev"]],
    ["https://api.cloudflare.com/client/v4/zones/z/purge_cache", ["abcd2345.hostthis.dev"]],
  ]);
});

test("a rate-limited first purge is retried after the race window", async () => {
  const { env: e } = env({ HOSTTHIS_CF_PURGE_TOKEN: "p", HOSTTHIS_CF_ZONE_ID: "z", HOSTTHIS_APEX_DOMAIN: "hostthis.dev" });
  const statuses = [429, 200];
  const fetcher = async () => new Response("{}", { status: statuses.shift() });
  const waits = [];
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e,
    { waitUntil: (p) => waits.push(p) }, new URL(ORIGIN + "/abuse/quarantine"), "operator",
    { fetcher, sleep: async () => {} });
  assert.equal((await res.json()).purged, false);
  await Promise.all(waits);
  assert.deepEqual(statuses, []);
});

test("without a purge credential the answer says so", async () => {
  const { env: e } = env();
  const res = await abuseRoute(req("/abuse/quarantine", FULL, { slug: "abcd2345", reason: "r" }), e, null,
    new URL(ORIGIN + "/abuse/quarantine"), "operator");
  assert.equal((await res.json()).purged, false);
});
