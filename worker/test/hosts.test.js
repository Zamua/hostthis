import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import worker from "../dist/index.js";
import { isHostname, normalizeHost, withinApex } from "../src/public/host.js";

const APEX = "example.com";
const TOKEN = "s3cr3t-token_value.~+/=";
const AUTH = { Authorization: `Bearer ${TOKEN}` };
const FORWARDED = "X-Hostthis-Forwarded-Host";
const UPLOAD = "a".repeat(32);
const ROOM = "abc12345%7C11111111-1111-4111-8111-111111111111";
const BINDINGS = ["IDENTITY", "PASTES", "ROOMS", "SUBNETS", "INTENT_LOGS", "PAYLOADS"];

// Bindings that record every method called on them. A cell answers 200 and the
// bucket holds nothing, so a routed request is told apart by its calls.
function env(vars, calls) {
  const binding = (name) => new Proxy({}, {
    get(_target, prop) {
      if (typeof prop !== "string" || prop === "then") return undefined;
      return (...args) => {
        calls.push(`${name}.${prop}`);
        if (prop === "idFromName") return { name: args[0] };
        if (prop === "get" && name !== "PAYLOADS") return { fetch: async () => new Response("cell\n") };
        if (prop === "list") return { objects: [], truncated: false };
        return null;
      };
    },
  });
  const out = { ...vars };
  for (const name of BINDINGS) out[name] = binding(name);
  return out;
}

function call(e, url, { method = "GET", headers = {} } = {}) {
  const init = { method, headers };
  if (method === "POST" || method === "PUT") {
    init.body = "{}";
    init.duplex = "half";
  }
  return worker.fetch(new Request(url, init), e);
}

async function assertPublicNotFound(res, what) {
  assert.equal(res.status, 404, what);
  assert.equal(res.headers.get("Content-Type"), "text/plain; charset=utf-8", what);
  assert.equal(res.headers.get("X-Content-Type-Options"), "nosniff", what);
  assert.equal(await res.text(), "404 page not found\n", what);
}

// Every op a cell answers, read from the op tables, so a new op is covered
// without editing this file.
function cellOps() {
  const src = fileURLToPath(new URL("../src", import.meta.url));
  const text = readdirSync(src, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".js"))
    .map((entry) => readFileSync(join(entry.parentPath, entry.name), "utf8"))
    .join("\n");
  const ops = new Set();
  for (const table of text.matchAll(/^const (?:[A-Z]+_OPS|CELL_ROUTES) = \{\n([\s\S]*?)\n\};/gm)) {
    for (const key of table[1].matchAll(/^ {2}(\w+):/gm)) ops.add(key[1]);
  }
  return [...ops];
}

test("host normalization lowercases and drops the port and a trailing dot", () => {
  const cases = [
    ["example.com", "example.com"],
    ["Example.COM", "example.com"],
    ["example.com.", "example.com"],
    ["example.com:8443", "example.com"],
    ["Example.COM.:443", "example.com"],
    [" example.com ", "example.com"],
    ["[::1]:8080", "[::1]"],
    ["[::1]", "[::1]"],
    ["", ""],
  ];
  for (const [input, want] of cases) assert.equal(normalizeHost(input), want, JSON.stringify(input));
  assert.ok(withinApex("example.com", APEX));
  assert.ok(withinApex("a.b.example.com", APEX));
  assert.ok(!withinApex("xexample.com", APEX));
  assert.ok(!withinApex("example.com.evil.org", APEX));
  assert.ok(isHostname("abc12345.example.com"));
  assert.ok(!isHostname("a..example.com"));
  assert.ok(!isHostname("abc/1.example.com"));
});

test("without an apex, the host and the forwarded header change nothing", async () => {
  for (const apex of [undefined, ""]) {
    for (const token of [undefined, TOKEN]) {
      const vars = {};
      if (apex !== undefined) vars.HOSTTHIS_APEX_DOMAIN = apex;
      if (token !== undefined) vars.HOSTTHIS_WORKER_TOKEN = token;
      for (const host of ["worker.internal", APEX, `abc12345.${APEX}`]) {
        const what = `apex ${JSON.stringify(apex)} token ${!!token} host ${host}`;
        const headers = { [FORWARDED]: `abc12345.${APEX}`, ...(token ? AUTH : {}) };

        const calls = [];
        const res = await call(env(vars, calls), `http://${host}/paste/get?slug=abc12345`, { headers });
        assert.equal(res.status, 200, what);
        assert.equal(await res.text(), "cell\n", what);
        assert.ok(calls.includes("PASTES.get"), what);

        const health = await call(env(vars, []), `http://${host}/healthz`);
        assert.equal(await health.text(), "ok\n", what);

        if (token) {
          const denied = [];
          const res = await call(env(vars, denied), `http://${host}/paste/get?slug=abc12345`, {
            headers: { [FORWARDED]: `abc12345.${APEX}` },
          });
          assert.equal(res.status, 401, what);
          assert.deepEqual(denied, [], what);
        }
      }
    }
  }
});

test("every host at or under the apex reaches the public router", async () => {
  const apexes = [APEX, "Example.COM", "example.com.", "EXAMPLE.com.:8443"];
  const hosts = [
    APEX,
    "EXAMPLE.COM",
    "example.com.",
    "example.com:8443",
    "Example.Com.:443",
    `abc12345.${APEX}`,
    "ABC12345.Example.Com:8080",
    `www.${APEX}`,
    `a.b.${APEX}`,
    `x.${APEX}.`,
    "X.EXAMPLE.COM.:1",
  ];
  for (const apex of apexes) {
    for (const token of [undefined, TOKEN]) {
      const vars = { HOSTTHIS_APEX_DOMAIN: apex };
      if (token) vars.HOSTTHIS_WORKER_TOKEN = token;
      for (const host of hosts) {
        const calls = [];
        const res = await call(env(vars, calls), `http://${host}/`);
        await assertPublicNotFound(res, `apex ${apex} host ${host}`);
        assert.deepEqual(calls, [], `apex ${apex} host ${host}`);
      }
    }
  }
});

test("hosts outside the apex stay on the internal router", async () => {
  const vars = { HOSTTHIS_APEX_DOMAIN: APEX, HOSTTHIS_WORKER_TOKEN: TOKEN };
  for (const host of ["worker.internal", "127.0.0.1:8787", "xexample.com", `${APEX}.evil.org`, "example.org"]) {
    const calls = [];
    const res = await call(env(vars, calls), `http://${host}/paste/get?slug=abc12345`, { headers: AUTH });
    assert.equal(res.status, 200, host);
    assert.ok(calls.includes("PASTES.get"), host);

    const denied = [];
    const unauthorized = await call(env(vars, denied), `http://${host}/paste/get?slug=abc12345`);
    assert.equal(unauthorized.status, 401, host);
    assert.deepEqual(denied, [], host);

    const health = await call(env(vars, []), `http://${host}/healthz`);
    assert.equal(await health.text(), "ok\n", host);
  }
});

test("a public request never reaches the internal router, a cell or the bucket", async () => {
  const ops = cellOps();
  assert.ok(ops.length >= 40 && ops.includes("get") && ops.includes("image"), `op tables not read: ${ops}`);
  const paths = [
    "/",
    "/healthz",
    "/paste/get?slug=abc12345",
    `/blob/uploads/${UPLOAD}/0`,
    `/blob?prefix=uploads/${UPLOAD}/`,
    "/__cell/image",
    "/__cell/restore",
    "/paste/__cell/image?slug=abc12345",
    `/room/join?room=${ROOM}`,
    ...ops.flatMap((op) => [
      `/?op=${op}`,
      `/abc?op=${op}`,
      `/paste/${op}?slug=abc12345`,
      `/identity/${op}?scope=owner1`,
      `/room/${op}?room=${ROOM}`,
      `/subnet/${op}?subnet=10.0.0.0`,
      `/__cell/${op}`,
    ]),
  ];
  const headers = { ...AUTH, [FORWARDED]: `abc12345.${APEX}`, Upgrade: "websocket", Connection: "Upgrade" };
  for (const token of [undefined, TOKEN]) {
    const vars = { HOSTTHIS_APEX_DOMAIN: APEX };
    if (token) vars.HOSTTHIS_WORKER_TOKEN = token;
    for (const host of [APEX, `abc12345.${APEX}`]) {
      for (const path of paths) {
        for (const method of ["GET", "HEAD", "POST", "PUT", "DELETE"]) {
          const what = `${method} ${host}${path}`;
          const calls = [];
          const res = await call(env(vars, calls), `http://${host}${path}`, { method, headers });
          assert.equal(res.status, path === "/healthz" ? 200 : 404, what);
          assert.deepEqual(calls, [], what);
        }
      }
    }
  }
});

test("an apex without a token closes the internal router", async () => {
  const paths = ["/healthz", "/paste/get?slug=abc12345", "/identity/list?scope=owner1", `/blob/uploads/${UPLOAD}/0`, "/"];
  for (const token of [undefined, ""]) {
    const vars = { HOSTTHIS_APEX_DOMAIN: APEX };
    if (token !== undefined) vars.HOSTTHIS_WORKER_TOKEN = token;
    for (const host of ["worker.internal", "127.0.0.1:8787", "example.org"]) {
      for (const path of paths) {
        for (const headers of [{}, AUTH, { ...AUTH, [FORWARDED]: `abc12345.${APEX}` }]) {
          const what = `token ${JSON.stringify(token)} ${host}${path} ${Object.keys(headers)}`;
          const calls = [];
          const res = await call(env(vars, calls), `http://${host}${path}`, { headers });
          assert.equal(res.status, 503, what);
          assert.deepEqual(calls, [], what);
        }
      }
    }
  }
});

test("a forwarded request reaches the public router only behind the token", async () => {
  const vars = { HOSTTHIS_APEX_DOMAIN: APEX, HOSTTHIS_WORKER_TOKEN: TOKEN };
  const origin = "http://worker.internal";
  const paths = ["/", "/healthz", "/paste/get?slug=abc12345", "/?op=put", `/blob/uploads/${UPLOAD}/0`, "/__cell/image"];

  for (const forwarded of [`abc12345.${APEX}`, "ABC12345.Example.COM.:443", APEX, `www.${APEX}`, `a.b.${APEX}`]) {
    for (const path of paths) {
      const what = `${forwarded} ${path}`;
      const calls = [];
      const res = await call(env(vars, calls), origin + path, { headers: { ...AUTH, [FORWARDED]: forwarded } });
      if (path === "/healthz") {
        assert.equal(await res.text(), "ok\n", what);
      } else {
        await assertPublicNotFound(res, what);
      }
      assert.deepEqual(calls, [], what);
    }
  }

  const outside = [
    "example.org",
    `abc12345.${APEX}.evil.org`,
    "xexample.com",
    "",
    "a..example.com",
    ".example.com",
    "abc/12345.example.com",
    "abc_1.example.com",
    "[::1]",
  ];
  for (const forwarded of outside) {
    const calls = [];
    const res = await call(env(vars, calls), `${origin}/paste/get?slug=abc12345`, {
      headers: { ...AUTH, [FORWARDED]: forwarded },
    });
    assert.equal(res.status, 400, JSON.stringify(forwarded));
    assert.deepEqual(calls, [], JSON.stringify(forwarded));
  }

  for (const authorization of [undefined, `Bearer ${TOKEN}x`, "Bearer "]) {
    const headers = { [FORWARDED]: `abc12345.${APEX}` };
    if (authorization !== undefined) headers.Authorization = authorization;
    const calls = [];
    const res = await call(env(vars, calls), `${origin}/paste/get?slug=abc12345`, { headers });
    assert.equal(res.status, 401, String(authorization));
    assert.deepEqual(calls, [], String(authorization));
    const health = await call(env(vars, []), `${origin}/healthz`, { headers });
    assert.equal(await health.text(), "ok\n", String(authorization));
  }

  const calls = [];
  const res = await call(env(vars, calls), `${origin}/paste/get?slug=abc12345`, { headers: AUTH });
  assert.equal(res.status, 200);
  assert.ok(calls.includes("PASTES.get"));
});
