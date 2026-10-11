import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { ASSET_EXTENSIONS, resolveRead } from "../src/read.js";

const shared = JSON.parse(readFileSync(new URL("../../testdata/read-vectors.json", import.meta.url), "utf8"));

for (const v of shared.vectors) {
  test(`read vector: ${v.name}`, () => {
    const row = v.row === null ? undefined : structuredClone(shared.rows[v.row]);
    assert.ok(v.row === null || row, `no row ${v.row}`);
    const before = structuredClone(row);
    assert.deepEqual(resolveRead(row, v.request), v.want);
    assert.deepEqual(row, before, "the row is left as it was");
  });
}

test("the asset extensions are the shared table's", () => {
  const shared_ = Object.entries(shared.extensions).filter(([, asset]) => asset).map(([ext]) => ext);
  assert.deepEqual([...ASSET_EXTENSIONS].sort(), shared_.sort());
});

// The Go read path answers 500 here: it reads the blob store for a key it does
// not have, where docs/SPEC.md "Entries without an object key" says 404.
test("a knowledge base document without a key is not found under ?raw", () => {
  const row = structuredClone(shared.rows.kb);
  assert.equal(row.manifest.Files["keyless.md"].Key, "");
  assert.equal(resolveRead(row, { path: "/keyless.md", raw: true }).action, "notfound");
});

test("flags default to off", () => {
  assert.deepEqual(resolveRead(shared.rows.markdown, { path: "/" }), resolveRead(shared.rows.markdown, {
    path: "/", raw: false, files: false,
  }));
});
