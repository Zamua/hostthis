import assert from "node:assert/strict";
import { test } from "node:test";

import {
  CALL_GAP_MS, FAILURE_WAIT_MS, PURGE_HOSTS_MAX, Purge, QUEUE_MAX, SECOND_PURGE_MS, SECOND_SLACK_MS,
  STATUS_MAX_AGE_MS, enqueuePurge, purgeConfig, purgeStatus,
} from "../src/cells/purge.js";
import { FakeStorage, denylistEnv, state } from "./harness.js";

const MINUTE = 60_000;

// A Purge cell on a clock the test moves, over a purge API the test scripts:
// `answers` are taken in order (a status, or [status, headers]); with none
// left a call succeeds.
function world({ budget = "2/3,1/2" } = {}) {
  const w = {
    now: 1_000_000,
    calls: [],
    answers: [],
    storage: new FakeStorage(),
  };
  const env = {
    ...denylistEnv(),
    HOSTTHIS_CF_PURGE_TOKEN: "token",
    HOSTTHIS_CF_ZONE_ID: "zone",
    HOSTTHIS_APEX_DOMAIN: "example.test",
    HOSTTHIS_PURGE_BUDGET: budget,
  };
  env.PURGE = { idFromName: (name) => name, get: () => ({ fetch: (req) => w.cell.fetch(req) }) };
  w.env = env;
  w.cell = new Purge(state(w.storage), env);
  w.cell.now = () => w.now;
  w.cell.fetcher = async (url, init) => {
    w.calls.push({ at: w.now, url, hosts: JSON.parse(init.body).hosts, auth: init.headers.Authorization });
    const next = w.answers.shift() ?? 200;
    if (next === "throw") throw new Error("no route");
    const [status, headers] = Array.isArray(next) ? next : [next, {}];
    return new Response(status === 200 ? "{}" : "refused", { status, headers });
  };
  w.enqueue = async (slugs, urgent = false) => {
    const res = await w.cell.fetch(new Request("https://cell/purge/enqueue", { method: "POST", body: JSON.stringify({ slugs, urgent }) }));
    return { status: res.status, ...(await res.json()) };
  };
  // Moves the clock to each alarm in turn and runs it, up to `until`.
  w.run = async (until) => {
    for (;;) {
      const at = await w.storage.getAlarm();
      if (at === null || at > until) break;
      w.now = Math.max(w.now, at);
      w.storage.alarm = null;
      await w.cell.alarm();
    }
    w.now = Math.max(w.now, until);
  };
  w.status = async () => (await purgeStatus(env, w.now)).json();
  return w;
}

const hosts = (w) => w.calls.map((c) => c.hosts);

test("a purge goes out at once, to the slug's host, and again after the fill window", async () => {
  const w = world();
  assert.deepEqual(await w.enqueue(["aaaa1111"]), { status: 200, configured: true, queued: 1, sent: true });
  assert.equal(w.calls[0].url, "https://api.cloudflare.com/client/v4/zones/zone/purge_cache");
  assert.equal(w.calls[0].auth, "Bearer token");
  assert.deepEqual(hosts(w), [["aaaa1111.example.test"]]);
  const sentAt = w.now;
  await w.run(sentAt + MINUTE);
  assert.deepEqual(hosts(w), [["aaaa1111.example.test"], ["aaaa1111.example.test"]]);
  assert.equal(w.calls[1].at, sentAt + SECOND_PURGE_MS + SECOND_SLACK_MS);
  assert.equal(await w.storage.getAlarm(), null, "nothing left to send");
  assert.deepEqual((await w.status()).pending, 0);
});

test("a burst shares calls: nothing goes out within the gap of the last call", async () => {
  const w = world();
  await w.enqueue(["aaaa1111"]);
  assert.equal((await w.enqueue(["bbbb2222"])).sent, false);
  assert.equal((await w.enqueue(["cccc3333"])).sent, false);
  assert.equal(w.calls.length, 1);
  await w.run(w.now + CALL_GAP_MS);
  assert.deepEqual(hosts(w)[1], ["bbbb2222.example.test", "cccc3333.example.test"]);
});

test("a rate-limited purge stays queued and goes out when the budget refills", async () => {
  const w = world();
  w.answers = [429, 429, 429];
  const started = w.now;
  assert.equal((await w.enqueue(["aaaa1111"])).sent, false);
  await w.run(started + 10 * MINUTE);
  const accepted = w.calls.findIndex((c, i) => i >= 3);
  assert.equal(accepted, 3, "three refusals, then the call that went through");
  assert.deepEqual(w.calls[3].hosts, ["aaaa1111.example.test"]);
  // Each retry waits for a whole token at 2 a minute.
  assert.deepEqual(w.calls.slice(0, 4).map((c) => c.at - started), [0, 30_000, 60_000, 90_000]);
  assert.equal((await w.status()).pending, 0);
});

test("no refusal drops a purge: server errors, rejected credentials and no answer are all retried", async () => {
  const w = world({ budget: "60/60,60/60" });
  w.answers = [500, 403, "throw", 400];
  const started = w.now;
  await w.enqueue(["aaaa1111"]);
  await w.run(started + 10 * MINUTE);
  assert.deepEqual(w.calls.slice(0, 5).map((c) => c.at - started), [0, 1, 2, 3, 4].map((n) => n * FAILURE_WAIT_MS));
  assert.equal((await w.status()).pending, 0);
});

test("a refusal's Retry-After is honored", async () => {
  const w = world({ budget: "60/60,60/60" });
  w.answers = [[429, { "Retry-After": "45" }]];
  const started = w.now;
  await w.enqueue(["aaaa1111"]);
  await w.run(started + 5 * MINUTE);
  assert.equal(w.calls[1].at - started, 45_000);
});

test("urgent purges go first, and have a budget ordinary purges cannot spend", async () => {
  const w = world({ budget: "1/2,1/1" });
  // Two ordinary calls spend the ordinary burst.
  await w.enqueue(["aaaa1111"]);
  w.now += CALL_GAP_MS;
  await w.enqueue(["bbbb2222"]);
  w.now += CALL_GAP_MS;
  assert.equal((await w.enqueue(["cccc3333"])).sent, false, "the ordinary budget is spent");
  assert.equal(w.calls.length, 2);
  // An urgent purge still goes at once, and what waits rides with it, after it.
  assert.equal((await w.enqueue(["zzzz9999"], true)).sent, true);
  assert.deepEqual(hosts(w)[2], ["zzzz9999.example.test", "cccc3333.example.test"]);
});

test("a full call takes the urgent and the oldest; the rest follow", async () => {
  const w = world({ budget: "60/60,60/60" });
  const many = Array.from({ length: PURGE_HOSTS_MAX }, (_, i) => `s${String(i).padStart(4, "0")}`);
  w.answers = [429];
  await w.enqueue(many.slice(0, 60));
  await w.enqueue(many.slice(60));
  w.now += 10;
  await w.enqueue(["late0001"]);
  await w.enqueue(["urgent01"], true);
  w.calls.length = 0;
  await w.run(w.now + 5_000);
  assert.equal(w.calls[0].hosts.length, PURGE_HOSTS_MAX);
  assert.equal(w.calls[0].hosts[0], "urgent01.example.test");
  assert.ok(!w.calls[0].hosts.includes("late0001.example.test"));
  // The urgent slug took the place of the newest of the first hundred.
  assert.deepEqual(w.calls[1].hosts, ["s0099.example.test", "late0001.example.test"]);
});

test("a slug queued twice is purged once, and a second ask makes it urgent", async () => {
  const w = world({ budget: "1/1,1/1" });
  await w.enqueue(["aaaa1111"]);
  w.now += CALL_GAP_MS;
  await w.enqueue(["bbbb2222"]);
  await w.enqueue(["bbbb2222"]);
  assert.equal((await w.status()).pending, 1);
  assert.equal((await w.enqueue(["bbbb2222"], true)).sent, true, "now urgent, so the urgent budget pays");
});

test("a change during the fill window pushes the second purge after the newest first one", async () => {
  const w = world({ budget: "60/60,60/60" });
  await w.enqueue(["aaaa1111"]);
  w.now += 10_000;
  await w.enqueue(["aaaa1111"]);
  const second = w.now;
  await w.run(second + MINUTE);
  assert.deepEqual(w.calls.map((c) => c.at - second), [-10_000, 0, SECOND_PURGE_MS + SECOND_SLACK_MS]);
});

test("the queue survives the cell's restart", async () => {
  const w = world();
  w.answers = [429];
  await w.enqueue(["aaaa1111"]);
  const again = new Purge(state(w.storage), w.env);
  again.now = () => w.now;
  again.fetcher = w.cell.fetcher;
  w.cell = again;
  await w.run(w.now + 2 * MINUTE);
  assert.deepEqual(hosts(w)[1], ["aaaa1111.example.test"]);
});

test("a malformed enqueue is refused, and a full queue refuses new slugs", async () => {
  const w = world();
  for (const slugs of [undefined, "aaaa1111", ["UPPER"], ["a.b"], [""], [7], Array.from({ length: PURGE_HOSTS_MAX + 1 }, (_, i) => `s${i}`)]) {
    assert.equal((await w.enqueue(slugs)).status, 400, JSON.stringify(slugs)?.slice(0, 40));
  }
  w.answers = Array(200).fill(429);
  for (let i = 0; i < QUEUE_MAX; i += PURGE_HOSTS_MAX) {
    const res = await w.enqueue(Array.from({ length: PURGE_HOSTS_MAX }, (_, j) => `q${i + j}`));
    assert.equal(res.status, 200);
  }
  const full = await w.enqueue(["overflow"]);
  assert.equal(full.status, 507);
  assert.equal(full.error, "purge-queue-full");
  assert.equal((await w.enqueue(["q0"], true)).status, 200, "a slug already queued is still accepted");
});

test("a deployment with no purge credential queues nothing", async () => {
  const w = world();
  delete w.env.HOSTTHIS_CF_PURGE_TOKEN;
  assert.equal(purgeConfig(w.env), null);
  assert.deepEqual(await enqueuePurge(w.env, ["aaaa1111"]), { configured: false, queued: 0, sent: false });
  assert.deepEqual(await w.enqueue(["aaaa1111"]), { status: 200, configured: false, queued: 0, sent: false });
  assert.equal(w.calls.length, 0);
  assert.equal((await w.status()).configured, false);
});

test("enqueuePurge reports what the cell did and throws when it refuses", async () => {
  const w = world();
  assert.deepEqual(await enqueuePurge(w.env, ["aaaa1111"], { urgent: true }), { configured: true, queued: 1, sent: true });
  await assert.rejects(enqueuePurge(w.env, ["NOT A SLUG"]), /status 400/);
});

test("the budget setting is parsed, and a malformed one falls back to the default", () => {
  const env = { HOSTTHIS_CF_PURGE_TOKEN: "t", HOSTTHIS_CF_ZONE_ID: "z", HOSTTHIS_APEX_DOMAIN: "example.test" };
  assert.deepEqual(purgeConfig({ ...env, HOSTTHIS_PURGE_BUDGET: "2/12, 0.5/4" }).budget,
    { ordinary: { perMinute: 2, burst: 12 }, urgent: { perMinute: 0.5, burst: 4 } });
  const fallback = purgeConfig(env).budget;
  for (const bad of ["2/12", "2/12,1/4,1/1", "0/5,1/1", "2/0,1/1", "x"]) {
    assert.deepEqual(purgeConfig({ ...env, HOSTTHIS_PURGE_BUDGET: bad }).budget, fallback, bad);
  }
});

test("the status names the longest wait and is read without the cell", async () => {
  const w = world();
  let cellCalls = 0;
  const get = w.env.PURGE.get;
  w.env.PURGE.get = () => { cellCalls++; return get(); };
  w.answers = [429];
  const queued = w.now;
  await w.enqueue(["aaaa1111"]);
  cellCalls = 0;
  w.now += 5_000;
  const status = await w.status();
  assert.equal(status.pending, 1);
  assert.equal(status.oldestAt, queued);
  assert.equal(cellCalls, 0, "the snapshot answered");
  // A snapshot that cannot be used, or is old, is rebuilt from the cell.
  const [key] = [...w.env.PAYLOADS.objects.keys()];
  w.env.PAYLOADS.objects.set(key, "{");
  assert.equal((await w.status()).pending, 1);
  assert.equal(cellCalls, 1);
  w.now += STATUS_MAX_AGE_MS;
  w.storage.alarm = null;
  assert.equal((await w.status()).oldestAt, queued);
  assert.equal(cellCalls, 2);
});
