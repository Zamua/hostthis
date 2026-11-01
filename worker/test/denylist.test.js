import assert from "node:assert/strict";
import test from "node:test";

import { DENYLIST_CAP, Denylist, canonicalEntry } from "../src/cells/denylist.js";
import { FakeStorage, state } from "./harness.js";

const KEY = "SHA256:" + "ab".repeat(32);

function cell() {
  return new Denylist(state(new FakeStorage()));
}

async function op(c, name, body) {
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  const res = await c.fetch(new Request(`https://cell/denylist/${name}`, init));
  return { status: res.status, body: await res.json() };
}

test("canonicalEntry accepts a key fingerprint and masks a network", () => {
  assert.equal(canonicalEntry(KEY), KEY);
  assert.equal(canonicalEntry("203.0.113.77/24"), "203.0.113.0/24");
  assert.equal(canonicalEntry("10.1.2.3/8"), "10.0.0.0/8");
  assert.equal(canonicalEntry("2001:DB8:aa:bb::1/48"), "2001:db8:aa::/48");
  assert.equal(canonicalEntry("2001:db8:0:0:1:0:0:1/128"), "2001:db8::1:0:0:1/128");
  assert.equal(canonicalEntry("::/16"), "::/16");
});

test("canonicalEntry refuses anything else", () => {
  for (const bad of [
    "", "SHA256:" + "AB".repeat(32), "SHA256:abc", "key:" + KEY,
    "203.0.113.0", "203.0.113.0/7", "203.0.113.0/33", "256.0.0.0/24", "1.2.3/24",
    "2001:db8::/15", "2001:db8::/129", "2001:db8:::1/64", "1:2:3:4:5:6:7:8:9/64", "2001:db8::g/64",
    "01.2.3.4/24", " 203.0.113.0/24",
  ]) {
    assert.equal(canonicalEntry(bad), null, bad);
  }
});

test("add, list and remove round-trip in canonical form", async () => {
  const c = cell();
  assert.equal((await op(c, "add", { entry: KEY, reason: "abuse" })).status, 200);
  assert.equal((await op(c, "add", { entry: "198.51.100.9/24", reason: "" })).status, 200);
  const { body } = await op(c, "list");
  assert.deepEqual(body.entries.map((e) => [e.entry, e.reason]).sort(),
    [["198.51.100.0/24", ""], [KEY, "abuse"]]);
  assert.ok(body.entries.every((e) => Number.isSafeInteger(e.addedAt) && e.addedAt > 0));
  assert.equal((await op(c, "remove", { entry: "198.51.100.200/24" })).status, 200);
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.entry), [KEY]);
});

test("adding a present entry replaces its reason and keeps one row", async () => {
  const c = cell();
  await op(c, "add", { entry: KEY, reason: "first" });
  await op(c, "add", { entry: KEY, reason: "second" });
  assert.deepEqual((await op(c, "list")).body.entries.map((e) => e.reason), ["second"]);
});

test("removing an absent entry succeeds", async () => {
  const res = await op(cell(), "remove", { entry: KEY });
  assert.equal(res.status, 200);
  assert.equal(res.body.removed, false);
});

test("a malformed entry or reason is refused", async () => {
  const c = cell();
  for (const body of [{ entry: "nope", reason: "" }, { entry: KEY, reason: "x".repeat(201) }, { entry: KEY }]) {
    const res = await op(c, "add", body);
    assert.equal(res.status, 400, JSON.stringify(body));
    assert.equal(res.body.error, "invalid-entry");
  }
  assert.equal((await op(c, "remove", { entry: "nope" })).status, 400);
  assert.deepEqual((await op(c, "list")).body.entries, []);
});

test("the list stops at its cap, and replacing an entry still works there", async () => {
  const c = cell();
  for (let i = 0; i < DENYLIST_CAP; i++) {
    await op(c, "add", { entry: `10.${i >> 8}.${i & 255}.0/24`, reason: "" });
  }
  const full = await op(c, "add", { entry: KEY, reason: "" });
  assert.equal(full.status, 507);
  assert.equal(full.body.error, "denylist-full");
  assert.equal((await op(c, "add", { entry: "10.0.0.0/24", reason: "again" })).status, 200);
});
