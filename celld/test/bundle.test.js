import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import * as bundle from "../dist/index.js";
import * as source from "../src/index.js";

const BUNDLE = new URL("../dist/index.js", import.meta.url);

// celld loads one module under no_bundle, so the bundle may import nothing.
test("the bundle is one self-contained module", () => {
  const text = readFileSync(BUNDLE, "utf8");
  assert.doesNotMatch(text, /^\s*import\b/m);
  assert.doesNotMatch(text, /^\s*export\b[^;]*\bfrom\s*["']/m);
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
