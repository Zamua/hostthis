import assert from "node:assert/strict";
import test from "node:test";

import { Identity, SUBNET_RETENTION_MS } from "../src/cells/identity.js";
import { FakeStorage, state } from "./harness.js";

const DAY = 86_400_000;
const T = 1_800_000_000_000;

async function call(cell, path, body) {
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  const res = await cell.fetch(new Request(`https://cell/identity/${path}`, init));
  return res.status === 204 ? null : res.json();
}

test("the subnet index counts the window but keeps 30 days for owner bans", async () => {
  const cell = new Identity(state(new FakeStorage()), {});
  await call(cell, "notesubnet?scope=k", { subnet: "203.0.113.0/24", now: T });
  await call(cell, "notesubnet?scope=k", { subnet: "198.51.100.0/24", now: T + 2 * DAY });
  const now = T + 3 * DAY;
  assert.equal((await call(cell, `subnets?scope=k&now=${now}&window=${DAY}`)).count, 0);
  assert.deepEqual((await call(cell, `subnetlist?scope=k&now=${now}`)).subnets.map((s) => s.subnet),
    ["198.51.100.0/24", "203.0.113.0/24"]);
  const later = T + SUBNET_RETENTION_MS;
  await call(cell, `subnets?scope=k&now=${later}&window=${DAY}`);
  assert.deepEqual((await call(cell, `subnetlist?scope=k&now=${later}`)).subnets.map((s) => s.subnet), ["198.51.100.0/24"]);
});

test("a re-introduced pair moves its entry forward and counts again", async () => {
  const cell = new Identity(state(new FakeStorage()), {});
  await call(cell, "notesubnet?scope=k", { subnet: "203.0.113.0/24", now: T });
  await call(cell, "notesubnet?scope=k", { subnet: "203.0.113.0/24", now: T + 2 * DAY });
  assert.equal((await call(cell, `subnets?scope=k&now=${T + 2 * DAY + 1}&window=${DAY}`)).count, 1);
  assert.deepEqual((await call(cell, `subnetlist?scope=k&now=${T + 2 * DAY + 1}`)).subnets,
    [{ subnet: "203.0.113.0/24", at: T + 2 * DAY }]);
});
