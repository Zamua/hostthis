import assert from "node:assert/strict";
import test from "node:test";

import { Paste } from "../src/cells/paste.js";
import { FakeStorage, state } from "./harness.js";

function paste(seed) {
  return new Paste(state(new FakeStorage(new Map(seed))));
}

async function created(p, id, fresh = true) {
  assert.equal((await p.roomCreated({ id, subnet: "10.0.0.0/24", at: 1, fresh })).status, 204);
}

async function preflight(p, roomLimit) {
  return (await p.roomPreflight({ appCap: 1 << 30, roomLimit })).status;
}

test("an app may create rooms up to its limit and no more", async () => {
  const p = paste();
  await created(p, "r1");
  assert.equal(await preflight(p, 2), 200);
  await created(p, "r2");
  assert.equal(await preflight(p, 2), 507);
  assert.equal(await preflight(p, 0), 200);
});

test("a create replayed onto an existing room does not count", async () => {
  const p = paste();
  await created(p, "r1");
  await created(p, "r2", false);
  assert.equal(await preflight(p, 2), 200);
});

test("an app older than the counter starts from its rooms that hold bytes", async () => {
  const p = paste([
    ["roomAllocation:a", { version: 1, allocated: 5, target: 5 }],
    ["roomAllocation:b", { version: 1, allocated: 5, target: 5 }],
  ]);
  assert.equal(await preflight(p, 2), 507);
  assert.equal(await preflight(p, 3), 200);
  await created(p, "c");
  assert.equal(await preflight(p, 3), 507);
});
