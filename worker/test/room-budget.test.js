import assert from "node:assert/strict";
import test from "node:test";

import { Room } from "../src/index.js";
import { alarmDeadline, budgetKind } from "../src/cells/room.js";
import { FakeStorage, roomDoc, state } from "./harness.js";

test("a budget operation is idle, growing or releasing", () => {
  assert.equal(budgetKind(undefined), "idle");
  assert.equal(budgetKind({ targetBytes: 9, mutation: { key: "k" } }), "growing");
  assert.equal(budgetKind({ targetBytes: 2 }), "releasing");
});

test("the alarm is the earlier of the budget retry and the next push", () => {
  assert.equal(alarmDeadline(undefined, undefined), null);
  assert.equal(alarmDeadline({ retryAt: 50 }, { nextFireAt: null }), 50);
  assert.equal(alarmDeadline(undefined, { nextFireAt: 80 }), 80);
  assert.equal(alarmDeadline({ retryAt: 90 }, { nextFireAt: 80 }), 80);
  assert.equal(alarmDeadline({}, { nextFireAt: 80 }), 80);
});

test("a push save keeps the retry instant of a budget recorded without one", async () => {
  const doc = roomDoc();
  doc.pending = { targetBytes: 2 };
  const storage = new FakeStorage(new Map([["state", doc]]), false, 5_000);
  const room = new Room(state(storage), {});
  await room.savePush({ subscriptions: [], schedule: { items: [{ due: 9_000 }] }, lastTestAt: 0 });
  assert.equal(storage.alarm, 5_000);
});
