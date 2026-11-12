import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import test from "node:test";

import { actorHash, countingPageReads, isPageRead, notePublish, noteRoomCreated } from "../src/activity.js";
import { Paste } from "../src/cells/paste.js";
import { FakeStorage, state } from "./harness.js";

const OWNER = "key:SHA256:" + "ab".repeat(32);
const SALT = "s".repeat(40);
const CONTENT = "public, no-cache";

// A dataset that keeps its points, and a waiter that keeps the work.
function world(env = {}) {
  const points = [];
  const work = [];
  return {
    points,
    env: { ACTIVITY: { writeDataPoint: (point) => points.push(point) }, ACTIVITY_SALT: SALT, ...env },
    waiter: { waitUntil: (p) => work.push(p) },
    settle: () => Promise.all(work),
  };
}

const want = (id) => createHmac("sha256", SALT).update(`hostthis:${id}`).digest("hex").slice(0, 16);

test("an actor is written as 16 hex characters of a keyed hash, never as itself", async () => {
  assert.equal(await actorHash(SALT, OWNER), want(OWNER));
  assert.notEqual(await actorHash("t".repeat(40), OWNER), want(OWNER));
  const w = world();
  notePublish(w.env, w.waiter, OWNER, "site");
  await w.settle();
  assert.deepEqual(w.points, [
    { indexes: [want(OWNER)], blobs: ["hostthis", "active", ""], doubles: [1] },
    { indexes: [`${want(OWNER)}:publish`], blobs: ["hostthis", "publish", "site"], doubles: [1] },
  ]);
  assert.equal(JSON.stringify(w.points).includes("SHA256"), false);
});

test("a publish names a paste or a site; a room has no actor", async () => {
  const w = world();
  for (const kind of ["html", "markdown", "knowledgebase", undefined]) {
    notePublish(w.env, w.waiter, OWNER, kind);
  }
  noteRoomCreated(w.env, w.waiter);
  await w.settle();
  // The room's point needs no hash, so it may land first.
  assert.deepEqual(w.points.filter((p) => p.blobs[1] === "publish").map((p) => `${p.indexes[0]} ${p.blobs[2]}`).sort(),
    ["-:publish room", `${want(OWNER)}:publish paste`, `${want(OWNER)}:publish paste`, `${want(OWNER)}:publish paste`, `${want(OWNER)}:publish site`].sort());
  assert.deepEqual(w.points.filter((p) => p.blobs[1] === "active").map((p) => p.indexes[0]), Array(4).fill(want(OWNER)));
});

test("without a dataset, a salt or an owner nothing is written, and a failing write is silent", async () => {
  for (const env of [{ ACTIVITY: undefined }, { ACTIVITY_SALT: "" }]) {
    const w = world(env);
    notePublish(w.env, w.waiter, OWNER, "html");
    await w.settle();
    assert.deepEqual(w.points, []);
  }
  const unsalted = world({ ACTIVITY_SALT: undefined });
  noteRoomCreated(unsalted.env, unsalted.waiter);
  notePublish(unsalted.env, unsalted.waiter, "", "html");
  await unsalted.settle();
  assert.deepEqual(unsalted.points.map((p) => p.blobs[2]), ["room"]);

  const broken = world({ ACTIVITY: { writeDataPoint: () => { throw new Error("no"); } } });
  notePublish(broken.env, broken.waiter, OWNER, "html");
  notePublish(undefined, undefined, OWNER, "html");
  notePublish(broken.env, { waitUntil: () => { throw new Error("no"); } }, OWNER, "html");
  await broken.settle();
});

test("a listed owner's publishes are test traffic and write nothing", async () => {
  const other = "key:SHA256:" + "cd".repeat(32);
  const w = world({ HOSTTHIS_ACTIVITY_SKIP_OWNERS: ` ${other}, ${OWNER} ` });
  notePublish(w.env, w.waiter, OWNER, "html");
  notePublish(w.env, w.waiter, other, "site");
  notePublish(w.env, w.waiter, OWNER + "0", "html");
  await w.settle();
  assert.deepEqual(w.points.map((p) => p.indexes[0]), [want(OWNER + "0"), `${want(OWNER + "0")}:publish`]);
});

const get = (headers = {}, method = "GET") => new Request("https://abcd2345.apex.example/", { method, headers: { "User-Agent": "Mozilla/5.0", ...headers } });
const html = (status = 200, headers = {}) => new Response(status === 304 ? null : "x", {
  status, headers: { "Cache-Control": CONTENT, ...(status === 304 ? {} : { "Content-Type": "text/html; charset=utf-8" }), ...headers },
});

test("a page read is a person's GET of a paste's HTML", () => {
  const cases = [
    [get(), html(), true],
    [get({ "Sec-Fetch-Dest": "document" }), html(), true],
    [get({ "Sec-Fetch-Dest": "document" }), html(304), true],
    [get(), html(304), false],
    [get({ "Sec-Fetch-Dest": "iframe" }), html(), false],
    [get({ "X-Activity-Skip": "1" }), html(), false],
    [get({ "X-Activity-Skip": "0" }), html(), true],
    [get({}, "HEAD"), html(), false],
    [get(), html(200, { "Cache-Control": "no-store" }), false],
    [get(), html(200, { "Content-Type": "text/css" }), false],
    [get(), html(404), false],
    [get({ "User-Agent": "curl/8.1" }), html(), false],
    [get({ "User-Agent": "Mozilla/5.0 (compatible; Examplebot/2.1)" }), html(), false],
    [new Request("https://abcd2345.apex.example/"), html(), false],
  ];
  cases.forEach(([request, res, counted], i) => assert.equal(isPageRead(request, res, CONTENT), counted, `case ${i}`));
});

test("the counted read path answers as the bare one and writes one point per page read", async () => {
  const w = world({ ACTIVITY_SALT: undefined });
  const answers = [html(), html(200, { "Content-Type": "text/css" })];
  const read = countingPageReads(async () => answers.shift(), CONTENT);
  const first = await read(get(), w.env, w.waiter, {});
  assert.equal(await first.text(), "x");
  await read(get(), w.env, w.waiter, {});
  await w.settle();
  assert.deepEqual(w.points, [{ indexes: ["-:page_read"], blobs: ["hostthis", "page_read", ""], doubles: [1] }]);
});

async function call(cell, op, body) {
  const res = await cell.fetch(new Request(`https://cell/x/${op}`, { method: "POST", body: JSON.stringify(body) }));
  return res.status;
}

test("a paste is a publish once: when it turns ready, not on a replay", async () => {
  const w = world();
  const row = (status) => ({ slug: "abcd2345", identity: OWNER, kind: "html", size: 3, createdAt: 7, updatedAt: 7, status, uploadId: "a".repeat(32) });
  const pending = new Paste({ ...state(new FakeStorage()), waitUntil: w.waiter.waitUntil }, w.env);
  assert.equal(await call(pending, "put", { row: row("pending"), generation: "g1", fingerprint: "f1" }), 204);
  await w.settle();
  assert.deepEqual(w.points, []);
  assert.equal(await call(pending, "status", { generation: "g1", status: "ready" }), 200);
  assert.equal(await call(pending, "status", { generation: "g1", status: "ready" }), 200);
  await w.settle();
  assert.deepEqual(w.points.map((p) => p.blobs.slice(1)), [["active", ""], ["publish", "paste"]]);

  const site = world();
  const ready = new Paste({ ...state(new FakeStorage()), waitUntil: site.waiter.waitUntil }, site.env);
  const put = { row: { ...row("ready"), kind: "site" }, generation: "g1", fingerprint: "f1" };
  assert.equal(await call(ready, "put", put), 204);
  assert.equal(await call(ready, "put", put), 204);
  await site.settle();
  assert.deepEqual(site.points.map((p) => p.blobs.slice(1)), [["active", ""], ["publish", "site"]]);
});
