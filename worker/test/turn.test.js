import assert from "node:assert/strict";
import test from "node:test";

import {
  issuedKey, monthOf, READ_EVERY_MS, STUN_SERVER, Turn, TURN_CAPS, TURN_TTL_S, turnConfig,
} from "../src/cells/turn.js";
import { FakeStorage, state } from "./harness.js";

// docs/SPEC.md "TURN relay credentials".

const NOW = Date.parse("2026-11-05T12:00:00Z");
const CONFIG = {
  HOSTTHIS_TURN_KEY_ID: "key1",
  HOSTTHIS_TURN_KEY_TOKEN: "key-token",
  HOSTTHIS_TURN_ACCOUNT_ID: "acct",
  HOSTTHIS_TURN_ANALYTICS_TOKEN: "analytics-token",
};
const TURN_URLS = [
  "turn:turn.cloudflare.com:3478?transport=udp",
  "turn:turn.cloudflare.com:53?transport=udp",
  "turn:turn.cloudflare.com:443?transport=tcp",
  "turns:turn.cloudflare.com:443?transport=tcp",
];
const KEPT_URLS = TURN_URLS.filter((u) => !u.includes(":53"));

// A Turn cell over a fake Cloudflare: `egress` is what analytics reports,
// `fail` names the calls that answer 500, and every call is recorded.
function harness({ env = CONFIG, egress = 0, fail = new Set() } = {}) {
  const calls = [];
  let minted = 0;
  const cf = { egress, fail };
  const storage = new FakeStorage();
  const turnFetch = async (url, init) => {
    calls.push({ url, init, body: init.body ? JSON.parse(init.body) : undefined });
    if (url.endsWith("/graphql")) {
      if (cf.fail.has("read")) return new Response("down", { status: 500 });
      return Response.json({
        data: { viewer: { accounts: [{ callsTurnUsageAdaptiveGroups: [{ dimensions: { keyId: "key1" }, sum: { egressBytes: cf.egress } }] }] } },
        errors: null,
      });
    }
    if (url.endsWith("/generate-ice-servers")) {
      if (cf.fail.has("mint")) return new Response("down", { status: 500 });
      minted++;
      return Response.json({
        iceServers: [
          { urls: ["stun:stun.cloudflare.com:3478"] },
          { urls: TURN_URLS, username: `user${minted}`, credential: `secret${minted}` },
        ],
      }, { status: 201 });
    }
    if (url.endsWith("/revoke")) {
      if (cf.fail.has("revoke")) return new Response("down", { status: 500 });
      return new Response(null, { status: 204 });
    }
    return new Response("unexpected", { status: 404 });
  };
  const cell = new Turn(state(storage), { ...env, TURN_FETCH: turnFetch });
  const issue = async ({ app = "app23456", room = "room-a", now = NOW } = {}) => {
    const res = await cell.fetch(new Request("https://cell/turn/issue", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ app, now, room }),
    }));
    return { status: res.status, body: await res.json() };
  };
  const callsTo = (suffix) => calls.filter((c) => c.url.endsWith(suffix));
  return { cell, cf, calls, callsTo, issue, storage };
}

const issuedKeys = (storage) => [...storage.data.keys()].filter((k) => k.startsWith("i:"));

test("without all four settings the answer is STUN only and Cloudflare is never called", async () => {
  for (const missing of Object.keys(CONFIG)) {
    const env = { ...CONFIG };
    delete env[missing];
    const h = harness({ env });
    assert.deepStrictEqual(await h.issue(), { status: 200, body: { iceServers: [STUN_SERVER] } }, missing);
    assert.equal(h.calls.length, 0, missing);
  }
});

test("a configured cell under budget reads usage, mints, and answers STUN plus TURN with its ttl", async () => {
  const h = harness();
  const got = await h.issue();
  assert.deepStrictEqual(got, {
    status: 200,
    body: { iceServers: [STUN_SERVER, { urls: KEPT_URLS, username: "user1", credential: "secret1" }], ttl: TURN_TTL_S },
  });

  const [read] = h.callsTo("/graphql");
  assert.equal(read.init.headers.Authorization, "Bearer analytics-token");
  assert.deepStrictEqual(read.body.variables, { accountTag: "acct", keyId: "key1", from: "2026-11-01", to: "2026-11-05" });
  assert.match(read.body.query, /callsTurnUsageAdaptiveGroups/);
  assert.match(read.body.query, /egressBytes/);

  const [mint] = h.callsTo("/generate-ice-servers");
  assert.equal(mint.url, "https://rtc.live.cloudflare.com/v1/turn/keys/key1/credentials/generate-ice-servers");
  assert.equal(mint.init.headers.Authorization, "Bearer key-token");
  assert.deepStrictEqual(mint.body, { ttl: TURN_TTL_S });
  assert.equal(mint.init.redirect, "manual");

  // Kept for revocation only, and the alarm watches the budget meanwhile.
  assert.deepStrictEqual(issuedKeys(h.storage), [issuedKey(NOW + TURN_TTL_S * 1000, "user1")]);
  assert.equal(h.storage.alarm, NOW + READ_EVERY_MS);
});

test("usage is read at most once every 5 minutes, and again in a new month", async () => {
  const h = harness();
  await h.issue();
  await h.issue({ now: NOW + READ_EVERY_MS - 1 });
  assert.equal(h.callsTo("/graphql").length, 1);
  await h.issue({ now: NOW + READ_EVERY_MS });
  assert.equal(h.callsTo("/graphql").length, 2);

  const december = Date.parse("2026-12-01T00:00:30Z");
  assert.equal(monthOf(december), "2026-12");
  await h.issue({ now: december });
  assert.deepStrictEqual(h.callsTo("/graphql").at(-1).body.variables, {
    accountTag: "acct", keyId: "key1", from: "2026-12-01", to: "2026-12-01",
  });
});

test("at the budget nothing is issued and every unexpired credential is revoked", async () => {
  const h = harness({ env: { ...CONFIG, HOSTTHIS_TURN_BUDGET_GB: "1" } });
  await h.issue();
  await h.issue({ room: "room-b" });
  // One already expired: forgotten, not revoked.
  await h.storage.put(issuedKey(NOW - 1, "old"), true);

  h.cf.egress = 1e9;
  const got = await h.issue({ now: NOW + READ_EVERY_MS });
  assert.deepStrictEqual(got.body, { iceServers: [STUN_SERVER] });
  assert.deepStrictEqual(h.callsTo("/revoke").map((c) => c.url).sort(), [
    "https://rtc.live.cloudflare.com/v1/turn/keys/key1/credentials/user1/revoke",
    "https://rtc.live.cloudflare.com/v1/turn/keys/key1/credentials/user2/revoke",
  ]);
  assert.equal(h.callsTo("/revoke")[0].init.headers.Authorization, "Bearer key-token");
  assert.deepStrictEqual(issuedKeys(h.storage), []);
  assert.equal(h.callsTo("/generate-ice-servers").length, 2);
});

test("the budget defaults to 900 GB and a zero budget issues nothing", async () => {
  assert.equal(turnConfig(CONFIG).budgetBytes, 900e9);
  assert.equal(turnConfig({ ...CONFIG, HOSTTHIS_TURN_BUDGET_GB: "nope" }).budgetBytes, 900e9);
  assert.equal(turnConfig({ ...CONFIG, HOSTTHIS_TURN_BUDGET_GB: "2.5" }).budgetBytes, 2.5e9);

  const under = harness({ egress: 899e9 });
  assert.equal((await under.issue()).body.iceServers.length, 2);
  const over = harness({ egress: 900e9 });
  assert.deepStrictEqual((await over.issue()).body, { iceServers: [STUN_SERVER] });
  const zero = harness({ env: { ...CONFIG, HOSTTHIS_TURN_BUDGET_GB: "0" } });
  assert.deepStrictEqual((await zero.issue()).body, { iceServers: [STUN_SERVER] });
});

test("without a reading, or with one older than 30 minutes, nothing is issued", async () => {
  const h = harness({ fail: new Set(["read"]) });
  assert.deepStrictEqual((await h.issue()).body, { iceServers: [STUN_SERVER] });
  assert.equal(h.callsTo("/generate-ice-servers").length, 0);

  h.cf.fail.delete("read");
  assert.equal((await h.issue()).body.iceServers.length, 2);

  // Analytics goes down: the last reading serves until it is 30 minutes old.
  h.cf.fail.add("read");
  assert.equal((await h.issue({ now: NOW + 29 * 60 * 1000 })).body.iceServers.length, 2);
  assert.deepStrictEqual((await h.issue({ now: NOW + 30 * 60 * 1000 })).body, { iceServers: [STUN_SERVER] });
});

test("a refused, unreachable or malformed credential request answers STUN only and counts nothing", async () => {
  const h = harness({ fail: new Set(["mint"]) });
  assert.deepStrictEqual((await h.issue()).body, { iceServers: [STUN_SERVER] });
  assert.deepStrictEqual(issuedKeys(h.storage), []);
  assert.equal(h.storage.data.get("window"), undefined);

  const thrown = harness();
  thrown.cell.env.TURN_FETCH = async (url) => {
    if (url.endsWith("/graphql")) {
      return Response.json({ data: { viewer: { accounts: [{ callsTurnUsageAdaptiveGroups: [] }] } } });
    }
    throw new Error("connection reset");
  };
  assert.deepStrictEqual((await thrown.issue()).body, { iceServers: [STUN_SERVER] });

  const only53 = harness();
  only53.cell.env.TURN_FETCH = async (url) => (url.endsWith("/graphql")
    ? Response.json({ data: { viewer: { accounts: [{ callsTurnUsageAdaptiveGroups: [] }] } } })
    : Response.json({ iceServers: [{ urls: ["turn:turn.cloudflare.com:53"], username: "u", credential: "c" }] }, { status: 201 }));
  assert.deepStrictEqual((await only53.issue()).body, { iceServers: [STUN_SERVER] });
});

test("caps: 64 per room, 512 per app and 4096 in total per UTC hour", async () => {
  const h = harness();
  for (let i = 0; i < TURN_CAPS.room; i++) {
    assert.equal((await h.issue()).body.iceServers.length, 2, `credential ${i + 1}`);
  }
  assert.deepStrictEqual((await h.issue()).body, { iceServers: [STUN_SERVER] });
  assert.equal((await h.issue({ room: "room-b" })).body.iceServers.length, 2);

  const nextHour = Math.floor(NOW / 3600000) * 3600000 + 3600000;
  assert.equal((await h.issue({ now: nextHour })).body.iceServers.length, 2);

  // The app and total caps, from a window already at them.
  const hour = Math.floor(NOW / 3600000);
  const app = harness();
  await app.storage.put("window", { hour, total: 0, apps: { app23456: TURN_CAPS.app }, rooms: {} });
  assert.deepStrictEqual((await app.issue()).body, { iceServers: [STUN_SERVER] });
  assert.equal((await app.issue({ app: "other234" })).body.iceServers.length, 2);
  const total = harness();
  await total.storage.put("window", { hour, total: TURN_CAPS.total, apps: {}, rooms: {} });
  assert.deepStrictEqual((await total.issue({ app: "other234" })).body, { iceServers: [STUN_SERVER] });
});

test("the alarm reads the budget, revokes on overrun, forgets expired credentials and stops when none remain", async () => {
  const h = harness({ env: { ...CONFIG, HOSTTHIS_TURN_BUDGET_GB: "1" } });
  await h.issue();
  assert.equal(h.storage.alarm, NOW + READ_EVERY_MS);

  // Under budget: read, keep the live credential, re-arm.
  await h.cell.alarm({ now: NOW + READ_EVERY_MS });
  assert.equal(h.callsTo("/graphql").length, 2);
  assert.equal(issuedKeys(h.storage).length, 1);
  assert.equal(h.storage.alarm, NOW + 2 * READ_EVERY_MS);

  // A failed revocation stays listed for the next run.
  h.cf.egress = 2e9;
  h.cf.fail.add("revoke");
  await h.cell.alarm({ now: NOW + 2 * READ_EVERY_MS });
  assert.equal(issuedKeys(h.storage).length, 1);
  assert.equal(h.storage.alarm, NOW + 3 * READ_EVERY_MS);
  h.cf.fail.delete("revoke");
  await h.cell.alarm({ now: NOW + 3 * READ_EVERY_MS });
  assert.deepStrictEqual(issuedKeys(h.storage), []);
  assert.equal(h.storage.alarm, NOW + 3 * READ_EVERY_MS, "not re-armed with nothing left to watch");

  // Expiry alone empties the list.
  const quiet = harness();
  await quiet.issue();
  await quiet.cell.alarm({ now: NOW + TURN_TTL_S * 1000 });
  assert.deepStrictEqual(issuedKeys(quiet.storage), []);
  assert.equal(quiet.callsTo("/revoke").length, 0);
});

test("a malformed issue request is a 400", async () => {
  const h = harness();
  const res = await h.cell.fetch(new Request("https://cell/turn/issue", {
    method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ app: "a", room: "" }),
  }));
  assert.equal(res.status, 400);
});
