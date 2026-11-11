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

test("a create whose row is too large for the cell is refused 413 and leaves nothing behind", async () => {
  const storage = new FakeStorage();
  storage.tooBig = (key) => key === "row";
  const cell = new Paste(state(storage), {});
  const res = await cell.fetch(new Request("https://cell/paste/put?slug=abc12345", {
    method: "POST",
    body: JSON.stringify({ row: { ...ROW, manifest: { files: [] } }, generation: "g1", fingerprint: "f1" }),
  }));
  assert.equal(res.status, 413);
  assert.deepEqual(await res.json(), { error: "version-too-large" });
  assert.equal(storage.data.has("row"), false);
  assert.equal(storage.data.has("maxVer"), false);
});

const SITE = { ...ROW, kind: "site", generation: "g1" };
const file = (key) => ({ Key: key, DecodedSize: 3, ContentType: "text/plain" });
const resolveKey = async (cell, path) => {
  const res = await cell.fetch(new Request(`https://cell/paste/resolve?slug=abc12345&path=${path}`));
  return (await res.json()).entry?.key;
};

test("reads of one row decode its manifest once, and any op that may write decodes it afresh", async () => {
  const storage = new FakeStorage(new Map([["row", { ...SITE, manifest: { Files: { "a.txt": file("blobs/one") } } }]]));
  const cell = new Paste(state(storage), {});
  assert.equal(await resolveKey(cell, "/a.txt"), "blobs/one");

  // Stored behind the cell's back with nothing else changed: only a kept
  // decoding still answers with the old key.
  storage.data.set("row", { ...SITE, manifest: { Files: { "a.txt": file("blobs/two") } } });
  assert.equal(await resolveKey(cell, "/a.txt"), "blobs/one");

  // A mutating op, whatever it does, drops what was kept.
  await cell.fetch(new Request("https://cell/paste/release?slug=abc12345", { method: "POST", body: "{}" }));
  assert.equal(await resolveKey(cell, "/a.txt"), "blobs/two");

  // So does the alarm.
  storage.data.set("row", { ...SITE, manifest: { Files: { "a.txt": file("blobs/three") } } });
  await cell.alarm();
  assert.equal(await resolveKey(cell, "/a.txt"), "blobs/three");
});

test("a row that differs in generation, upload, update time or pin is decoded afresh without any op", async () => {
  const changes = [{ generation: "g2" }, { uploadId: "b".repeat(32) }, { updatedAt: 8 }, { pinnedVersion: 2 }];
  for (const change of changes) {
    const storage = new FakeStorage(new Map([["row", { ...SITE, manifest: { Files: { "a.txt": file("blobs/one") } } }]]));
    const cell = new Paste(state(storage), {});
    assert.equal(await resolveKey(cell, "/a.txt"), "blobs/one");
    storage.data.set("row", { ...SITE, ...change, manifest: { Files: { "a.txt": file("blobs/two") } } });
    assert.equal(await resolveKey(cell, "/a.txt"), "blobs/two", JSON.stringify(change));
  }
});

test("a quarantined paste answers not found even with its manifest decoded and kept", async () => {
  const storage = new FakeStorage(new Map([["row", { ...SITE, manifest: { Files: { "a.txt": file("blobs/one") } } }]]));
  const cell = new Paste(state(storage), {});
  assert.equal(await resolveKey(cell, "/a.txt"), "blobs/one");
  storage.data.set("quarantine", { reason: "abuse", at: 9 });
  const res = await cell.fetch(new Request("https://cell/paste/resolve?slug=abc12345&path=/a.txt"));
  assert.deepEqual(await res.json(), { found: false });
});

test("every paste op declares whether it may write", async () => {
  const { PASTE_OPS } = await import("../src/cells/paste.js");
  for (const [name, op] of Object.entries(PASTE_OPS)) {
    assert.equal(typeof op.mutating, "boolean", name);
  }
});
