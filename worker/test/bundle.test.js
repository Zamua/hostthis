import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { buildId, hashInputs } from "../build/buildid.mjs";
import * as bundle from "../dist/index.js";
import * as source from "../src/index.js";

const BUNDLE = new URL("../dist/index.js", import.meta.url);

// celld loads one module under no_bundle plus the WASM modules beside it, so
// the bundle imports nothing else.
test("the bundle imports only the WASM modules built beside it", () => {
  const text = readFileSync(BUNDLE, "utf8");
  const imports = [...text.matchAll(/^\s*import\b.*$/gm)].map((m) => m[0].trim());
  assert.deepEqual(imports, ['import zdModule from "./zd.wasm";']);
  assert.doesNotMatch(text, /^\s*export\b[^;]*\bfrom\s*["']/m);
  assert.doesNotMatch(text, /\bimport\s*\(/);
  const vendored = readFileSync(new URL("../vendor/zstd/zd.wasm", import.meta.url));
  assert.ok(readFileSync(new URL("../dist/zd.wasm", import.meta.url)).equals(vendored));
});

test("the bundle carries the build id of the sources it was built from", () => {
  const id = buildId();
  assert.match(id, /^[0-9a-f]{16}$/);
  assert.ok(readFileSync(BUNDLE, "utf8").includes(JSON.stringify(id)), "a stale bundle, or the id is not built in");
});

test("the build id hashes the shells and every source and vendored file", () => {
  const files = [{ path: "worker/src/a.js", bytes: Buffer.from("a") }];
  const base = hashInputs("shells", files);
  assert.equal(hashInputs("shells", files), base);
  assert.notEqual(hashInputs("shells2", files), base);
  assert.notEqual(hashInputs("shells", [{ ...files[0], bytes: Buffer.from("b") }]), base);
  assert.notEqual(hashInputs("shells", [{ ...files[0], path: "worker/src/b.js" }]), base);
});

test("the bundle exports what the source entry exports", () => {
  assert.deepEqual(Object.keys(bundle).sort(), Object.keys(source).sort());
  assert.equal(typeof bundle.default.fetch, "function");
});

test("exported classes keep their names", () => {
  const classes = Object.entries(bundle)
    .filter(([, value]) => typeof value === "function" && /^class\b/.test(Function.prototype.toString.call(value)));
  assert.ok(classes.length > 0, "no exported classes");
  for (const [name, value] of classes) assert.equal(value.name, name);
});
