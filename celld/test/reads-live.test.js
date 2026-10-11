// Public reads under workerd (Miniflare, from celld/localrt), where the WASM
// decoder loads as the compiled module the runtime hands the bundle and the
// payloads come from a real R2 binding. The blobs are internal/zstdenc's.
// Skipped until `npm ci --prefix celld/localrt` has installed the runtime.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { fileURLToPath } from "node:url";

import { REPO } from "../build/shells.mjs";

const celld = fileURLToPath(new URL("..", import.meta.url));
const APEX = "example.test";
const SLUG = "abcdefgh";
const TOKEN = "live-token";
const AUTH = { Authorization: `Bearer ${TOKEN}` };
const FILES = ["empty", "onebyte", "text", "incompressible", "repeat100m"];

let Miniflare = null;
try {
  ({ Miniflare } = createRequire(`${celld}localrt/package.json`)("miniflare"));
} catch {
  // not installed
}
const skip = Miniflare ? false : "celld/localrt is not installed";

let mf;
let dir;
let manifest;

// The bucket takes a body of known length, as hostthisd sends it.
async function internal(path, init) {
  const headers = { ...AUTH, "Content-Length": String(Buffer.byteLength(init.body)) };
  const res = await mf.dispatchFetch(`http://celld.internal${path}`, { ...init, headers });
  assert.ok(res.ok, `${path}: ${res.status} ${await res.clone().text()}`);
  await res.arrayBuffer();
}

before(async () => {
  if (skip) return;
  dir = mkdtempSync(join(tmpdir(), "hostthis-reads-live-"));
  execFileSync("go", ["run", "./celld/test/zstdcorpus", dir], { cwd: REPO, stdio: ["ignore", "ignore", "inherit"] });
  manifest = JSON.parse(readFileSync(join(dir, "manifest.json"), "utf8"));
  const config = JSON.parse(readFileSync(`${celld}wrangler.jsonc`, "utf8")
    .split("\n").filter((line) => !line.trim().startsWith("//")).join("\n"));
  mf = new Miniflare({
    modules: true,
    scriptPath: `${celld}dist/index.js`,
    modulesRoot: celld,
    modulesRules: [{ type: "ESModule", include: ["**/*.js"] }, { type: "CompiledWasm", include: ["**/*.wasm"] }],
    compatibilityDate: config.compatibility_date,
    cf: false,
    bindings: { HOSTTHIS_APEX_DOMAIN: APEX, HOSTTHIS_CELLD_TOKEN: TOKEN },
    r2Buckets: Object.fromEntries(config.r2_buckets.map((b) => [b.binding, b.bucket_name])),
    durableObjectsPersist: false,
    durableObjects: Object.fromEntries(config.durable_objects.bindings
      .map((b) => [b.name, { className: b.class_name, useSQLite: true }])),
  });
  await mf.ready;
  const files = {};
  for (const name of FILES) {
    const key = `uploads/live/${name}`;
    await internal(`/blob/${key}`, { method: "PUT", body: readFileSync(join(dir, `${name}.hz`)) });
    files[`${name}.txt`] = { Key: key, Size: manifest[name].size, ContentType: "text/plain; charset=utf-8" };
  }
  const text = readFileSync(join(dir, "text.hz"));
  await internal("/blob/uploads/live/truncated", { method: "PUT", body: text.subarray(0, text.length - 2) });
  files["truncated.txt"] = { Key: "uploads/live/truncated", Size: manifest.text.size, ContentType: "text/plain" };
  // Whole bytes, one short of the size the manifest gives them.
  files["oversized.txt"] = { Key: "uploads/live/text", Size: manifest.text.size + 1, ContentType: "text/plain" };
  await internal("/blob/uploads/live/unframed", { method: "PUT", body: Buffer.from("not at rest") });
  files["unframed.txt"] = { Key: "uploads/live/unframed", Size: 11, ContentType: "text/plain" };
  await internal(`/paste/put?slug=${SLUG}`, {
    method: "POST",
    body: JSON.stringify({
      generation: "g1", fingerprint: "f1",
      row: {
        slug: SLUG, identity: "owner", status: "ready", kind: "site", size: 1, name: "", pinnedVersion: 0,
        createdAt: 1, updatedAt: 1, uploadId: "live", manifest: { Files: files },
      },
    }),
  });
});

after(async () => {
  await mf?.dispose();
  if (dir) rmSync(dir, { recursive: true, force: true });
});

// A GET over the runtime's own HTTP server, which shows the framing a client
// sees (dispatchFetch's proxy re-encodes the body and drops Content-Length).
// aborted is whether the connection broke before the body ended.
async function wireGet(path) {
  const url = new URL(path, await mf.ready);
  return new Promise((resolve, reject) => {
    const req = http.get(url, { agent: false, headers: { Host: `${SLUG}.${APEX}` } }, (res) => {
      const hash = createHash("sha256");
      let size = 0;
      let aborted = false;
      res.on("data", (chunk) => {
        hash.update(chunk);
        size += chunk.length;
      });
      res.on("aborted", () => { aborted = true; });
      res.on("error", () => { aborted = true; });
      res.on("close", () => resolve({ status: res.statusCode, headers: res.headers, size, sha256: hash.digest("hex"), aborted }));
    });
    req.on("error", reject);
  });
}

test("every corpus blob decodes in workerd to its bytes, with its size as Content-Length", { skip }, async () => {
  for (const name of FILES) {
    const res = await mf.dispatchFetch(`https://${SLUG}.${APEX}/${name}.txt`);
    assert.equal(res.status, 200, name);
    assert.equal(res.headers.get("Cache-Control"), "public, no-cache", name);
    const hash = createHash("sha256");
    let size = 0;
    for await (const chunk of res.body) {
      hash.update(chunk);
      size += chunk.length;
    }
    assert.equal(size, manifest[name].size, name);
    assert.equal(hash.digest("hex"), manifest[name].sha256, name);
    if (name === "repeat100m") continue;
    const wire = await wireGet(`/${name}.txt`);
    assert.equal(wire.headers["content-length"], String(manifest[name].size), name);
    assert.equal(wire.headers["transfer-encoding"], undefined, name);
    assert.deepEqual([wire.size, wire.sha256, wire.aborted], [manifest[name].size, manifest[name].sha256, false], name);
  }
});

test("damage before the first byte is a 500, and after it the body ends short of its Content-Length", { skip }, async () => {
  const unframed = await mf.dispatchFetch(`https://${SLUG}.${APEX}/unframed.txt`);
  assert.equal(unframed.status, 500);
  assert.equal(await unframed.text(), "internal error\n");
  for (const [name, length] of [["truncated", manifest.text.size], ["oversized", manifest.text.size + 1]]) {
    const wire = await wireGet(`/${name}.txt`);
    assert.equal(wire.status, 200, name);
    assert.equal(wire.headers["content-length"], String(length), name);
    assert.ok(wire.size < length, `${name}: ${wire.size} of ${length} bytes`);
    assert.ok(wire.aborted, `${name}: the connection ended as if the body were whole`);
  }
});
