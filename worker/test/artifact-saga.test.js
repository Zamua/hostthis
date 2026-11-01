import assert from "node:assert/strict";
import test from "node:test";

import { DECIDE, DROP, PROJECT, advance, decisionEvent, nextPending, samePending, startPending } from "../src/cells/artifact-saga.js";

const KINDS = ["projection", "append", "abandon", "deleteVersion", "pin", "remove", "fail"];
const EVENTS = ["unreachable", "noRow", "conflict", "absent", "refused", "granted", "tooBig", "projected", "dropped", "invalid"];
const DROPS = new Set(["remove", "fail"]);

const at = (kind, stage) => ({ kind, stage, opId: "op", generation: "g", version: 2, target: 10 });

// The whole table: every kind at every stage under every event.
const EXPECTED = (kind, stage, event) => {
  if (event === "unreachable" || event === "noRow") return { effect: "retry", outcome: "unavailable" };
  if (stage === DECIDE) {
    if (kind === "projection") return { effect: "retry", outcome: "invalid" };
    if (event === "absent") return DROPS.has(kind) ? { effect: "settleAbsent" } : { effect: "retry", outcome: "conflict" };
    if (event === "refused") return { effect: "refuse", outcome: "refused" };
    if (event === "granted") return { effect: kind === "append" ? "publish" : "advance" };
    if (event === "tooBig") return kind === "append" ? { effect: "abandon" } : { effect: "retry", outcome: "invalid" };
    if (event === "conflict") return { effect: "retry", outcome: "conflict" };
    return { effect: "retry", outcome: "invalid" };
  }
  if (stage === PROJECT) {
    if (event === "projected") return { effect: "clear", outcome: "complete" };
    return { effect: "retry", outcome: event === "conflict" ? "conflict" : "invalid" };
  }
  if (stage === DROP) {
    if (!DROPS.has(kind)) return { effect: "retry", outcome: "invalid" };
    if (event === "dropped") return { effect: "finishDrop", outcome: "complete" };
    return { effect: "retry", outcome: event === "conflict" ? "conflict" : "invalid" };
  }
  return { effect: "retry", outcome: "invalid" };
};

test("every kind, stage and event has one decided step", () => {
  for (const kind of KINDS) {
    for (const stage of [DECIDE, PROJECT, DROP, "bogus"]) {
      for (const event of EVENTS) {
        assert.deepStrictEqual(nextPending(at(kind, stage), event), EXPECTED(kind, stage, event), `${kind}/${stage}/${event}`);
      }
    }
  }
});

test("an unknown kind is an invalid retry, never a silent success", () => {
  for (const event of EVENTS) {
    assert.deepStrictEqual(nextPending(at("rename", DECIDE), event), { effect: "retry", outcome: "invalid" });
  }
  assert.throws(() => startPending("rename", {}));
});

test("each kind starts at its first stage and advances to its second", () => {
  assert.equal(startPending("projection", {}).stage, PROJECT);
  for (const kind of KINDS.filter((k) => k !== "projection")) {
    const started = startPending(kind, { opId: "o" });
    assert.equal(started.stage, DECIDE);
    assert.equal("attempts" in started, false);
    assert.deepStrictEqual(advance({ ...started, attempts: 4 }), { ...started, stage: DROPS.has(kind) ? DROP : PROJECT, attempts: 0 });
  }
});

test("a decide answer becomes one event", () => {
  const p = at("pin", DECIDE);
  assert.equal(decisionEvent(p, 404, { error: "artifact-absent" }), "absent");
  assert.equal(decisionEvent(p, 404, { error: "other" }), "conflict");
  assert.equal(decisionEvent(p, 507, { granted: false, version: 2 }), "refused");
  assert.equal(decisionEvent(p, 507, { granted: false, version: 1 }), "conflict");
  assert.equal(decisionEvent(p, 200, { granted: true, version: 2, allocated: 10 }), "granted");
  assert.equal(decisionEvent(p, 200, { granted: true, version: 2, allocated: 9 }), "conflict");
  assert.equal(decisionEvent(p, 409, { error: "stale-version" }), "conflict");
});

test("a step's guard holds only for the same operation at the same stage", () => {
  const p = at("remove", DECIDE);
  assert.equal(samePending(p, p, DECIDE), true);
  assert.equal(samePending(undefined, p, DECIDE), false);
  assert.equal(samePending({ ...p, opId: "x" }, p, DECIDE), false);
  assert.equal(samePending({ ...p, generation: "h" }, p, DECIDE), false);
  assert.equal(samePending(p, p, DROP), false);
});

// A model of the cell's shell: performs each effect on the record alone.
function run(kind, events) {
  let pending = startPending(kind, { opId: "o", generation: "g", version: 1, target: 5 });
  const trail = [];
  for (const event of events) {
    const step = nextPending(pending, event);
    trail.push(step.effect);
    if (step.effect === "retry") pending = { ...pending, attempts: (pending.attempts ?? 0) + 1 };
    else if (step.effect === "advance" || step.effect === "publish") pending = advance(pending);
    else if (step.effect === "abandon") pending = { ...pending, kind: "abandon", version: pending.version + 1, attempts: 0 };
    else return { done: step.effect, trail, pending };
  }
  return { done: null, trail, pending };
}

// Deterministic pseudo-random event streams.
function* streams(seed, count) {
  let x = seed;
  const rand = () => (x = (x * 1103515245 + 12345) % 2147483648) / 2147483648;
  for (let n = 0; n < count; n++) {
    const length = 1 + Math.floor(rand() * 12);
    yield Array.from({ length }, () => EVENTS[Math.floor(rand() * EVENTS.length)]);
  }
}

test("random event streams never break the lifecycle", () => {
  for (const kind of KINDS) {
    for (const events of streams(kind.length * 7919, 2000)) {
      const { done, trail, pending } = run(kind, events);
      // remove and fail never project; the rest never drop.
      assert.notEqual(pending.stage, DROPS.has(pending.kind) ? PROJECT : DROP, `${kind}: ${events}`);
      // A stage advance always restarts the attempt count.
      if (trail.at(-1) === "advance" || trail.at(-1) === "publish") assert.equal(pending.attempts, 0);
      // An operation ends only through its kind's own terminal effects.
      if (done) {
        const allowed = DROPS.has(kind) ? ["settleAbsent", "refuse", "finishDrop"] : ["refuse", "clear"];
        assert.ok(allowed.includes(done), `${kind} ended by ${done}: ${events}`);
      }
    }
  }
});

test("retries alone never end an operation, and the happy path always does", () => {
  for (const kind of KINDS) {
    assert.equal(run(kind, Array(50).fill("unreachable")).done, null);
    const happy = kind === "projection" ? ["projected"] : DROPS.has(kind) ? ["granted", "dropped"] : ["granted", "projected"];
    assert.equal(run(kind, ["conflict", "unreachable", ...happy]).done, DROPS.has(kind) ? "finishDrop" : "clear");
  }
});

test("a pending row's status moves only to ready or failed", async () => {
  const { Paste } = await import("../src/index.js");
  const { FakeStorage, state } = await import("./harness.js");
  const storage = new FakeStorage(new Map([["row", { slug: "s", status: "pending", generation: "g" }]]));
  const res = await new Paste(state(storage)).setStatus({ generation: "g", status: "archived" });
  assert.equal(res.status, 400);
  assert.equal(storage.data.get("row").status, "pending");
});
