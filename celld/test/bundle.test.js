import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

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
