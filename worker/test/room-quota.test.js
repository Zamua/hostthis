import assert from "node:assert/strict";
import test from "node:test";

import { Identity, Paste } from "../src/index.js";
import { USER_QUOTA_BYTES } from "../src/rooms/policy.js";
import { FakeStorage, state } from "./harness.js";

const ROOM = "11111111-1111-4111-8111-111111111111";
const APP_CAP = 64 * 1024 * 1024;

// One app's Paste cell over owner Identity cells that share storage by name.
function world({ owner = "key:owner", total = 0, ownerBytes = 0 } = {}) {
  const identities = new Map();
  const identityStorage = (name) => {
    if (!identities.has(name)) identities.set(name, new FakeStorage());
    return identities.get(name);
  };
  if (ownerBytes) {
    identityStorage(owner).data.set("entries", { other: { generation: "g", size: ownerBytes, chargedSize: ownerBytes } });
  }
  const link = { lose: 0, down: false };
  const env = {
    IDENTITY: {
      idFromName(name) { return name; },
      get(name) {
        return {
          async fetch(request) {
            if (link.down) throw new Error("identity unreachable");
            const res = await new Identity(state(identityStorage(name)), {}).fetch(request);
            if (link.lose-- > 0) throw new Error("answer lost");
            return res;
          },
        };
      },
    },
  };
  const pasteStorage = new FakeStorage(new Map([["roomAllocated", total]]));
  if (owner) pasteStorage.data.set("row", { slug: "appslug1", status: "ready", kind: "site", identity: owner });
  if (total) pasteStorage.data.set(`roomAllocation:${ROOM}`, { version: 0, allocated: total, target: total });
  const paste = () => new Paste(state(pasteStorage), env);
  return { env, link, pasteStorage, paste, identityStorage };
}

async function decide(w, version, targetBytes) {
  const res = await w.paste().roomDecide({ app: "appslug1", room: ROOM, version, targetBytes, appCap: APP_CAP });
  return { status: res.status, body: await res.json() };
}

async function ownerBytes(w, owner = "key:owner") {
  return (await (await new Identity(state(w.identityStorage(owner)), {}).bytes()).json());
}

test("room growth is charged to the app owner and a shrink releases it", async () => {
  const w = world();
  assert.equal((await decide(w, 1, 1000)).status, 200);
  assert.deepStrictEqual(await ownerBytes(w), { bytes: 1000, roomBytes: 1000 });
  assert.equal((await decide(w, 2, 400)).status, 200);
  assert.deepStrictEqual(await ownerBytes(w), { bytes: 400, roomBytes: 400 });
});

test("growth past the owner's quota is refused and leaves the app as it was", async () => {
  const w = world({ ownerBytes: USER_QUOTA_BYTES - 500 });
  const refused = await decide(w, 1, 1000);
  assert.equal(refused.status, 507);
  assert.equal(refused.body.error, "owner-over-quota");
  assert.equal(w.pasteStorage.data.get("roomAllocated"), 0);
  const replay = await decide(w, 1, 1000);
  assert.deepStrictEqual([replay.status, replay.body.granted, replay.body.version], [507, false, 1]);
  assert.equal((await decide(w, 2, 400)).status, 200);
  assert.equal((await ownerBytes(w)).roomBytes, 400);
});

test("a shrink sent without a ceiling, as a delete is, still releases the charge", async () => {
  const w = world();
  assert.equal((await decide(w, 1, 1000)).status, 200);
  const res = await w.paste().roomDecide({ app: "appslug1", room: ROOM, version: 2, targetBytes: 0, appCap: 0 });
  assert.equal(res.status, 200);
  assert.deepStrictEqual(await ownerBytes(w), { bytes: 0, roomBytes: 0 });
});

test("an upload counts the owner's room bytes against the quota", async () => {
  const w = world();
  assert.equal((await decide(w, 1, 1000)).status, 200);
  const identity = new Identity(state(w.identityStorage("key:owner")), {});
  const res = await identity.reserve({
    slug: "newslug1", size: USER_QUOTA_BYTES - 999, userCap: USER_QUOTA_BYTES, generation: "g1",
    intent: { kind: "create_paste", id: "i1", subject: "newslug1", fingerprint: "f" },
  });
  assert.equal(res.status, 507);
});

test("a deleted app's rooms stay charged to its last owner", async () => {
  const w = world();
  assert.equal((await decide(w, 1, 1000)).status, 200);
  w.pasteStorage.data.delete("row");
  assert.equal((await decide(w, 2, 1500)).status, 200);
  assert.equal((await ownerBytes(w)).roomBytes, 1500);
});

test("a slug that never had an owner is bounded by the app ceiling alone", async () => {
  const w = world({ owner: null });
  assert.equal((await decide(w, 1, 900)).status, 200);
  assert.equal(w.pasteStorage.data.get("roomAllocated"), 900);
});

test("a lost owner answer is replayed, never charged twice", async () => {
  const w = world();
  w.link.lose = 1;
  assert.equal((await decide(w, 1, 1000)).status, 502);
  assert.equal(w.pasteStorage.data.get("roomAllocated"), 0);
  assert.equal((await decide(w, 1, 1000)).status, 200);
  assert.deepStrictEqual(await ownerBytes(w), { bytes: 1000, roomBytes: 1000 });
  w.link.down = true;
  assert.equal((await decide(w, 2, 2000)).status, 502);
  w.link.down = false;
  assert.equal((await decide(w, 2, 2000)).status, 200);
  assert.deepStrictEqual(await ownerBytes(w), { bytes: 2000, roomBytes: 2000 });
});

test("a re-minted app moves its room charge to the new owner", async () => {
  const w = world();
  assert.equal((await decide(w, 1, 1000)).status, 200);
  w.pasteStorage.data.set("row", { ...w.pasteStorage.data.get("row"), identity: "key:next" });
  assert.equal((await decide(w, 2, 1200)).status, 200);
  assert.equal((await ownerBytes(w, "key:owner")).roomBytes, 0);
  assert.equal((await ownerBytes(w, "key:next")).roomBytes, 1200);
});

test("sync charges bytes that predate the rule, even past the quota", async () => {
  const w = world({ total: 5000, ownerBytes: USER_QUOTA_BYTES });
  const res = await (await w.paste().roomChargeSync()).json();
  assert.equal(res.owner, true);
  assert.equal((await ownerBytes(w)).roomBytes, 5000);
  assert.equal((await decide(w, 1, 5001)).body.error, "owner-over-quota");
  assert.equal((await decide(w, 2, 10)).status, 200);
  assert.equal((await ownerBytes(w)).roomBytes, 10);
});

test("with room ceilings off the owner is not charged", async () => {
  const w = world();
  const res = await w.paste().roomDecide({ app: "appslug1", room: ROOM, version: 1, targetBytes: 1000, appCap: 0 });
  assert.equal(res.status, 200);
  assert.equal((await ownerBytes(w)).roomBytes, 0);
});
