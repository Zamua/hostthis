import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { build } from "esbuild";

import { listAssets, loadTable, MAX_ASSET_BYTES, REPO, shellsPlugin } from "../build/shells.mjs";
import { asset, rawContentType, renderShell } from "../src/public/shells.js";

// hostthisd's responses, recorded by internal/http's TestShells_GoldenResponses.
const golden = JSON.parse(readFileSync(join(REPO, "testdata", "shells-golden.json"), "utf8"));
const DIST = fileURLToPath(new URL("../dist", import.meta.url));

function header(response, name) {
  const found = Object.keys(response.header).find((k) => k.toLowerCase() === name.toLowerCase());
  return found === undefined ? undefined : response.header[found];
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

test("every shell renders the bytes, ETag and policy hostthisd serves", () => {
  const kinds = Object.keys(golden).filter((k) => k.startsWith("shell/")).map((k) => k.slice("shell/".length));
  assert.deepEqual(kinds.sort(), Object.keys(loadTable().kinds).sort());
  for (const kind of kinds) {
    const want = golden[`shell/${kind}`];
    const got = renderShell(kind);
    const body = new TextEncoder().encode(got.body);
    assert.equal(body.length, want.size, kind);
    assert.equal(sha256(body), want.sha256, kind);
    assert.equal(`"${got.version}"`, header(want, "ETag"), kind);
    assert.equal(got.csp, header(want, "Content-Security-Policy"), kind);
    assert.equal(rawContentType(kind), header(golden[`raw/${kind}`], "Content-Type"), kind);
  }
});

test("bundled through the build's plugin, shells.js renders what the source does", async () => {
  const out = await build({
    absWorkingDir: fileURLToPath(new URL("..", import.meta.url)),
    entryPoints: ["src/public/shells.js"],
    bundle: true,
    format: "esm",
    platform: "neutral",
    write: false,
    logLevel: "silent",
    plugins: [shellsPlugin],
  });
  const text = out.outputFiles[0].text;
  assert.doesNotMatch(text, /^\s*import\b/m);
  const bundled = await import(`data:text/javascript;base64,${Buffer.from(text).toString("base64")}`);
  for (const kind of Object.keys(loadTable().kinds)) {
    assert.deepEqual(bundled.renderShell(kind), renderShell(kind), kind);
  }
});

test("a kind without a shell renders nothing", () => {
  for (const kind of ["html", "site", "", "__proto__", "constructor", "toString", "MARKDOWN"]) {
    assert.equal(renderShell(kind), null, kind);
    assert.equal(rawContentType(kind), null, kind);
  }
});

test("assets resolve to hostthisd's Content-Type, and nothing else resolves", () => {
  const served = Object.entries(golden).filter(([k, r]) => k.startsWith("asset/") && !k.includes("/", 6) && r.status === 200);
  assert.ok(served.length > 0);
  for (const [k, want] of served) {
    assert.equal(asset(k.slice("asset/".length))?.contentType, header(want, "Content-Type"), k);
  }
  for (const name of ["shell.html", "shells.json", "evil.js", "", "__proto__", "mdshell/md.js"]) {
    assert.equal(asset(name), null, name);
  }
});

function parseHeaders(text) {
  const rules = new Map();
  let path = null;
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    if (!/^\s/.test(line)) {
      assert.ok(!rules.has(line), `duplicate rule ${line}`);
      path = line;
      rules.set(path, {});
      continue;
    }
    const [, name, value] = line.match(/^\s+([^:]+):\s*(.*)$/);
    rules.get(path)[name] = value;
  }
  return rules;
}

test("the assets directory holds every servable asset under 25 MiB, each with its headers", () => {
  assert.deepEqual(readdirSync(join(DIST, "assets")).sort(), ["_headers", "_hostthis"]);
  const files = readdirSync(join(DIST, "assets", "_hostthis")).sort();
  const small = listAssets().filter((a) => a.size <= MAX_ASSET_BYTES).map((a) => a.name);
  assert.deepEqual(files, small);
  assert.ok(!files.includes("shell.html") && !files.includes("shells.json"));

  const rules = parseHeaders(readFileSync(join(DIST, "assets", "_headers"), "utf8"));
  // Cloudflare reads at most 100 rules.
  assert.ok(rules.size <= 100, `${rules.size} rules`);
  assert.deepEqual([...rules.keys()].sort(), files.map((name) => `/_hostthis/${name}`));
  for (const name of files) {
    const path = join(DIST, "assets", "_hostthis", name);
    const want = golden[`asset/${name}`];
    assert.ok(statSync(path).size <= MAX_ASSET_BYTES, name);
    assert.equal(sha256(readFileSync(path)), want.sha256, name);
    assert.deepEqual(rules.get(`/_hostthis/${name}`), {
      "Content-Type": header(want, "Content-Type"),
      "Cache-Control": header(want, "Cache-Control"),
    }, name);
  }
});

test("static-r2.json lists exactly the oversized assets, under their shell version", () => {
  const listed = JSON.parse(readFileSync(join(DIST, "static-r2.json"), "utf8"));
  const oversized = listAssets().filter((a) => a.size > MAX_ASSET_BYTES);
  assert.ok(oversized.length > 0, "no asset is oversized, so this checks nothing");
  assert.deepEqual(listed.map((e) => e.name), oversized.map((a) => a.name));
  for (const entry of listed) {
    const want = golden[`asset/${entry.name}`];
    assert.equal(entry.key, asset(entry.name).key, entry.name);
    assert.match(entry.key, /^static\/[^/]+\/[^/]+$/, entry.name);
    assert.equal(entry.contentType, header(want, "Content-Type"), entry.name);
    const bytes = readFileSync(join(REPO, entry.source));
    assert.equal(bytes.length, entry.size, entry.name);
    assert.ok(entry.size > MAX_ASSET_BYTES, entry.name);
    assert.equal(sha256(bytes), want.sha256, entry.name);
  }
});
