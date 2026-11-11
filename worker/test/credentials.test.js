import assert from "node:assert/strict";
import test from "node:test";

import { ROUTES as ABUSE_ROUTES } from "../src/abuse-routes.js";
import { SCOPED_OPS } from "../src/auth.js";
import { DENYLIST_OPS } from "../src/cells/denylist.js";
import { IDENTITY_OPS } from "../src/cells/identity.js";
import { PASTE_OPS } from "../src/cells/paste.js";
import { ROOM_OPS } from "../src/cells/room.js";
import worker from "../src/index.js";

const ORIGIN = "http://worker.internal";
const FULL = "full-token";
const NEXT = "next-token";
const ADMIN = "admin-token";
const READ = "read-token";
const KEY = `uploads/${"a".repeat(32)}/0`;

// Cell namespaces that record each routed request and answer an empty JSON 200.
function env(extra = {}, calls = []) {
  const ns = (name) => ({
    idFromName: (id) => ({ name: id }),
    get: () => ({
      async fetch(request) {
        calls.push(`${name} ${new URL(request.url).pathname}`);
        return Response.json({});
      },
    }),
  });
  const payloads = {
    async get() { calls.push("PAYLOADS get"); return null; },
    async put() { calls.push("PAYLOADS put"); },
    async list() { calls.push("PAYLOADS list"); return { objects: [], truncated: false }; },
    async delete() { calls.push("PAYLOADS delete"); },
  };
  return {
    IDENTITY: ns("IDENTITY"), PASTES: ns("PASTES"), ROOMS: ns("ROOMS"), SUBNETS: ns("SUBNETS"),
    DENYLIST: ns("DENYLIST"), ABUSE: ns("ABUSE"), TURN: ns("TURN"), PAYLOADS: payloads,
    HOSTTHIS_WORKER_TOKEN: FULL, ...extra,
  };
}

async function call(e, path, { method = "GET", token, headers = {}, body } = {}) {
  const h = new Headers(headers);
  if (token !== undefined) h.set("Authorization", `Bearer ${token}`);
  const res = await worker.fetch(new Request(ORIGIN + path, { method, headers: h, body }), e);
  return { status: res.status, text: await res.text(), authenticate: res.headers.get("WWW-Authenticate") };
}

const REFUSED = { status: 401, text: "unauthorized\n", authenticate: "Bearer" };

test("the next token is accepted beside the current one, and only while it is set", async () => {
  const both = env({ HOSTTHIS_WORKER_TOKEN_NEXT: NEXT });
  assert.equal((await call(both, "/paste/put?slug=abc12345", { method: "POST", token: FULL, body: "{}" })).status, 200);
  assert.equal((await call(both, "/paste/put?slug=abc12345", { method: "POST", token: NEXT, body: "{}" })).status, 200);
  assert.deepEqual(await call(env(), "/paste/get?slug=abc12345", { token: NEXT }), REFUSED);
  // An empty or unset slot never matches an empty bearer.
  for (const e of [env(), env({ HOSTTHIS_WORKER_TOKEN_NEXT: "" })]) {
    assert.deepEqual(await call(e, "/paste/get?slug=abc12345", { headers: { Authorization: "Bearer " } }), REFUSED);
  }
});

test("with an address list the full token works only from a listed address, by the configured source alone", async () => {
  const listed = { HOSTTHIS_INTERNAL_ALLOW: "203.0.113.10, 198.51.100.0/24,2001:db8:1::/48", HOSTTHIS_WORKER_TOKEN_NEXT: NEXT };
  const edge = env({ ...listed, HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" });
  const path = "/paste/get?slug=abc12345";
  for (const address of ["203.0.113.10", "198.51.100.77", "2001:db8:1:ffff::9", "::ffff:203.0.113.10"]) {
    for (const token of [FULL, NEXT]) {
      assert.equal((await call(edge, path, { token, headers: { "CF-Connecting-IP": address } })).status, 200, address);
    }
  }
  // Off the list, no address, or a second header a client could add: the same
  // answer as a wrong token.
  const wrong = await call(edge, path, { token: "not-the-token", headers: { "CF-Connecting-IP": "203.0.113.10" } });
  assert.deepEqual(wrong, REFUSED);
  for (const headers of [
    { "CF-Connecting-IP": "203.0.113.11" },
    { "CF-Connecting-IP": "198.51.101.1" },
    { "CF-Connecting-IP": "2001:db8:2::1" },
    { "CF-Connecting-IP": "not an address" },
    {},
    { "X-Forwarded-For": "203.0.113.10" },
    { "X-Hostthis-Client-IP": "203.0.113.10" },
  ]) {
    const calls = [];
    const res = await call(env({ ...listed, HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" }, calls), path, { token: FULL, headers });
    assert.deepEqual(res, wrong, JSON.stringify(headers));
    assert.deepEqual(calls, []);
  }

  // A runtime with no trusted edge names no source (or another one): there a
  // client controls CF-Connecting-IP, so a forged one must not open the list.
  for (const source of [undefined, "", "xff-rightmost", "something-else"]) {
    const untrusted = env({ ...listed, HOSTTHIS_CLIENT_IP_SOURCE: source });
    assert.deepEqual(await call(untrusted, path, { token: FULL, headers: { "CF-Connecting-IP": "203.0.113.10" } }), wrong, String(source));
  }
  // Behind a proxy the deployment trusts, the right-most forwarded address is the client.
  const proxied = env({ ...listed, HOSTTHIS_CLIENT_IP_SOURCE: "xff-rightmost" });
  assert.equal((await call(proxied, path, { token: FULL, headers: { "X-Forwarded-For": "203.0.113.99, 203.0.113.10" } })).status, 200);
  assert.deepEqual(await call(proxied, path, { token: FULL, headers: { "X-Forwarded-For": "203.0.113.10, 203.0.113.99" } }), wrong);

  // A list that does not parse matches nothing; health stays open.
  const broken = env({ HOSTTHIS_INTERNAL_ALLOW: "203.0.113.10,203.0.113.0/33", HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip" });
  assert.deepEqual(await call(broken, path, { token: FULL, headers: { "CF-Connecting-IP": "203.0.113.10" } }), wrong);
  assert.equal((await call(broken, "/healthz")).status, 200);
});

test("the read token reads and nothing else, from any address", async () => {
  const e = (calls) => env({
    HOSTTHIS_READ_TOKEN: READ, HOSTTHIS_ADMIN_TOKEN: ADMIN,
    HOSTTHIS_INTERNAL_ALLOW: "203.0.113.10", HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip",
  }, calls);
  const from = { "CF-Connecting-IP": "192.0.2.50" };
  const allowed = [
    ["GET", "/paste/get?slug=abc12345"], ["GET", "/paste/operatorrow?slug=abc12345"], ["GET", "/paste/versions?slug=abc12345"],
    ["GET", "/identity/list?scope=key:x"], ["GET", "/identity/subnetlist?scope=key:x&now=1"],
    ["GET", "/denylist/list"], ["GET", "/abuse/list"], ["GET", "/abuse/audit"], ["GET", "/abuse/bans"],
    ["GET", "/room/scan?room=abc12345%7Cr"], ["GET", `/blob/${KEY}`],
  ];
  for (const [method, path] of allowed) {
    const res = await call(e([]), path, { method, token: READ, headers: from });
    assert.notEqual(res.status, 401, `${method} ${path}`);
  }
  const refused = [
    ["POST", "/paste/remove?slug=abc12345"], ["POST", "/paste/put?slug=abc12345"], ["POST", "/paste/quarantine?slug=abc12345"],
    ["GET", "/paste/get?slug=abc12345&op=remove"], ["POST", "/paste/zzz?slug=abc12345&op=remove"],
    ["GET", "/paste/get/extra?slug=abc12345"], ["GET", "/paste/roomcounts?slug=abc12345"],
    ["POST", "/identity/reserve?scope=key:x"], ["GET", "/identity/subnets?scope=key:x"], ["GET", "/identity/podget?scope=key:x"],
    ["POST", "/denylist/add"], ["POST", "/abuse/quarantine"], ["POST", "/abuse/ban-owner"],
    ["POST", "/subnet/admit?subnet=203.0.113.0%2F24"], ["POST", "/turn/issue"],
    ["GET", "/room/pushsublist?room=abc12345%7Cr"], ["POST", "/room/put?room=abc12345%7Cr"], ["GET", "/room/join?room=abc12345%7Cr"],
    ["PUT", `/blob/${KEY}`], ["DELETE", `/blob/${KEY}`], ["DELETE", `/blob?prefix=uploads/${"a".repeat(32)}/`], ["GET", "/blob"],
    ["GET", "/watcher/key-subnets"], ["POST", "/watcher/ban-key"],
  ];
  for (const [method, path] of refused) {
    const calls = [];
    const res = await call(e(calls), path, { method, token: READ, headers: from, body: method === "GET" || method === "DELETE" ? undefined : "{}" });
    assert.deepEqual(res, REFUSED, `${method} ${path}`);
    assert.deepEqual(calls, [], `${method} ${path} reached ${calls[0]}`);
  }
});

test("the admin token adds the abuse actions, the denylist and a takedown's deletions, and no more", async () => {
  const e = (calls) => env({ HOSTTHIS_READ_TOKEN: READ, HOSTTHIS_ADMIN_TOKEN: ADMIN }, calls);
  const allowed = [
    ["GET", "/paste/operatorrow?slug=abc12345"], ["POST", "/paste/remove?slug=abc12345"],
    ["POST", "/denylist/add"], ["POST", "/denylist/remove"], ["GET", "/abuse/bans"],
    ["DELETE", `/blob?prefix=uploads/${"a".repeat(32)}/`], ["GET", `/blob/${KEY}`],
  ];
  for (const [method, path] of allowed) {
    const res = await call(e([]), path, { method, token: ADMIN, body: method === "POST" ? "{}" : undefined });
    assert.notEqual(res.status, 401, `${method} ${path}`);
  }
  // The abuse routes authenticate here and validate their own bodies after.
  for (const path of ["/abuse/quarantine", "/abuse/release", "/abuse/room-delete", "/abuse/ban-owner", "/abuse/unban-owner"]) {
    assert.notEqual((await call(e([]), path, { method: "POST", token: ADMIN, body: "{}" })).status, 401, path);
  }
  const refused = [
    ["POST", "/paste/put?slug=abc12345"], ["POST", "/paste/append?slug=abc12345"], ["POST", "/paste/pin?slug=abc12345"],
    ["POST", "/paste/remove?slug=abc12345&op=put"], ["POST", "/identity/reserve?scope=key:x"], ["POST", "/identity/drop?scope=key:x"],
    ["POST", "/subnet/admit?subnet=203.0.113.0%2F24"], ["POST", "/turn/issue"], ["POST", "/room/put?room=abc12345%7Cr"],
    ["PUT", `/blob/${KEY}`], ["DELETE", `/blob/${KEY}`], ["DELETE", "/blob"],
    ["POST", "/watcher/ban-key"],
  ];
  for (const [method, path] of refused) {
    const calls = [];
    const res = await call(e(calls), path, { method, token: ADMIN, body: method === "POST" || method === "PUT" ? "{}" : undefined });
    assert.deepEqual(res, REFUSED, `${method} ${path}`);
    assert.deepEqual(calls, [], `${method} ${path} reached ${calls[0]}`);
  }
});

test("a narrower token forwards reads to the public surface, and only the full token forwards writes", async () => {
  const e = () => env({ HOSTTHIS_READ_TOKEN: READ, HOSTTHIS_ADMIN_TOKEN: ADMIN, HOSTTHIS_APEX_DOMAIN: "paste.test" });
  const forwarded = { "X-Hostthis-Forwarded-Host": "abc12345.paste.test" };
  for (const token of [READ, ADMIN, FULL]) {
    assert.notEqual((await call(e(), "/", { token, headers: forwarded })).status, 401, token);
  }
  for (const token of [READ, ADMIN]) {
    assert.deepEqual(await call(e(), "/api/rooms", { method: "POST", token, headers: forwarded }), REFUSED, token);
  }
  assert.notEqual((await call(e(), "/api/rooms", { method: "POST", token: FULL, headers: forwarded })).status, 401);
});

test("every scoped operation is an operation of its cell, and every read-scope one is a read there", () => {
  const tables = { paste: PASTE_OPS, identity: IDENTITY_OPS, denylist: DENYLIST_OPS, room: ROOM_OPS };
  for (const [family, ops] of Object.entries(SCOPED_OPS.read)) {
    for (const op of ops) {
      if (family === "abuse") {
        assert.ok(ABUSE_ROUTES.operator[`/abuse/${op}`] && !ABUSE_ROUTES.operator[`/abuse/${op}`].fields, `abuse ${op} is not a read route`);
        continue;
      }
      assert.ok(tables[family][op], `${family} has no op ${op}`);
      assert.ok(!tables[family][op].mutating, `${family}/${op} mutates`);
    }
  }
  for (const [family, ops] of Object.entries(SCOPED_OPS.admin)) {
    for (const op of ops) {
      const known = family === "abuse" ? ABUSE_ROUTES.operator[`/abuse/${op}`] : tables[family][op];
      assert.ok(known, `${family} has no op ${op}`);
    }
  }
});
