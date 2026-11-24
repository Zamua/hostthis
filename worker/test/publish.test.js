import assert from "node:assert/strict";
import test from "node:test";

import worker, { Identity, Paste } from "../src/index.js";
import { PUBLISH_PATH } from "../src/publish.js";
import { FakeStorage, state } from "./harness.js";

const TOKEN = "full-token";
const READ = "read-token";
const ADMIN = "admin-token";
const OWNER = "key:SHA256:owner";

// The internal router over real Identity and Paste cells in memory. Every cell
// request is recorded, and `fail` makes the next calls to an op throw.
function world() {
  const storages = new Map();
  const calls = [];
  const fail = new Map();
  const storageOf = (key) => {
    if (!storages.has(key)) storages.set(key, new FakeStorage());
    return storages.get(key);
  };
  const env = {
    HOSTTHIS_WORKER_TOKEN: TOKEN,
    HOSTTHIS_READ_TOKEN: READ,
    HOSTTHIS_ADMIN_TOKEN: ADMIN,
  };
  const ns = (kind, Cls) => ({
    idFromName: (name) => name,
    get: (name) => ({
      async fetch(request) {
        const op = new URL(request.url).pathname;
        calls.push(op);
        if ((fail.get(op) ?? 0) > 0) {
          fail.set(op, fail.get(op) - 1);
          throw new Error(`${op} unavailable`);
        }
        return new Cls(state(storageOf(`${kind}:${name}`)), env).fetch(request);
      },
    }),
  });
  env.IDENTITY = ns("identity", Identity);
  env.PASTES = ns("paste", Paste);
  return {
    env, calls, fail, storageOf,
    publish(body, token = TOKEN) {
      return worker.fetch(new Request(`https://internal${PUBLISH_PATH}`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token}`, "content-type": "application/json" },
        body: JSON.stringify(body),
      }), env, { waitUntil() {} });
    },
  };
}

function publishBody({ slug = "slugone1", size = 3, userCap = 10, generation = "generation-1", pod } = {}) {
  const body = {
    row: {
      slug, identity: OWNER, generation, status: "pending", kind: "markdown",
      uploadId: "0123456789abcdef0123456789abcdef", size, name: "",
      pinnedVersion: 0, createdAt: 9, updatedAt: 9,
      manifest: { files: [{ path: "", key: "uploads/0123456789abcdef0123456789abcdef/0", size }] },
    },
    generation,
    fingerprint: `fingerprint-${slug}-${generation}`,
    intentId: `create:${slug}:${generation}`,
    userCap,
    now: 9,
  };
  if (pod !== undefined) body.pod = pod;
  return body;
}

async function json(res) {
  return { status: res.status, body: await res.json() };
}

const entries = (w) => w.storageOf(`identity:${OWNER}`).data.get("entries") ?? {};
const intents = (w) => [...w.storageOf(`identity:${OWNER}`).data.keys()].filter((k) => k.startsWith("intent:"));
const rowOf = (w, slug = "slugone1") => w.storageOf(`paste:${slug}`).data.get("row");

test("publish commits the row, the charge and the confirm in one request", async () => {
  const w = world();
  assert.deepEqual(await json(await w.publish(publishBody())), { status: 200, body: { active: 3 } });
  assert.deepEqual(w.calls, ["/identity/reserve", "/paste/put", "/identity/confirm"]);
  assert.equal(rowOf(w).generation, "generation-1");
  assert.equal(entries(w).slugone1.status, "pending");
  assert.equal(entries(w).slugone1.chargedSize, 3);
  assert.deepEqual(intents(w), [], "the create intent is discharged");
});

test("publish answers the owner's charged total after the create", async () => {
  const w = world();
  await w.publish(publishBody({ slug: "slugone1", size: 3 }));
  assert.deepEqual(await json(await w.publish(publishBody({ slug: "slugtwo2", size: 4 }))),
    { status: 200, body: { active: 7 } });
});

test("publish over quota is refused before any row exists", async () => {
  const w = world();
  const res = await json(await w.publish(publishBody({ size: 11 })));
  assert.equal(res.status, 507);
  assert.equal(res.body.error, "over-quota");
  assert.deepEqual(w.calls, ["/identity/reserve"]);
  assert.equal(rowOf(w), undefined);
  assert.deepEqual(entries(w), {});
});

test("publish onto a slug another paste holds releases its charge and answers slug-taken", async () => {
  const w = world();
  w.storageOf("paste:slugone1").data.set("row", { slug: "slugone1", identity: "key:other", generation: "other" });
  const res = await json(await w.publish(publishBody()));
  assert.deepEqual(res, { status: 409, body: { error: "slug-taken" } });
  assert.deepEqual(w.calls, ["/identity/reserve", "/paste/put", "/identity/release"]);
  assert.deepEqual(entries(w), {}, "the reservation is released");
  assert.deepEqual(intents(w), []);
});

test("a repeated publish of a completed create is slug-taken, never a second paste", async () => {
  const w = world();
  assert.equal((await w.publish(publishBody())).status, 200);
  const res = await json(await w.publish(publishBody()));
  assert.deepEqual(res, { status: 409, body: { error: "slug-taken" } });
  assert.equal(entries(w).slugone1.chargedSize, 3, "the first paste keeps its one charge");
  assert.ok(rowOf(w));
});

test("a publish whose put went unanswered once retries it and commits", async () => {
  const w = world();
  w.fail.set("/paste/put", 1);
  assert.equal((await w.publish(publishBody())).status, 200);
  assert.deepEqual(w.calls, ["/identity/reserve", "/paste/put", "/paste/put", "/identity/confirm"]);
  assert.deepEqual(intents(w), []);
});

test("a publish whose put never answers keeps the intent for recovery", async () => {
  const w = world();
  w.fail.set("/paste/put", 2);
  const res = await json(await w.publish(publishBody()));
  assert.deepEqual(res, { status: 502, body: { error: "put-unanswered" } });
  assert.equal(intents(w).length, 1, "the intent resolves the create");
  assert.ok(entries(w).slugone1, "the charge stays until the intent resolves");
});

test("a publish whose confirm goes unanswered is still committed", async () => {
  const w = world();
  w.fail.set("/identity/confirm", 1);
  assert.equal((await w.publish(publishBody())).status, 200);
  assert.ok(rowOf(w));
  assert.equal(intents(w).length, 1, "recovery confirms it");
});

test("publish checks the pod link the upload id was minted for, in the reserve", async () => {
  const w = world();
  assert.equal((await w.publish(publishBody({ slug: "nopod111", pod: "" }))).status, 200);

  w.storageOf(`identity:${OWNER}`).data.set("pod", { id: "0123456789abcdef", webid: "w", storage: "s", linked: 1 });
  const moved = await json(await w.publish(publishBody({ slug: "moved111", pod: "" })));
  assert.deepEqual(moved, { status: 409, body: { error: "pod-moved", pod: "0123456789abcdef" } });
  assert.equal(entries(w).moved111, undefined, "nothing is reserved");
  assert.equal(rowOf(w, "moved111"), undefined);

  assert.equal((await w.publish(publishBody({ slug: "inpod111", pod: "0123456789abcdef" }))).status, 200);
  // A publish that names no pod is not checked, as a deploy's create is not.
  assert.equal((await w.publish(publishBody({ slug: "unchkd11" }))).status, 200);
});

test("publish refuses a malformed body before touching a cell", async () => {
  const w = world();
  for (const body of [{}, { ...publishBody(), generation: "" }, { ...publishBody(), intentId: 3 },
    { ...publishBody(), pod: 7 }, { ...publishBody(), row: { ...publishBody().row, identity: "" } }]) {
    const res = await w.publish(body);
    assert.equal(res.status, 400);
  }
  assert.deepEqual(w.calls, []);
});

test("only the full token may publish", async () => {
  for (const token of [READ, ADMIN, "wrong"]) {
    const w = world();
    assert.equal((await w.publish(publishBody(), token)).status, 401, token);
    assert.deepEqual(w.calls, [], token);
  }
});
