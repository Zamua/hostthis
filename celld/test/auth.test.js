import assert from "node:assert/strict";
import test from "node:test";

import worker from "../src/index.js";

const ORIGIN = "http://celld.internal";
const TOKEN = "s3cr3t-token_value.~+/=";

// Cell namespaces that record each forwarded request and answer 200, so a test
// can tell a routed request from one the Worker stopped itself.
function env(token, calls = []) {
  const ns = (name) => ({
    idFromName: (id) => ({ name: id }),
    get: (id) => ({
      async fetch(request) {
        calls.push({ ns: name, id: id.name, path: new URL(request.url).pathname });
        return new Response("cell\n");
      },
    }),
  });
  const payloads = {
    async get() { calls.push({ ns: "PAYLOADS" }); return null; },
    async put() { calls.push({ ns: "PAYLOADS" }); },
    async list() { calls.push({ ns: "PAYLOADS" }); return { objects: [], truncated: false }; },
    async delete() { calls.push({ ns: "PAYLOADS" }); },
  };
  const out = {
    IDENTITY: ns("IDENTITY"),
    PASTES: ns("PASTES"),
    ROOMS: ns("ROOMS"),
    SUBNETS: ns("SUBNETS"),
    PAYLOADS: payloads,
  };
  if (token !== undefined) out.HOSTTHIS_CELLD_TOKEN = token;
  return out;
}

function call(e, path, { method = "GET", authorization, headers = {} } = {}) {
  const h = new Headers(headers);
  if (authorization !== undefined) h.set("Authorization", authorization);
  return worker.fetch(new Request(ORIGIN + path, { method, headers: h }), e);
}

const ROUTES = [
  { path: "/identity/get?scope=owner1", ns: "IDENTITY" },
  { path: "/paste/get?slug=abc12345", ns: "PASTES" },
  { path: "/room/get?room=app12345%2Fr1", ns: "ROOMS" },
  { path: "/subnet/admit?subnet=10.0.0.0", ns: "SUBNETS" },
  { path: `/blob/uploads/${"a".repeat(32)}/0`, ns: "PAYLOADS" },
  { path: `/blob?prefix=uploads/${"a".repeat(32)}/`, ns: "PAYLOADS", method: "DELETE" },
  {
    path: "/room/join?room=app12345%2Fr1",
    ns: "ROOMS",
    headers: { Upgrade: "websocket", Connection: "Upgrade" },
  },
];

test("without a token every route is open, as before", async () => {
  for (const token of [undefined, ""]) {
    for (const route of ROUTES) {
      const calls = [];
      const res = await call(env(token, calls), route.path, route);
      assert.notEqual(res.status, 401, `${route.path} with token ${JSON.stringify(token)}`);
      assert.equal(calls[0]?.ns, route.ns, `${route.path} was not routed`);
    }
  }
});

test("with a token every route refuses a request without it", async () => {
  for (const route of ROUTES) {
    const calls = [];
    const res = await call(env(TOKEN, calls), route.path, route);
    assert.equal(res.status, 401, route.path);
    assert.equal(res.headers.get("WWW-Authenticate"), "Bearer");
    assert.deepEqual(calls, [], `${route.path} reached ${calls[0]?.ns}`);
    assert.ok(!(await res.text()).includes(TOKEN));
  }
});

test("with a token every route refuses a wrong one", async () => {
  const wrong = [
    `Bearer ${TOKEN}x`,
    `Bearer ${TOKEN.slice(0, -1)}`,
    "Bearer ",
    "Bearer",
    `Basic ${TOKEN}`,
    TOKEN,
    `Bearer  ${TOKEN} extra`,
    `Bearer ${TOKEN.toUpperCase()}`,
  ];
  for (const route of ROUTES) {
    for (const authorization of wrong) {
      const calls = [];
      const res = await call(env(TOKEN, calls), route.path, { ...route, authorization });
      assert.equal(res.status, 401, `${route.path} with ${JSON.stringify(authorization)}`);
      assert.deepEqual(calls, []);
    }
  }
});

test("with a token every route serves a request carrying it", async () => {
  for (const authorization of [`Bearer ${TOKEN}`, `bearer ${TOKEN}`, `BEARER  ${TOKEN}`]) {
    for (const route of ROUTES) {
      const calls = [];
      const res = await call(env(TOKEN, calls), route.path, { ...route, authorization });
      assert.notEqual(res.status, 401, `${route.path} with ${JSON.stringify(authorization)}`);
      assert.equal(calls[0]?.ns, route.ns, `${route.path} was not routed`);
    }
  }
});

test("/healthz stays open with a token", async () => {
  const res = await call(env(TOKEN), "/healthz");
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "ok\n");
});

test("a path naming a cell's internal surface is never routed", async () => {
  const paths = [
    "/__cell/image",
    "/__cell/restore",
    "/paste/__cell/image?slug=abc12345",
    "/identity/__cell/restore?scope=owner1",
    "/identity/__cell/adopt?scope=owner1",
    "/room/__cell/name?room=app12345%2Fr1",
    "/subnet/__cell/image?subnet=10.0.0.0",
    "/paste/%5F%5Fcell/image?slug=abc12345",
    "/paste/x/__cell?slug=abc12345",
  ];
  for (const [token, authorization] of [[undefined, undefined], [TOKEN, `Bearer ${TOKEN}`]]) {
    for (const path of paths) {
      for (const method of ["GET", "POST"]) {
        const calls = [];
        const res = await call(env(token, calls), path, { method, authorization });
        assert.equal(res.status, 404, `${method} ${path}`);
        assert.deepEqual(calls, [], `${method} ${path} reached ${calls[0]?.ns}`);
      }
    }
  }
});
