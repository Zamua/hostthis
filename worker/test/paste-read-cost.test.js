import assert from "node:assert/strict";
import test from "node:test";

import { Paste } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";

const ROW = { slug: "abc12345", status: "ready", kind: "html", identity: "key:owner", size: 3, createdAt: 7, updatedAt: 7, uploadId: "a".repeat(32) };

// A Paste cell over storage that counts reads of the row.
function counted() {
  const storage = new FakeStorage(new Map([["row", ROW]]));
  const reads = { row: 0 };
  const get = storage.get.bind(storage);
  storage.get = (key, ...rest) => {
    if (key === "row") {
      reads.row++;
    }
    return get(key, ...rest);
  };
  const notes = [];
  const env = { FEED: { idFromName: (n) => n, get: () => ({ fetch: async (req) => { notes.push(new URL(req.url).pathname); return new Response(null, { status: 204 }); } }) } };
  return { cell: new Paste(state(storage), env), reads, notes };
}

test("a read op reads the row once and tells the feed nothing", async () => {
  for (const path of ["/paste/resolve?slug=abc12345&path=/", "/paste/get?slug=abc12345", "/paste/exists?slug=abc12345", "/do/x?slug=abc12345&op=exists"]) {
    const { cell, reads, notes } = counted();
    const res = await cell.fetch(new Request(`https://cell${path}`));
    assert.equal(res.status, 200, path);
    assert.equal(reads.row, 1, `${path} read the row ${reads.row} times`);
    assert.deepEqual(notes, [], path);
  }
});

test("a mutating op still notices the change it makes", async () => {
  const { cell, reads, notes } = counted();
  const res = await cell.fetch(new Request("https://cell/paste/quarantine?slug=abc12345", {
    method: "POST", body: JSON.stringify({ reason: "abuse", actor: "operator", at: 9 }),
  }));
  assert.equal(res.status, 200);
  assert.ok(reads.row >= 2, "the change is noticed by comparing the row before and after");
  assert.equal(notes.length, 1);
});
