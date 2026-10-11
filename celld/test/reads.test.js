import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";

import { listAssets, REPO } from "../build/shells.mjs";
import { goStringList } from "../src/public/gojson.js";
import { cleanRedirect } from "../src/public/path.js";
import { rawContentType, renderShell } from "../src/public/shells.js";
import { publicReads } from "../src/public/surface.js";
import { INPUT_BATCH } from "../src/public/zstd.js";
import {
  APEX, assertGolden, at, documentRow, FORWARDED, GOLDEN, headersOf, hiRow, sha256, SLUG, TOKEN, UPDATED, world,
} from "./public-world.js";
import { stored } from "./stored.js";

const golden = GOLDEN;
const vectors = JSON.parse(readFileSync(join(REPO, "testdata", "read-vectors.json"), "utf8"));

const SHELL_KINDS = Object.keys(golden).filter((k) => k.startsWith("shell/")).map((k) => k.slice("shell/".length));
const RAW_SOURCE = "# title\n\nsome **body**\n";

// internal/http's buildKBServer.
const KB_FILES = {
  "README.md": ["key-readme", "# notes\n"],
  "guide/setup.md": ["key-setup", "# setup\n"],
  "guide/diagram.png": ["key-png", "\x89PNG"],
  "guide/index.html": ["key-guide-index", "<h1>guide index</h1>"],
  "report.html": ["key-html", "<h1>report</h1>"],
  "files.json": ["key-files", "{\"own\":\"file\"}"],
};

function kbWorld(w) {
  const size = (content) => new TextEncoder().encode(content).length;
  const files = Object.fromEntries(Object.entries(KB_FILES)
    .map(([path, [key, content]]) => [path, { Key: key, Size: size(content), ContentType: "", Kind: "" }]));
  w.row(SLUG, { kind: "knowledgebase", status: "ready", updatedAt: UPDATED, uploadId: "up-kb", manifest: { Files: files } });
  for (const [key, content] of Object.values(KB_FILES)) w.blob(key, content);
}

// internal/http's escapingKBPaths.
const ESCAPING_PATHS = [
  "a<b>&c.md", "line\u2028sep\u2029.md", "quote\"back\\slash.md", "tab\tnl\n.md", "ctl\x01\x1f\b\f.md",
  "del\x7f.md", "\u00e9.md", "\u{1d11e}.md", "\ue000.md", "z.md",
];

test("shells and raw bytes are hostthisd's, byte for byte", async () => {
  assert.ok(SHELL_KINDS.length >= 10);
  for (const kind of SHELL_KINDS) {
    const w = world();
    w.row(SLUG, documentRow(kind));
    w.blob("uploads/a/0", RAW_SOURCE);
    await assertGolden(await w.fetch(at("/")), `shell/${kind}`);
    await assertGolden(await w.fetch(at("/?raw=1")), `raw/${kind}`);
  }
});

test("a knowledge base's shell, documents and file list are hostthisd's", async () => {
  const w = world();
  kbWorld(w);
  for (const [name, path] of [["kb/root", "/"], ["kb/document", "/guide/setup.md"], ["kb/raw", "/guide/setup.md?raw=1"],
    ["kb/files", "/?files=1"]]) {
    await assertGolden(await w.fetch(at(path)), name);
  }
});

test("the file list is json.Encoder's bytes: HTML and line separator escapes, UTF-8 order", async () => {
  const w = world();
  const files = Object.fromEntries(ESCAPING_PATHS.map((path, i) => [path, { Key: `key-${i}`, Size: 1 }]));
  w.row(SLUG, { kind: "knowledgebase", status: "ready", updatedAt: UPDATED, uploadId: "up-escapes", manifest: { Files: files } });
  await assertGolden(await w.fetch(at("/?files")), "kb/files-escapes");
  assert.equal(goStringList(["<&>"]), "[\"\\u003c\\u0026\\u003e\"]\n");
  assert.equal(goStringList(["\ud800x"]), "[\"\\ufffdx\"]\n");
});

test("pending, failed, unknown slugs, redirects and health are hostthisd's", async () => {
  for (const [name, status] of [["status/pending", "pending"], ["status/failed", "failed"]]) {
    const w = world();
    w.row(SLUG, documentRow("markdown", status));
    await assertGolden(await w.fetch(at("/")), name);
  }
  const w = world();
  await assertGolden(await w.fetch(at("/")), "error/notfound");
  await assertGolden(await w.fetch(at("/a//b?raw=1")), "redirect/clean");
  await assertGolden(await w.fetch(at("/a//b"), { method: "HEAD" }), "redirect/head");
  await assertGolden(await w.fetch(at("/_hostthis?v=x")), "redirect/tree");
  await assertGolden(await w.fetch(at("/healthz")), "health/healthz");
  await assertGolden(await w.fetch(at("/readyz")), "health/readyz");
  await assertGolden(await w.fetch(at("//healthz")), "health/redirect");
  // Health answers on every public host, as hostthisd's does on any Host.
  for (const host of [APEX, `www.${APEX}`, `a.b.${APEX}`]) {
    await assertGolden(await w.fetch(`https://${host}/healthz`), "health/healthz");
  }
  assert.deepEqual(w.payloadCalls, []);
});

// Stored bytes for a vector's entry, as long as its size says.
function entryBytes(entry) {
  const text = `bytes of ${entry.key} `;
  return text.repeat(Math.ceil(entry.size / text.length)).slice(0, entry.size);
}

// The answer each shared read vector names, served as hostthisd serves it.
function expectFor(v, row) {
  const want = v.want;
  if (want.found === false || want.action === "notfound") return { status: 404, body: "404 page not found\n" };
  if (want.status === "pending") return { status: 200, golden: "status/pending" };
  if (want.status === "failed") return { status: 410, golden: "status/failed" };
  const validators = (etag) => ({ etag, "last-modified": new Date(row.updatedAt).toUTCString(), "cache-control": "public, no-cache" });
  switch (want.action) {
    case "html":
    case "raw":
    case "file":
    case "kbraw": {
      const types = {
        html: "text/html; charset=utf-8",
        raw: rawContentType(want.kind) ?? "text/plain; charset=utf-8",
        file: want.entry.contentType || "application/octet-stream",
        kbraw: rawContentType("knowledgebase"),
      };
      return {
        status: 200, body: entryBytes(want.entry), blob: want.entry.key, length: String(want.entry.size),
        headers: { ...validators(`"${want.entry.key}"`), "content-type": types[want.action] },
      };
    }
    case "shell":
    case "kbshell": {
      const shell = renderShell(want.shell);
      if (!shell) return { status: 500, body: "unsupported kind\n" };
      return {
        status: 200, body: shell.body,
        headers: { ...validators(`"${shell.version}"`), "content-type": "text/html; charset=utf-8", "content-security-policy": shell.csp },
      };
    }
    case "files":
      return {
        status: 200, json: want.files,
        headers: { ...validators(`"files-${want.uploadId}"`), "content-type": "application/json; charset=utf-8" },
      };
    default:
      throw new Error(`unhandled vector answer ${JSON.stringify(want)}`);
  }
}

function vectorURL(v) {
  const query = [v.request.raw && "raw=1", v.request.files && "files=1"].filter(Boolean).join("&");
  return at(v.request.path.split("/").map(encodeURIComponent).join("/") + (query ? `?${query}` : ""));
}

for (const v of vectors.vectors) {
  test(`read vector through the router: ${v.name}`, async () => {
    const w = world();
    const row = v.row === null ? null : vectors.rows[v.row];
    if (row) {
      w.row(SLUG, row);
    }
    if (v.want.entry?.key) {
      w.blob(v.want.entry.key, entryBytes(v.want.entry));
    }
    const want = expectFor(v, row);
    for (const method of ["GET", "HEAD"]) {
      w.payloadCalls.length = 0;
      const res = await w.fetch(vectorURL(v), { method });
      assert.equal(res.status, want.status, method);
      for (const [name, value] of Object.entries(want.headers ?? {})) {
        assert.equal(res.headers.get(name), value, `${method} ${name}`);
      }
      if (want.status === 200) {
        assert.equal(res.headers.get("x-frame-options"), "DENY");
        assert.equal(res.headers.get("referrer-policy"), "no-referrer");
      }
      const body = await res.text();
      if (method === "HEAD") {
        assert.equal(body, "");
        assert.deepEqual(w.payloadCalls, [], "a HEAD reads no payload");
        continue;
      }
      if (want.golden) await assertGolden(await w.fetch(vectorURL(v)), want.golden);
      else if (want.json) assert.deepEqual(JSON.parse(body), want.json);
      else assert.equal(body, want.body);
      assert.equal(res.headers.get("content-length"), want.length ?? null, "stored bytes carry their size, nothing else does");
      assert.deepEqual(w.payloadCalls, want.blob ? [`get ${want.blob}`] : []);
    }
  });
}

test("a conditional request is answered before any payload is read, comparing weakly", async () => {
  const w = world();
  w.row(SLUG, hiRow());
  w.blob("uploads/a/0", "<h1>hi</h1>");
  const lastModified = new Date(UPDATED).toUTCString();
  const notModified = [
    { "If-None-Match": "\"uploads/a/0\"" },
    { "If-None-Match": "W/\"uploads/a/0\"" },
    { "If-None-Match": "\"other\", W/\"uploads/a/0\"" },
    { "If-None-Match": " * " },
    { "If-Modified-Since": lastModified },
    { "If-Modified-Since": "Sun, 07 Jun 2026 14:00:00.5 GMT" },
    { "If-Modified-Since": "sun, 07 jun 2026 14:00:01 GMT" },
    { "If-Modified-Since": "Sunday, 07-Jun-26 14:00:00 EST" },
    { "If-Modified-Since": "Sun Jun  7 14:00:00 2026" },
    // An empty If-None-Match is none, as Go's Header.Get reads it.
    { "If-None-Match": "", "If-Modified-Since": lastModified },
  ];
  for (const headers of notModified) {
    w.payloadCalls.length = 0;
    const res = await w.fetch(at("/"), { headers });
    assert.equal(res.status, 304, JSON.stringify(headers));
    assert.equal(await res.text(), "");
    assert.equal(res.headers.get("ETag"), "\"uploads/a/0\"");
    assert.equal(res.headers.get("Last-Modified"), lastModified);
    assert.equal(res.headers.get("Cache-Control"), "public, no-cache");
    assert.equal(res.headers.get("Content-Type"), null);
    assert.deepEqual(w.payloadCalls, []);
  }
  const modified = [
    { "If-None-Match": "\"uploads/a/1\"" },
    { "If-Modified-Since": "Sun, 07 Jun 2026 13:59:59 GMT" },
    { "If-Modified-Since": "yesterday" },
    { "If-Modified-Since": "Sun, 31 Jun 2026 14:00:00 GMT" },
    { "If-Modified-Since": "Sun, 07 Jun 2026 14:00:00 UTC" },
    { "If-Modified-Since": "Sun, 7 Jun 2026 14:00:00 GMT" },
    // A present If-None-Match decides alone (RFC 9110 section 13.2.2).
    { "If-None-Match": "\"other\"", "If-Modified-Since": lastModified },
    { "If-None-Match": "W/\"other\", \"uploads/a/1\"", "If-Modified-Since": "Mon, 01 Jan 2035 00:00:00 GMT" },
  ];
  for (const headers of modified) {
    const res = await w.fetch(at("/"), { headers });
    assert.equal(res.status, 200, JSON.stringify(headers));
    assert.equal(await res.text(), "<h1>hi</h1>");
  }
  // A shell's 304 carries the validators and no CSP or Content-Type.
  w.row(SLUG, documentRow("markdown"));
  const shell = renderShell("markdown");
  const res = await w.fetch(at("/"), { headers: { "If-None-Match": `"${shell.version}"` } });
  assert.equal(res.status, 304);
  assert.equal(res.headers.get("Content-Security-Policy"), null);
});

test("reads answer GET and HEAD only, before any cell call", async () => {
  const w = world();
  w.row(SLUG, hiRow());
  w.blob("uploads/a/0", "<h1>hi</h1>");
  for (const method of ["POST", "PUT", "DELETE", "PATCH", "OPTIONS"]) {
    for (const path of ["/", "/x.png", "/_hostthis/md.js"]) {
      const init = { method };
      if (method !== "OPTIONS") init.body = "x";
      const res = await w.fetch(at(path), init);
      assert.equal(res.status, 405, `${method} ${path}`);
      assert.equal(res.headers.get("Allow"), "GET, HEAD");
      assert.equal(await res.text(), "Method Not Allowed\n");
    }
    // Health answers every method, as hostthisd's does.
    assert.equal((await w.fetch(at("/healthz"), { method })).status, 200, method);
  }
  assert.deepEqual(w.cellRequests, []);
  assert.deepEqual(w.payloadCalls, []);
});

test("the path is decoded once and a malformed escape is a 400", async () => {
  const w = world();
  w.row(SLUG, {
    kind: "site", status: "ready", updatedAt: UPDATED, uploadId: "us",
    manifest: { Files: {
      "index.html": { Key: "k-index", Size: 7, ContentType: "text/html; charset=utf-8" },
      "a b/ünï.txt": { Key: "k-text", Size: 6, ContentType: "text/plain; charset=utf-8" },
      "lit%20eral.txt": { Key: "k-literal", Size: 9, ContentType: "text/plain; charset=utf-8" },
    } },
  });
  for (const key of ["k-index", "k-text", "k-literal"]) w.blob(key, key);
  assert.equal(await (await w.fetch(at("/a%20b/%C3%BCn%C3%AF.txt"))).text(), "k-text");
  assert.equal(await (await w.fetch(at("/lit%2520eral.txt"))).text(), "k-literal");
  // The URL parser resolves dot segments before the Worker sees the path.
  assert.equal(await (await w.fetch(at("/x/../a%20b/%C3%BCn%C3%AF.txt"))).text(), "k-text");
  for (const path of ["/%zz", "/a%2", "/%"]) {
    const res = await w.fetch(at(path));
    assert.equal(res.status, 400, path);
    assert.equal(await res.text(), "400 Bad Request");
  }
  const res = await w.fetch(at("/x//y?raw"), { method: "POST" });
  assert.equal(res.status, 307);
  assert.equal(res.headers.get("Location"), "/x/y?raw");
  assert.equal(res.headers.get("Content-Type"), null);
});

// Each request path, and the Location Go's ServeMux answers it with (null: only
// that it stays on the request's host).
const ESCAPED_SEPARATOR_REDIRECTS = [
  ["/%2Fevil.com//", "/%2Fevil.com/"],
  ["/%2F%2Fevil.com//", "/%2F%2Fevil.com/"],
  ["/%2f%2fevil.com//?raw", "/%2f%2fevil.com/?raw"],
  ["//%2Fevil.com", "/%2Fevil.com"],
  ["/%5Cevil.com//", "/%5Cevil.com/"],
  ["/%5C%5Cevil.com//", "/%5C%5Cevil.com/"],
  ["/%2F%5Cevil.com//", "/%2F%5Cevil.com/"],
  ["/%5c/evil.com//", "/%5c/evil.com/"],
  ["/\\evil.com//", "/evil.com/"],
  ["/%2Fevil.com/|//", "/evil.com/%7C/"],
  ["/api/rooms/%2F%2Fevil.com//", "/api/rooms/%2F%2Fevil.com/"],
  ["/api/push/key/%2F%2Fevil.com//x", "/api/push/key/%2F%2Fevil.com/x"],
  ["/_hostthis/%2F%2Fevil.com//", "/_hostthis/%2F%2Fevil.com/"],
  ["/%2F%2Fevil.com/%2E%2E//", null],
];

test("a clean redirect keeps escaped separators escaped and stays on the host, on every public host", async () => {
  const w = world({ HOSTTHIS_URL_MODE: "path" });
  const targets = [
    (path) => [at(path), {}],
    (path) => [`https://${APEX}${path}`, {}],
    (path) => [`https://www.${APEX}${path}`, {}],
    (path) => [`https://a.b.${APEX}${path}`, {}],
    (path) => [`http://celld.internal${path}`, { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${SLUG}.${APEX}` }],
  ];
  for (const [path, want] of ESCAPED_SEPARATOR_REDIRECTS) {
    for (const target of targets) {
      const [url, headers] = target(path);
      for (const method of ["GET", "HEAD"]) {
        const res = await w.fetch(url, { method, headers });
        const location = res.headers.get("Location");
        assert.equal(res.status, 307, `${method} ${url}`);
        assert.doesNotMatch(location, /^\/[/\\]/, `${method} ${url}`);
        assert.equal(new URL(location, url).host, new URL(url).host, `${method} ${url} -> ${location}`);
        if (want !== null) assert.equal(location, want, `${method} ${url}`);
      }
    }
    // Path mode's prefix is part of the path the redirect keeps.
    const res = await w.fetch(`https://${APEX}/p/${SLUG}${path}`);
    assert.equal(new URL(res.headers.get("Location"), `https://${APEX}/`).host, APEX, path);
  }
  assert.deepEqual(w.cellRequests, []);
});

test("a clean redirect escapes a backslash the URL parser left in the path", () => {
  for (const [pathname, want] of [["/\\evil.com//", "/%5Cevil.com/"], ["/\\/evil.com//", "/%5C/evil.com/"]]) {
    assert.equal(cleanRedirect("GET", pathname, "").headers.get("Location"), want, pathname);
  }
});

test("flags parse as Go's URL.Query().Has does", async () => {
  const w = world();
  w.row(SLUG, documentRow("markdown"));
  w.blob("uploads/a/0", RAW_SOURCE);
  const raw = ["?raw", "?raw=", "?raw=0", "?r%61w", "?x=1&raw", "?raw=a+b", "?a=1;b&raw", "?raw&raw"];
  const shell = ["", "?", "?RAW", "?raw;x", "?raw=%zz", "?r%zzaw", "?rawx", `?${"x&".repeat(10000)}raw`];
  for (const query of raw) {
    assert.equal(await (await w.fetch(at(`/${query}`))).text(), RAW_SOURCE, query);
  }
  for (const query of shell) {
    assert.equal(await (await w.fetch(at(`/${query}`))).text(), renderShell("markdown").body, query);
  }
});

test("the cell sees only the parsed slug, path and flags, and nothing changes", async () => {
  const w = world();
  w.row(SLUG, hiRow());
  w.blob("uploads/a/0", "<h1>hi</h1>");
  const before = structuredClone([...w.storageOf(SLUG).data]);
  const opsSource = readFileSync(new URL("../src/index.js", import.meta.url), "utf8");
  const ops = [...opsSource.matchAll(/^ {2}(\w+): (?:read|mutation)\(/gm)].map((m) => m[1]);
  assert.ok(ops.includes("put") && ops.includes("remove") && ops.length > 20, `${ops}`);
  const paths = [
    "/", "/__cell/image", "/__cell/restore", `/blob/uploads/x/0`, "/blob?prefix=uploads/x/",
    ...ops.flatMap((op) => [`/?op=${op}`, `/paste/${op}?slug=${SLUG}`, `/x?op=${op}&slug=other&path=/x&raw=1`]),
  ];
  const targets = [
    (path) => [at(path), {}],
    (path) => [`http://${APEX}/p/${SLUG}${path}`, {}],
    (path) => [`http://celld.internal${path}`, { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${SLUG}.${APEX}` }],
  ];
  const env = { HOSTTHIS_URL_MODE: "path" };
  Object.assign(w.env, env);
  for (const target of targets) {
    for (const path of paths) {
      for (const method of ["GET", "HEAD", "POST", "DELETE"]) {
        const [url, headers] = target(path);
        const init = { method, headers };
        if (method === "POST") init.body = "{\"slug\":\"other\"}";
        await (await w.fetch(url, init)).arrayBuffer();
      }
    }
  }
  assert.ok(w.cellRequests.length > 0);
  for (const r of w.cellRequests) {
    const url = new URL(r.url);
    assert.equal(r.name, SLUG);
    assert.equal(r.method, "GET");
    assert.equal(r.body, null);
    assert.equal(url.pathname, "/paste/resolve");
    assert.deepEqual([...url.searchParams.keys()].filter((k) => !["raw", "files"].includes(k)), ["slug", "path"]);
    assert.equal(url.searchParams.get("slug"), SLUG);
  }
  assert.deepEqual(structuredClone([...w.storageOf(SLUG).data]), before);
  assert.ok(w.payloadCalls.every((c) => c === "get uploads/a/0"), `${w.payloadCalls}`);
});

test("path mode serves /p/<slug> on the apex host only", async () => {
  const w = world({ HOSTTHIS_URL_MODE: "path" });
  w.row(SLUG, {
    kind: "site", status: "ready", updatedAt: UPDATED, uploadId: "us",
    manifest: { Files: { "index.html": { Key: "k-index", Size: 5 }, "x/y.txt": { Key: "k-y", Size: 1, ContentType: "text/plain" } } },
  });
  w.blob("k-index", "index");
  w.blob("k-y", "y");
  assert.equal(await (await w.fetch(`http://${APEX}/p/${SLUG}`)).text(), "index");
  assert.equal(await (await w.fetch(`http://${APEX}/p/${SLUG}/`)).text(), "index");
  assert.equal(await (await w.fetch(`http://${APEX}/p/${SLUG}/x/y.txt`)).text(), "y");
  for (const url of [`http://${APEX}/p/ABC23456`, `http://${APEX}/p/`, `http://www.${APEX}/p/${SLUG}`, `http://${APEX}/`]) {
    assert.equal((await w.fetch(url)).status, 404, url);
  }
  const subdomain = world();
  subdomain.row(SLUG, hiRow());
  assert.equal((await subdomain.fetch(`http://${APEX}/p/${SLUG}`)).status, 404);
});

test("a forwarded read answers as the direct one", async () => {
  const w = world();
  w.row(SLUG, documentRow("markdown"));
  w.blob("uploads/a/0", RAW_SOURCE);
  for (const path of ["/", "/?raw=1", "/missing.png"]) {
    const direct = await w.fetch(at(path));
    const forwarded = await w.fetch(`http://celld.internal${path}`, {
      headers: { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${SLUG}.${APEX}` },
    });
    assert.equal(forwarded.status, direct.status, path);
    assert.deepEqual(headersOf(forwarded), headersOf(direct), path);
    assert.equal(await forwarded.text(), await direct.text(), path);
  }
  const denied = await w.fetch(`http://celld.internal/`, { headers: { [FORWARDED]: `${SLUG}.${APEX}` } });
  assert.equal(denied.status, 401);
});

async function capturingWarnings(run) {
  const warned = [];
  const original = console.warn;
  console.warn = (...args) => warned.push(args.join(" "));
  try {
    await run();
  } finally {
    console.warn = original;
  }
  return warned;
}

// A read-path 5xx is bare, as hostthisd's: its text, and nothing a cache keeps.
const BARE_500 = { "content-type": "text/plain; charset=utf-8", "x-content-type-options": "nosniff", "cache-control": "no-store" };

async function assertInternalError(res) {
  assert.equal(res.status, 500);
  assert.equal(await res.text(), "internal error\n");
  assert.deepEqual(headersOf(res), BARE_500);
}

test("a payload that is missing or damaged before the first byte is a 500 with one warning", async () => {
  const damaged = stored("<h1>hi</h1>");
  damaged[0] = 0x58;
  for (const [what, setup] of [
    ["missing", () => {}],
    ["bad magic", (w) => w.object("uploads/a/0", damaged)],
    ["empty object", (w) => w.object("uploads/a/0", new Uint8Array(0))],
    ["magic only", (w) => w.object("uploads/a/0", stored("").subarray(0, 4))],
    ["not a zstd frame", (w) => w.object("uploads/a/0", Uint8Array.of(...stored("").subarray(0, 4), 1, 2, 3, 4, 5, 6, 7, 8))],
    ["a window over 8 MiB", (w) => w.object("uploads/a/0", Uint8Array.of(...stored("").subarray(0, 8), 0x00, 14 << 3, 1, 0, 0))],
  ]) {
    const w = world();
    w.row(SLUG, hiRow());
    setup(w);
    const warned = await capturingWarnings(async () => assertInternalError(await w.fetch(at("/"))));
    assert.equal(warned.length, 1, what);
    assert.match(warned[0], new RegExp(`slug=${SLUG} `), what);
    // HEAD opens nothing, so it cannot see the damage.
    assert.equal((await w.fetch(at("/"), { method: "HEAD" })).status, 200, what);
  }
});

test("damage found after the first byte errors the body and logs one warning", async () => {
  const w = world();
  w.row(SLUG, documentRow("html", "ready", 300 * 1024));
  const content = new Uint8Array(300 * 1024).fill(0x61);
  const truncated = stored(content);
  w.object("uploads/a/0", truncated.subarray(0, truncated.length - 10));
  const warned = await capturingWarnings(async () => {
    const res = await w.fetch(at("/"));
    assert.equal(res.status, 200);
    await assert.rejects(res.arrayBuffer(), /truncated/);
  });
  assert.equal(warned.length, 1);
  assert.match(warned[0], new RegExp(`slug=${SLUG} .*truncated`));
});

// Reads the body to its end or its error; the bytes it gave and the error.
async function readAll(res) {
  let size = 0;
  try {
    for await (const chunk of res.body) size += chunk.length;
  } catch (error) {
    return { size, error };
  }
  return { size, error: null };
}

test("a checksummed payload decodes whole", async () => {
  const content = new Uint8Array(300 * 1024 + 5);
  for (let i = 0; i < content.length; i++) content[i] = (i * 7) & 0xff;
  for (const bytes of [content, content.subarray(0, 3), content.subarray(0, 0)]) {
    const w = world();
    w.row(SLUG, documentRow("html", "ready", bytes.length));
    w.object("uploads/a/0", stored(bytes, { checksum: true }));
    const res = await w.fetch(at("/"));
    assert.equal(sha256(new Uint8Array(await res.arrayBuffer())), sha256(bytes), `${bytes.length} bytes`);
  }
});

test("stored bytes carry their manifest size, and a body that disagrees ends short of it, aborted", async () => {
  const content = new Uint8Array(300 * 1024).fill(0x61);
  const whole = stored(content);
  const trailing = new Uint8Array(whole.length + 8).fill(0x41);
  trailing.set(whole);
  // A frame whose blocks end on an input batch, so the decoder sends every
  // byte before it reads the checksum, and only then fails it.
  const batched = content.subarray(0, 3 * INPUT_BATCH - 12);
  const flipped = stored(batched, { checksum: true });
  flipped[flipped.length - 1] ^= 0x01;
  for (const [what, size, object] of [
    ["damaged mid-body", content.length, whole.subarray(0, whole.length - 10)],
    ["a flipped checksum byte", batched.length, flipped],
    ["bytes after the frame", content.length, trailing],
    ["shorter than its size", content.length + 1, whole],
    ["longer than its size", content.length - 1, whole],
  ]) {
    const w = world();
    w.row(SLUG, documentRow("html", "ready", size));
    w.object("uploads/a/0", object);
    const warned = await capturingWarnings(async () => {
      const res = await w.fetch(at("/"));
      assert.equal(res.status, 200, what);
      assert.equal(res.headers.get("Content-Length"), String(size), what);
      const got = await readAll(res);
      assert.ok(got.error, `${what}: the body ended as complete`);
      assert.ok(got.size < size, `${what}: ${got.size} of ${size} bytes`);
    });
    assert.equal(warned.length, 1, what);
    assert.match(warned[0], new RegExp(`slug=${SLUG} `), what);
  }
});

test("stored bytes carry their size, generated pages none, and an unusable size streams without one", async () => {
  const w = world();
  w.row(SLUG, {
    kind: "knowledgebase", status: "ready", updatedAt: UPDATED, uploadId: "up",
    manifest: { Files: {
      "empty.txt": { Key: "k-empty", Size: 0, ContentType: "text/plain" },
      "doc.md": { Key: "k-doc", Size: 6 },
      "negative.txt": { Key: "k-negative", Size: -1, ContentType: "text/plain" },
    } },
  });
  for (const [key, content] of [["k-empty", ""], ["k-doc", "# doc\n"], ["k-negative", "negative"]]) w.blob(key, content);
  for (const [path, length, body] of [
    ["/empty.txt", "0", ""], ["/doc.md?raw", "6", "# doc\n"], ["/negative.txt", null, "negative"],
    ["/doc.md", null, null], ["/?files", null, null],
  ]) {
    const res = await w.fetch(at(path));
    assert.equal(res.status, 200, path);
    assert.equal(res.headers.get("Content-Length"), length, path);
    const text = await res.text();
    if (body !== null) assert.equal(text, body, path);
  }
  // An answer without a size streams the same way.
  const answer = { action: "file", kind: "site", updatedAt: UPDATED, uploadId: "up", entry: { key: "k-doc", contentType: "" } };
  w.env.PASTES = { idFromName: (name) => name, get: () => ({ fetch: async () => Response.json(answer) }) };
  const res = await w.fetch(at("/x"));
  assert.equal(res.headers.get("Content-Length"), null);
  assert.equal(await res.text(), "# doc\n");
});

test("a runtime's FixedLengthStream carries a body that states its length", async () => {
  const made = [];
  // Errors when the bytes written differ from the length, as workerd's does.
  class FixedLengthStream extends TransformStream {
    constructor(length) {
      let seen = 0;
      super({
        transform(chunk, controller) {
          seen += chunk.byteLength;
          if (seen > length) throw new Error("too long");
          controller.enqueue(chunk);
        },
        flush() {
          if (seen !== length) throw new Error("too short");
        },
      });
      made.push(length);
    }
  }
  const w = world();
  w.row(SLUG, documentRow("markdown", "ready", RAW_SOURCE.length));
  w.blob("uploads/a/0", RAW_SOURCE);
  const read = publicReads(w.env, undefined, FixedLengthStream);
  const request = (path, method = "GET") => new Request(at(path), { method });
  const target = (path, raw) => ({ slug: SLUG, path, host: `${SLUG}.${APEX}`, mount: "", flags: { raw, files: false } });
  const raw = await read(request("/?raw"), w.env, w.ctx, target("/", true));
  assert.equal(raw.headers.get("Content-Length"), String(RAW_SOURCE.length));
  assert.equal(await raw.text(), RAW_SOURCE);
  assert.deepEqual(made, [RAW_SOURCE.length]);
  await (await read(request("/"), w.env, w.ctx, target("/", false))).text();
  await read(request("/?raw", "HEAD"), w.env, w.ctx, target("/", true));
  assert.deepEqual(made, [RAW_SOURCE.length], "a shell or a HEAD is sent as it is");
});

test("a metadata failure is a 500 with one warning", async () => {
  const w = world();
  w.env.PASTES = {
    idFromName: (name) => name,
    get: () => ({ fetch: async () => new Response("cell busy\n", { status: 503 }) }),
  };
  const warned = await capturingWarnings(async () => assertInternalError(await w.fetch(at("/"))));
  assert.equal(warned.length, 1);
  assert.match(warned[0], new RegExp(`slug=${SLUG} metadata`));
});

test("a stored kind with no shell is a 500 naming it", async () => {
  const w = world();
  w.row(SLUG, documentRow("hologram"));
  const warned = await capturingWarnings(async () => {
    for (const headers of [{}, { "If-None-Match": "*" }]) {
      const res = await w.fetch(at("/"), { headers });
      assert.equal(res.status, 500);
      assert.equal(await res.text(), "unsupported kind\n");
      assert.deepEqual(headersOf(res), BARE_500);
    }
  });
  assert.equal(warned.length, 2);
  assert.match(warned[0], /hologram/);
});

const assetSources = new Map(listAssets().map((a) => [a.name, a]));

function staticAssets(request) {
  const name = new URL(request.url).pathname.slice("/_hostthis/".length);
  const found = assetSources.get(name);
  if (!found || found.size > 25 * 1024 * 1024) return new Response("", { status: 404 });
  return new Response(request.method === "HEAD" ? null : readFileSync(found.source), {
    headers: { "Content-Type": found.contentType, "ETag": "\"asset\"" },
  });
}

test("assets are hostthisd's, from static assets or the payload bucket", async () => {
  const names = Object.keys(golden).filter((k) => k.startsWith("asset/")).map((k) => k.slice("asset/".length));
  assert.ok(names.includes("duckdb-eh.wasm") && names.includes("mdshell/md.js") && names.includes("evil.js"));
  for (const withAssets of [true, false]) {
    const w = world();
    if (withAssets) w.assets(staticAssets);
    for (const found of assetSources.values()) {
      if (!withAssets || found.size > 25 * 1024 * 1024) {
        w.object(`static/${found.version}/${found.name}`, readFileSync(found.source));
      }
    }
    for (const name of names) {
      await assertGolden(await w.fetch(at(`/_hostthis/${name}?v=x`)), `asset/${name}`);
    }
    const r2 = [...assetSources.values()].filter((a) => a.size > 25 * 1024 * 1024).map((a) => a.name);
    assert.deepEqual(r2, ["duckdb-eh.wasm"]);
    if (withAssets) {
      // The last segment names the asset, as in hostthisd; the bucket serves only the oversized one.
      assert.ok(w.assetCalls.includes("GET /_hostthis/md.js"));
      assert.ok(!w.assetCalls.some((c) => c.endsWith("/evil.js") || c.endsWith("/duckdb-eh.wasm")));
      assert.deepEqual(w.payloadCalls, [`get static/${assetSources.get("duckdb-eh.wasm").version}/duckdb-eh.wasm`]);
    }
  }
});

test("an asset HEAD reads nothing, and a missing stored asset is a 500", async () => {
  const w = world();
  const res = await w.fetch(at("/_hostthis/md.js"), { method: "HEAD" });
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Content-Type"), "text/javascript; charset=utf-8");
  assert.equal(await res.text(), "");
  assert.deepEqual(w.payloadCalls, []);
  const warned = await capturingWarnings(async () => {
    const failed = await w.fetch(at("/_hostthis/md.js"));
    assert.equal(failed.status, 500);
    assert.equal(await failed.text(), "internal error\n");
    assert.deepEqual(headersOf(failed), { "content-type": "text/plain; charset=utf-8", "x-content-type-options": "nosniff" });
  });
  assert.equal(warned.length, 1);
  // Assets answer on every public host.
  w.assets(staticAssets);
  for (const host of [APEX, `www.${APEX}`]) {
    assert.equal((await w.fetch(`https://${host}/_hostthis/md.js`)).status, 200, host);
  }
});

test("a large payload streams in decoder-sized chunks", async () => {
  const w = world();
  w.row(SLUG, documentRow("html", "ready", 3 * 1024 * 1024));
  const content = new Uint8Array(3 * 1024 * 1024);
  for (let i = 0; i < content.length; i++) content[i] = (i * 31) & 0xff;
  w.blob("uploads/a/0", content);
  const res = await w.fetch(at("/"));
  let size = 0;
  let largest = 0;
  const hash = createHash("sha256");
  for await (const chunk of res.body) {
    size += chunk.length;
    largest = Math.max(largest, chunk.length);
    hash.update(chunk);
  }
  assert.equal(size, content.length);
  assert.equal(hash.digest("hex"), sha256(content));
  assert.ok(largest <= 128 * 1024, `${largest}`);
});
