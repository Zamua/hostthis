// The artifact accounting saga (docs/SPEC.md "Artifact accounting on celld"):
// the lifecycle of the Paste cell's one pending cross-cell operation, as data
// and one pure transition. The Paste cell performs the effects; nothing else
// decides what a pending operation does next.
//
// A pending record is { kind, stage, attempts, opId, slug, owner, generation,
// version, ... }. Its kind fixes the stages it passes through:
//
//   projection                     project
//   append, abandon, deleteVersion,
//   pin                            decide -> project
//   remove, fail                   decide -> drop
//
// decide asks the owner's Identity cell for the charge, project refreshes the
// owner listing, drop removes the listing entry.

export const DECIDE = "decide";
export const PROJECT = "project";
export const DROP = "drop";

// The stage after a granted decision, per kind; projection never decides.
const AFTER_DECIDE = {
  append: PROJECT,
  abandon: PROJECT,
  deleteVersion: PROJECT,
  pin: PROJECT,
  remove: DROP,
  fail: DROP,
};
const KINDS = new Set([...Object.keys(AFTER_DECIDE), "projection"]);

// A new pending operation of kind, at its first stage. No attempts field: an
// absent count is zero.
export function startPending(kind, fields) {
  if (!KINDS.has(kind)) {
    throw new Error(`unknown artifact operation ${kind}`);
  }
  return { kind, stage: kind === "projection" ? PROJECT : DECIDE, ...fields };
}

// Whether a record read inside a transaction is still the operation a step
// started from, at the stage it expects.
export function samePending(current, pending, stage) {
  return Boolean(current) && current.opId === pending.opId &&
    current.generation === pending.generation && current.stage === stage;
}

// The pending record moved to its next stage with a fresh attempt count.
export function advance(pending) {
  return { ...pending, stage: AFTER_DECIDE[pending.kind], attempts: 0 };
}

// Identity's answer to a decide call, as an event.
export function decisionEvent(pending, status, decision) {
  if (status === 404 && decision?.error === "artifact-absent") {
    return "absent";
  }
  if (status === 507 && decision?.granted === false && decision.version === pending.version) {
    return "refused";
  }
  if (status >= 200 && status < 300 && decision?.granted === true &&
      decision.version === pending.version && decision.allocated === pending.target) {
    return "granted";
  }
  return "conflict";
}

const retry = (outcome) => ({ effect: "retry", outcome });

// The effect a pending operation runs on event, and the outcome its caller
// sees. Events: unreachable (the call threw), conflict (any answer that is
// neither a grant nor a definite refusal), absent, refused, granted, tooBig
// (the granted version cannot be stored), noRow, projected, dropped.
//
// Effects: retry (count an attempt, back off), settleAbsent (the listing is
// already gone: finish without a drop), refuse (persist the quota refusal),
// publish (an append's version goes live, stage -> project), abandon (an
// append too large to store becomes a release), advance (stage -> next),
// clear (done), finishDrop (done once the drop's local fence commits).
export function nextPending(pending, event) {
  if (!KINDS.has(pending?.kind)) {
    return retry("invalid");
  }
  if (event === "unreachable" || event === "noRow") {
    return retry("unavailable");
  }
  switch (pending.stage) {
    case DECIDE:
      if (pending.kind === "projection") {
        return retry("invalid");
      }
      switch (event) {
        case "absent":
          return AFTER_DECIDE[pending.kind] === DROP ? { effect: "settleAbsent" } : retry("conflict");
        case "refused":
          return { effect: "refuse", outcome: "refused" };
        case "granted":
          return { effect: pending.kind === "append" ? "publish" : "advance" };
        case "tooBig":
          return pending.kind === "append" ? { effect: "abandon" } : retry("invalid");
        case "conflict":
          return retry("conflict");
        default:
          return retry("invalid");
      }
    case PROJECT:
      if (event === "projected") {
        return { effect: "clear", outcome: "complete" };
      }
      return retry(event === "conflict" ? "conflict" : "invalid");
    case DROP:
      if (AFTER_DECIDE[pending.kind] !== DROP) {
        return retry("invalid");
      }
      if (event === "dropped") {
        return { effect: "finishDrop", outcome: "complete" };
      }
      return retry(event === "conflict" ? "conflict" : "invalid");
    default:
      return retry("invalid");
  }
}
