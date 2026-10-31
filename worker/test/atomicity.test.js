import assert from "node:assert/strict";
import test from "node:test";

import worker, { Identity, Paste, Room, Subnet } from "../src/index.js";
import { CREATE_INTENT_GRACE_MS } from "../src/cells/records.js";
import {
  FakeStorage, PASTE_CELL_ID, ROOM_CELL_ID, ROOM_ID, SimulatedCrash,
  clone, fakeCellID, putBody, roomDoc, state,
} from "./harness.js";

async function assertCrashAtomic({ seed, invoke }) {
  const initial = new FakeStorage(seed).data;
  const reference = new FakeStorage(seed);
  await invoke(reference);
  const complete = reference.data;

  for (let boundary = 1; boundary <= reference.commits; boundary++) {
    const storage = new FakeStorage(seed);
    storage.crashAfter = boundary;
    try {
      await invoke(storage);
    } catch (error) {
      assert.ok(error instanceof SimulatedCrash, `unexpected error at boundary ${boundary}: ${error}`);
    }
    const isInitial = mapEqual(storage.data, initial);
    const isComplete = mapEqual(storage.data, complete);
    assert.ok(isInitial || isComplete,
      `commit boundary ${boundary} left partial state:\n${formatMap(storage.data)}`);
  }
}

// Sweeps every local commit boundary of a paste operation: a reference run
// counts the commits, then each boundary gets a fresh harness crashed there,
// recovered through the alarm it armed, and handed to `converged` with the
// run's name. Counting from the reference run keeps a newly added commit inside
// the sweep.
async function assertCrashConverges({ harness, invoke, converged }) {
  const reference = harness();
  await invoke(reference, "reference");
  const commits = reference.pasteStorage.commits;
  assert.ok(commits > 0, "reference run committed nothing");
  for (let boundary = 1; boundary <= commits; boundary++) {
    const h = harness();
    h.pasteStorage.crashAfter = boundary;
    await assert.rejects(() => invoke(h, `crash-${boundary}`), SimulatedCrash);
    h.pasteStorage.commits = 0;
    h.pasteStorage.crashAfter = Infinity;
    if (h.pasteStorage.alarm !== null) {
      await h.paste().alarm();
    }
    await converged(h, `crash-${boundary}`);
  }
}

function mapEqual(left, right) {
  try {
    assert.deepStrictEqual(left, right);
    return true;
  } catch {
    return false;
  }
}

function formatMap(value) {
  return JSON.stringify(Object.fromEntries(value), null, 2);
}

async function listing(storage) {
  return (await responseJSON(await new Identity(state(storage)).list())).body;
}

async function entryOf(storage, slug = "slugone1") {
  return (await listing(storage)).find((entry) => entry.slug === slug);
}

async function rowOf(paste) {
  const response = await paste.get();
  return response.status === 404 ? undefined : response.json();
}

// Oldest first.
async function versionsOf(paste) {
  return (await responseJSON(await paste.listVersions())).body.reverse();
}

// A mutation under a fresh operation id answers 423 exactly while another
// operation's work is pending in the cell; this one names no version, so it
// writes nothing.
async function holdsPending(paste, generation = "generation-1") {
  const probe = await paste.deleteVersion({ opId: "probe-pending", generation, ver: 0 });
  return probe.status === 423;
}

// Granted only while the paste's accounting version agrees with Identity's.
async function nextAppendGranted(h) {
  return (await h.paste().append({ ...appendBody("append-next"), size: 0 })).status === 200;
}

// A put of a new incarnation is refused while the removed one is unreleased.
async function putIncarnation(h, generation = "generation-2") {
  return (await h.paste().put({
    generation,
    fingerprint: `fingerprint-${generation}`,
    row: {
      slug: "slugone1", identity: "other", kind: "html",
      size: 1, status: "ready", createdAt: 9, updatedAt: 9,
    },
  })).status;
}

function serialState(storage) {
  let tail = Promise.resolve();
  return {
    storage,
    acceptWebSocket() {},
    getWebSockets() { return []; },
    blockConcurrencyWhile(callback) {
      const run = tail.then(callback, callback);
      tail = run.catch(() => {});
      return run;
    },
  };
}

test("Identity.reserve commits quota, first-seen, and intent together", async (t) => {
  const realNow = Date.now;
  Date.now = () => 1_000_000;
  t.after(() => { Date.now = realNow; });
  const seed = new Map([["entries", {
    existing1: { size: 2, status: "ready", at: 1, updatedAt: 1 },
  }]]);
  const body = {
    slug: "newslug2",
    generation: "generation-1",
    size: 3,
    userCap: 10,
    status: "pending",
    now: 9,
    updatedAt: 9,
    intent: {
      id: "intent-1", kind: "create_paste", subject: "newslug2",
      fingerprint: "fingerprint-1", reached: [],
    },
  };
  await assertCrashAtomic({
    seed,
    invoke: (storage) => new Identity(state(storage)).reserve(body),
  });
});

test("Identity.reserve requires a valid create intent before charging", async () => {
  for (const [name, intent] of [
    ["missing", undefined],
    ["malformed", { id: "intent-1", kind: "create_paste", subject: "newslug2" }],
  ]) {
    const storage = new FakeStorage();
    const identity = new Identity(state(storage));
    const response = await identity.reserve({
      slug: "newslug2", generation: "generation-1", size: 3, userCap: 10, intent,
    });
    assert.equal(response.status, 400, name);
    assert.deepStrictEqual(await listing(storage), [], name);
    assert.equal(storage.alarm, null, name);
  }
});

test("Identity.reserve replays only an unresolved create and refuses a completed one", async () => {
  const storage = new FakeStorage();
  const identity = new Identity(state(storage));
  const body = {
    slug: "slugone1", generation: "generation-1", size: 3, userCap: 10,
    status: "pending", now: 1,
    intent: {
      id: "create:slugone1:generation-1", kind: "create_paste",
      subject: "slugone1", fingerprint: "fingerprint-1",
    },
  };
  assert.equal((await identity.reserve(body)).status, 200);
  const replay = await responseJSON(await identity.reserve(structuredClone(body)));
  assert.equal(replay.status, 200);
  assert.equal(replay.body.replayed, true);

  assert.equal((await identity.confirm({
    slug: "slugone1", generation: "generation-1", status: "ready",
    intentId: "create:slugone1:generation-1",
  })).status, 204);
  const repeated = await responseJSON(await identity.reserve(structuredClone(body)));
  assert.equal(repeated.status, 409);
  assert.equal(repeated.body.error, "slug-taken");
  assert.equal((await entryOf(storage)).status, "ready");
});

test("Identity serializes concurrent quota reservations", async () => {
  const storage = new FakeStorage();
  const identity = new Identity(serialState(storage));
  const reserve = (slug) => identity.fetch(new Request("https://cell/identity/reserve", {
    method: "POST",
    body: JSON.stringify({
      slug, generation: `generation-${slug}`, size: 6, userCap: 10, now: 1,
      intent: {
        id: `create:${slug}:generation-${slug}`, kind: "create_paste", subject: slug,
        fingerprint: `fingerprint-${slug}`,
      },
    }),
  }));
  const responses = await Promise.all([reserve("slugone1"), reserve("slugtwo2")]);
  assert.deepStrictEqual(responses.map((response) => response.status).sort(), [200, 507]);
  const entries = await listing(storage);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].chargedSize, 6);
});

test("Identity rejects a competing generation for one owner slug", async () => {
  const storage = new FakeStorage();
  const identity = new Identity(state(storage));
  const reserve = (generation) => identity.reserve({
    slug: "slugone1", generation, size: 3, userCap: 10,
    status: "pending", now: 1,
    intent: {
      id: `create:slugone1:${generation}`, kind: "create_paste", subject: "slugone1",
      fingerprint: `fingerprint-${generation}`,
    },
  });

  assert.equal((await reserve("generation-a")).status, 200);
  assert.equal((await reserve("generation-b")).status, 409);
  assert.equal((await entryOf(storage)).generation, "generation-a");

  assert.equal((await identity.confirm({
    slug: "slugone1", generation: "generation-b", status: "ready",
  })).status, 409);
  assert.equal((await identity.release({
    slug: "slugone1", generation: "generation-b",
  })).status, 409);
  const entry = await entryOf(storage);
  assert.equal(entry.generation, "generation-a");
  assert.equal(entry.status, "pending");
});

// An empty or non-string generation is refused before any read.
test("Identity refuses a malformed generation without touching storage", async () => {
  const seed = new Map([["entries", { slugone1: {
    generation: "generation-a", size: 3, status: "pending", at: 1, updatedAt: 1,
  } }]]);
  for (const generation of [undefined, "", 7, null]) {
    const storage = new FakeStorage(seed);
    const identity = new Identity(state(storage));
    const body = { slug: "slugone1", generation, status: "ready" };
    assert.equal((await identity.confirm(body)).status, 400, String(generation));
    assert.equal((await identity.release(body)).status, 400, String(generation));
    assert.equal(storage.commits, 0, String(generation));
    assert.deepStrictEqual(storage.data, new FakeStorage(seed).data, String(generation));
  }
});

test("Identity.confirm commits readiness and intent removal together", async () => {
  const seed = new Map([
    ["entries", { newslug2: {
      generation: "generation-1", size: 3, status: "pending", at: 9, updatedAt: 9,
    } }],
    ["intent:intent-1", { kind: "create", subject: "newslug2", reached: [] }],
  ]);
  await assertCrashAtomic({
    seed,
    invoke: (storage) => new Identity(state(storage)).confirm({
      slug: "newslug2", generation: "generation-1",
      status: "ready", intentId: "intent-1",
    }),
  });
});

test("Identity.release commits charge and intent removal together", async () => {
  const seed = new Map([
    ["entries", { newslug2: {
      generation: "generation-1", size: 3, status: "pending", at: 9, updatedAt: 9,
    } }],
    ["intent:intent-1", { kind: "create", subject: "newslug2", reached: [] }],
  ]);
  await assertCrashAtomic({
    seed,
    invoke: (storage) => new Identity(state(storage)).release({
      slug: "newslug2", generation: "generation-1", intentId: "intent-1",
    }),
  });
});

function createIntentHarness() {
  const identityStorage = new FakeStorage();
  const pasteStorage = new FakeStorage();
  const paste = new Paste(state(pasteStorage));
  const transport = { failGet: 0 };
  const endpoint = {
    async fetch(request) {
      if (transport.failGet-- > 0) {
        throw new Error("paste unavailable");
      }
      return paste.fetch(request);
    },
  };
  const env = {
    PASTES: {
      idFromName(name) { return name; },
      get() { return endpoint; },
    },
  };
  return {
    identityStorage,
    pasteStorage,
    paste,
    transport,
    identity: new Identity(state(identityStorage), env),
  };
}

function createIntentBody() {
  return {
    slug: "newslug2", generation: "generation-1", size: 3, userCap: 10,
    status: "pending", now: 9, updatedAt: 9,
    intent: {
      id: "create:newslug2:generation-1", kind: "create_paste",
      subject: "newslug2", fingerprint: "fingerprint-1", startedAt: 9,
    },
  };
}

// Moves the cell clock past the recovery grace so alarm() acts on a create.
function passGrace(t) {
  const realNow = Date.now;
  const later = realNow() + CREATE_INTENT_GRACE_MS + 1;
  Date.now = () => later;
  t.after(() => { Date.now = realNow; });
}

// An unresolved create replays; a discharged one is a duplicate insert.
async function intentRetained(h, body) {
  const replay = await responseJSON(await h.identity.reserve(structuredClone(body)));
  if (replay.status === 200 && replay.body.replayed === true) {
    return true;
  }
  assert.deepStrictEqual(replay, { status: 409, body: { error: "slug-taken" } });
  return false;
}

test("Identity create alarm confirms a matching paste generation", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);
  assert.notEqual(h.identityStorage.alarm, null);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: body.intent.fingerprint,
    row: {
      slug: body.slug, identity: "owner", status: "pending", kind: "html",
      size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 204);

  passGrace(t);
  await h.identity.alarm();
  assert.equal((await entryOf(h.identityStorage, body.slug)).status, "pending");
  assert.equal(await intentRetained(h, body), false);
  assert.equal(h.identityStorage.alarm, null);
});

test("Identity create alarm releases a definitely absent paste", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

  passGrace(t);
  await h.identity.alarm();
  assert.deepStrictEqual(await listing(h.identityStorage), []);
  assert.deepStrictEqual((await responseJSON(await h.identity.bytes())).body, { bytes: 0 });
  assert.equal(h.identityStorage.alarm, null);
});

test("Identity create alarm retains intent across a transient paste failure", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);
  h.transport.failGet = 1;

  passGrace(t);
  await h.identity.alarm();
  assert.equal(await intentRetained(h, body), true);
  assert.equal((await entryOf(h.identityStorage, body.slug)).generation, body.generation);
  assert.notEqual(h.identityStorage.alarm, null);
});

test("Identity create alarm fences a late put before releasing quota", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

  passGrace(t);
  await h.identity.alarm();
  assert.equal(await entryOf(h.identityStorage, body.slug), undefined);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: body.intent.fingerprint,
    row: {
      slug: body.slug, identity: "owner", status: "pending", kind: "html",
      size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 409);
  assert.equal(await rowOf(h.paste), undefined);
});

test("Identity create alarm confirms the Paste row status monotonically", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: body.intent.fingerprint,
    row: {
      slug: body.slug, identity: "owner", status: "ready", kind: "html",
      size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 204);
  assert.equal((await h.identity.confirm({
    slug: body.slug, generation: body.generation, status: "ready",
  })).status, 204);

  passGrace(t);
  await h.identity.alarm();
  assert.equal((await entryOf(h.identityStorage, body.slug)).status, "ready");
  assert.equal(await intentRetained(h, body), false);
});

test("Identity create alarm retains a conflicting same-generation row", async (t) => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: "fingerprint-conflict",
    row: {
      slug: body.slug, identity: "other", status: "ready", kind: "markdown",
      size: 99, createdAt: 10, updatedAt: 10,
    },
  })).status, 204);

  passGrace(t);
  await h.identity.alarm();
  assert.equal(await intentRetained(h, body), true);
  assert.equal((await entryOf(h.identityStorage, body.slug)).status, "pending");
  assert.notEqual(h.identityStorage.alarm, null);
});

test("Identity create alarm leaves an in-flight create alone until the grace elapses", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  const before = Date.now();
  assert.equal((await h.identity.reserve(body)).status, 200);
  assert.ok(h.identityStorage.alarm >= before + 30_000);

  await h.identity.alarm();
  assert.equal(await intentRetained(h, body), true);
  assert.equal((await entryOf(h.identityStorage, body.slug)).generation, body.generation);
  assert.ok(h.identityStorage.alarm >= before + 30_000);
});

test("Identity create alarm retains malformed and unknown intents", async () => {
  for (const [name, intent] of [
    ["unknown", { kind: "create_passte", subject: "newslug2", generation: "generation-1" }],
    ["malformed", { kind: "create_paste", subject: "newslug2", generation: "generation-1" }],
  ]) {
    const storage = new FakeStorage(new Map([[`intent:${name}`, intent]]), false, 1);
    const identity = new Identity(state(storage), { PASTES: { idFromName() {}, get() {} } });
    await identity.alarm();
    assert.notEqual(storage.data.get(`intent:${name}`), undefined, name);
    assert.notEqual(storage.alarm, null, name);
  }
});

test("Paste.put replays only the exact creation fingerprint", async () => {
  const storage = new FakeStorage();
  const paste = new Paste(state(storage));
  const original = {
    slug: "newslug2", identity: "owner", kind: "html",
    size: 3, createdAt: 7, updatedAt: 7, status: "pending",
  };
  assert.equal((await paste.put({
    row: original, generation: "generation-1", fingerprint: "fingerprint-1",
  })).status, 204);
  const replay = { ...original, size: 99 };
  assert.equal((await paste.put({
    row: original, generation: "generation-1", fingerprint: "fingerprint-1",
  })).status, 204);
  assert.equal((await paste.put({
    row: replay, generation: "generation-1", fingerprint: "fingerprint-other",
  })).status, 409);
  assert.equal((await rowOf(paste)).size, 3);
  assert.equal((await versionsOf(paste)).length, 1);
  assert.equal((await paste.put({
    row: replay, generation: "generation-2", fingerprint: "fingerprint-2",
  })).status, 409);
});

test("Paste.put commits row, version seed, and counter together", async () => {
  const row = {
    identity: "owner", kind: "html", size: 3,
    createdAt: 7, updatedAt: 7, status: "ready",
  };
  await assertCrashAtomic({
    invoke: (storage) => new Paste(state(storage)).put({
      row, generation: "generation-1", fingerprint: "fingerprint-1",
    }),
  });
});

test("Paste append converges after every local commit crash", async () => {
  await assertCrashConverges({
    harness: artifactHarness,
    invoke: (h, run) => h.paste().append({ ...appendBody(`append-${run}`), uploadId: "up-2" }),
    async converged(h) {
      const versions = await versionsOf(h.paste());
      assert.deepStrictEqual(versions.map(({ ver, uploadId }) => ({ ver, uploadId })), [
        { ver: 1, uploadId: "" }, { ver: 2, uploadId: "up-2" },
      ]);
      assert.equal((await entryOf(h.identityStorage)).chargedSize, 6);
      assert.equal(await holdsPending(h.paste()), false);
      assert.equal(h.pasteStorage.alarm, null);
      assert.equal(await nextAppendGranted(h), true);
      assert.equal((await versionsOf(h.paste())).at(-1).ver, 3);
    },
  });
});

test("Paste serializes concurrent version appends", async () => {
  const h = artifactHarness({ serial: true });
  const paste = h.paste();
  const append = (opId) => paste.fetch(new Request("https://cell/paste/append", {
    method: "POST",
    body: JSON.stringify({
      opId, generation: "generation-1", userCap: 10,
      kind: "html", size: 2, now: 8,
    }),
  }));
  const results = await Promise.all([append("append-2"), append("append-3")]);
  assert.deepStrictEqual((await Promise.all(results.map(responseJSON))).map((result) => result.body.ver), [2, 3]);
  assert.deepStrictEqual((await versionsOf(paste)).map((version) => version.ver), [1, 2, 3]);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 6);
});

test("Paste history grows without any value outgrowing one version", async () => {
  const h = artifactHarness();
  const manifest = { "/index.html": { blob: "b".repeat(2000) } };
  h.pasteStorage.tooBig = (_key, value) => JSON.stringify(value).length > 3000;
  for (let ver = 2; ver <= 6; ver++) {
    const appended = await h.paste().append({ ...appendBody(`append-${ver}`), size: 1, manifest });
    assert.equal(appended.status, 200);
  }
  assert.equal(h.pasteStorage.data.has("versions"), false);
  assert.deepStrictEqual((await rowOf(h.paste())).manifest, manifest);
  const listed = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(listed.body.map((version) => version.ver), [6, 5, 4, 3, 2, 1]);
  assert.ok(listed.body.every((version) => !Object.hasOwn(version, "manifest")));
});

test("Paste versions list reads no manifest, legacy or split", async () => {
  const legacySeed = artifactPasteSeed();
  legacySeed.get("versions")[0].manifest = { "/": { blob: "v1" } };
  const legacy = await responseJSON(await artifactHarness({ pasteSeed: legacySeed }).paste().listVersions());
  assert.deepStrictEqual(legacy.body.map((version) => Object.hasOwn(version, "manifest")), [false]);

  const h = artifactHarness();
  const manifest = { "/index.html": { blob: "b" } };
  assert.equal((await h.paste().append({ ...appendBody(), manifest })).status, 200);
  const read = [];
  const get = h.pasteStorage.get.bind(h.pasteStorage);
  h.pasteStorage.get = (key) => {
    read.push(key);
    return get(key);
  };
  const listed = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(listed.body.map((version) => version.ver), [2, 1]);
  assert.ok(listed.body.every((version) => !Object.hasOwn(version, "manifest")));
  assert.deepStrictEqual(read.filter((key) => key.startsWith("manifest:")), []);
});

test("Paste splits a legacy single-value history on its first version write", async () => {
  const pasteSeed = artifactPasteSeed();
  const legacyManifest = { "/": { blob: "v1" } };
  pasteSeed.get("versions")[0].manifest = legacyManifest;
  pasteSeed.get("row").manifest = legacyManifest;
  const h = artifactHarness({ pasteSeed });
  assert.equal((await h.paste().append(appendBody())).status, 200);
  assert.equal(h.pasteStorage.data.has("versions"), false);
  assert.deepStrictEqual(h.pasteStorage.data.get("ver:1"), {
    ver: 1, kind: "html", size: 2, createdAt: 7, deleted: false,
  });
  assert.deepStrictEqual(h.pasteStorage.data.get("manifest:1"), legacyManifest);
  assert.equal(h.pasteStorage.data.has("manifest:2"), false);
  assert.equal(h.pasteStorage.data.get("row").manifest, null);
  assert.equal((await h.paste().pin({ opId: "pin-1", generation: "generation-1", ver: 1 })).status, 200);
  assert.deepStrictEqual(h.pasteStorage.data.get("row").manifest, legacyManifest);
});

test("Paste alarm publishes an append granted before the storage limit refused it", async () => {
  const h = artifactHarness();
  h.pasteStorage.tooBig = (key) => key === "versions";
  assert.equal((await h.paste().append(appendBody())).status, 200);

  const stuck = artifactHarness();
  stuck.pasteStorage.tooBig = (key) => key === "versions";
  stuck.transport.failAfter = 1;
  assert.equal((await stuck.paste().append(appendBody())).status, 502);
  assert.equal((await entryOf(stuck.identityStorage)).chargedSize, 6);
  await stuck.paste().alarm();
  assert.deepStrictEqual((await versionsOf(stuck.paste())).map((version) => version.ver), [1, 2]);
  assert.equal(await holdsPending(stuck.paste()), false);
  assert.equal(stuck.pasteStorage.alarm, null);
  assert.equal((await entryOf(stuck.identityStorage)).chargedSize, 6);
});

test("Paste append refused by storage settles its grant and answers 413 for good", async () => {
  const h = artifactHarness();
  h.pasteStorage.tooBig = (key) => key.startsWith("manifest:");
  const body = { ...appendBody("append-huge"), manifest: { "/": { blob: "huge" } } };
  const refused = await responseJSON(await h.paste().append(body));
  assert.deepStrictEqual(refused, { status: 413, body: { error: "version-too-large" } });
  assert.deepStrictEqual((await versionsOf(h.paste())).map((version) => version.ver), [1]);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 2);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);

  h.pasteStorage.tooBig = null;
  assert.deepStrictEqual(await responseJSON(await h.paste().append(body)), refused);
  assert.equal((await h.paste().append(appendBody("append-2"))).status, 200);
});

test("Paste alarm retries back off to a ceiling and never throw", async (t) => {
  const realNow = Date.now;
  const now = 1_000_000;
  Date.now = () => now;
  t.after(() => { Date.now = realNow; });
  const h = artifactHarness();
  h.transport.failBefore = Infinity;
  assert.equal((await h.paste().append(appendBody())).status, 502);
  const delays = [h.pasteStorage.alarm - now];
  for (let fire = 0; fire < 10; fire++) {
    await h.paste().alarm();
    delays.push(h.pasteStorage.alarm - now);
  }
  assert.deepStrictEqual(delays, [
    1000, 2000, 4000, 8000, 16000, 32000, 64000, 128000, 256000, 300000, 300000,
  ]);

  const get = h.pasteStorage.get;
  h.pasteStorage.get = async () => { throw new Error("storage unavailable"); };
  await h.paste().alarm();
  h.pasteStorage.get = get;
  assert.equal(h.pasteStorage.alarm - now, 300000);
  assert.equal(await holdsPending(h.paste()), true);

  h.pasteStorage.alarm = now;
  h.pasteStorage.transaction = async () => { throw new Error("handler reset"); };
  await assert.rejects(h.paste().alarm());
  assert.equal(h.pasteStorage.alarm - now, 300000, "a dying handler left the alarm due at once");
});

test("Paste append refuses a version too large to persist before any charge", async () => {
  const h = artifactHarness();
  h.pasteStorage.tooBig = (_key, value) => JSON.stringify(value).length > 1000;
  const refused = await responseJSON(await h.paste().append({
    ...appendBody("append-huge"), manifest: { "/": { blob: "b".repeat(2000) } },
  }));
  assert.deepStrictEqual(refused, { status: 413, body: { error: "version-too-large" } });
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 2);
  assert.deepStrictEqual((await versionsOf(h.paste())).map((version) => version.ver), [1]);
});

test("Paste delete converges after every local commit crash", async () => {
  await assertCrashConverges({
    harness: deletableArtifactHarness,
    invoke: (h, run) => h.paste().deleteVersion({
      opId: `delete-${run}`, generation: "generation-1", ver: 1,
    }),
    async converged(h) {
      assert.equal((await versionsOf(h.paste()))[0].deleted, true);
      assert.equal((await rowOf(h.paste())).size, 4);
      const entry = await entryOf(h.identityStorage);
      assert.equal(entry.chargedSize, 4);
      assert.equal(entry.servedSize, 4);
      assert.equal(await holdsPending(h.paste()), false);
      assert.equal(h.pasteStorage.alarm, null);
    },
  });
});

function artifactPasteSeed({ generation = "generation-1", charge = 2 } = {}) {
  return new Map([
    ["row", {
      slug: "slugone1", identity: "owner", kind: "html", size: 2,
      createdAt: 7, updatedAt: 7, status: "ready", pinnedVersion: 0,
      generation, accountingVersion: 0,
    }],
    ["versions", [{
      ver: 1, kind: "html", size: charge,
      createdAt: 7, deleted: false, manifest: null,
    }]],
    ["maxVer", 1],
  ]);
}

function artifactIdentitySeed({ generation = "generation-1", charge = 2, extra = 0 } = {}) {
  const entries = {
    slugone1: {
      generation, size: charge, chargedSize: charge, servedSize: 2,
      accountingVersion: 0, status: "ready", at: 7, updatedAt: 7,
      latestVersion: 1, kind: "html", name: "",
    },
  };
  if (extra > 0) {
    entries.other = { generation: "other-generation", size: extra, chargedSize: extra };
  }
  return new Map([
    ["entries", entries],
    [`artifact-account:slugone1:${generation}`, { version: 0, allocated: charge, target: charge }],
  ]);
}

function artifactHarness(options = {}) {
  const pasteStorage = new FakeStorage(options.pasteSeed ?? artifactPasteSeed(options));
  const identityStorage = new FakeStorage(options.identitySeed ?? artifactIdentitySeed(options));
  const identity = new Identity(state(identityStorage));
  const transport = { failBefore: 0, failAfter: 0 };
  const endpoint = {
    async fetch(request) {
      if (transport.failBefore-- > 0) {
        throw new Error("identity unavailable");
      }
      const response = await identity.fetch(request);
      if (transport.failAfter-- > 0) {
        throw new Error("identity response lost");
      }
      return response;
    },
  };
  const env = {
    IDENTITY: {
      idFromName(name) { return name; },
      get() { return endpoint; },
    },
    PASTES: {
      idFromName(name) {
        assert.equal(name, "slugone1");
        return fakeCellID(PASTE_CELL_ID);
      },
    },
  };
  const pasteState = options.serial ? serialState(pasteStorage) : state(pasteStorage);
  return {
    pasteStorage,
    identityStorage,
    transport,
    paste: () => new Paste(pasteState, env),
  };
}

// Two versions with the second served, so version 1 is deletable. `pinned`
// pins the row to version 2 instead of serving it as the latest.
function deletableArtifactHarness({ pinned = false } = {}) {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("versions").push({
    ver: 2, kind: "markdown", size: 4,
    createdAt: 8, deleted: false, manifest: null,
  });
  pasteSeed.set("maxVer", 2);
  const row = pasteSeed.get("row");
  row.kind = "markdown";
  row.size = 4;
  const identitySeed = artifactIdentitySeed({ charge: 6 });
  const entry = identitySeed.get("entries").slugone1;
  entry.servedSize = 4;
  if (pinned) {
    row.pinnedVersion = 2;
  } else {
    entry.latestVersion = 2;
  }
  return artifactHarness({ pasteSeed, identitySeed });
}

function appendBody(opId = "append-1", userCap = 10) {
  return {
    opId, generation: "generation-1", userCap, kind: "markdown",
    size: 4, now: 8,
  };
}

test("Paste append recovers a response-lost reservation without duplicating the version", async () => {
  const h = artifactHarness();
  h.transport.failAfter = 1;

  assert.equal((await h.paste().append(appendBody())).status, 502);
  assert.equal((await versionsOf(h.paste())).length, 1);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal((await versionsOf(h.paste())).length, 2);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);

  const replay = await responseJSON(await h.paste().append(appendBody()));
  assert.deepStrictEqual(replay, {
    status: 200,
    body: { appended: true, ver: 2, wasPinned: false, totalSize: 6 },
  });
  assert.equal((await versionsOf(h.paste())).length, 2);
  assert.equal(await nextAppendGranted(h), true);
});

test("Paste projection reloads state after recovering prior accounting", async () => {
  const h = artifactHarness();
  h.transport.failAfter = 1;
  assert.equal((await h.paste().append(appendBody())).status, 502);

  const renamed = await responseJSON(await h.paste().rename({
    name: "after recovery", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(renamed, { status: 200, body: { changed: true } });
  const row = await rowOf(h.paste());
  assert.equal(row.kind, "markdown");
  assert.equal(row.size, 4);
  assert.equal(row.name, "after recovery");
  const entry = await entryOf(h.identityStorage);
  assert.equal(entry.name, "after recovery");
  assert.equal(entry.servedSize, 4);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
  assert.equal(await nextAppendGranted(h), true);
});

test("Paste append keeps a quota refusal permanent across later capacity", async () => {
  const h = artifactHarness({ extra: 6 });
  const refused = await responseJSON(await h.paste().append(appendBody("append-refused")));
  assert.deepStrictEqual(refused, { status: 507, body: { error: "over-quota" } });
  assert.equal((await versionsOf(h.paste())).length, 1);

  assert.equal((await new Identity(state(h.identityStorage)).release({
    slug: "other", generation: "other-generation",
  })).status, 204);

  assert.deepStrictEqual(await responseJSON(await h.paste().append(appendBody("append-refused"))), refused);
  assert.equal((await versionsOf(h.paste())).length, 1);
  assert.equal((await h.paste().append(appendBody("append-2"))).status, 200);
  assert.equal((await versionsOf(h.paste())).length, 2);
});

test("Paste projection remains available after a quota refusal", async () => {
  const h = artifactHarness({ extra: 6 });
  assert.equal((await h.paste().append(appendBody("append-refused"))).status, 507);

  const renamed = await responseJSON(await h.paste().rename({
    name: "still mutable", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(renamed, { status: 200, body: { changed: true } });
  assert.equal((await entryOf(h.identityStorage)).name, "still mutable");
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste delete commits before an unavailable allocation release", async () => {
  const h = deletableArtifactHarness();
  h.transport.failBefore = 1;

  const deleted = await responseJSON(await h.paste().deleteVersion({
    opId: "delete-1", generation: "generation-1", ver: 1,
  }));
  assert.deepStrictEqual(deleted, {
    status: 200, body: { deleted: true, totalSize: 4, upload: "", prefixes: [], keys: [], freed: 2 },
  });
  assert.equal((await versionsOf(h.paste()))[0].deleted, true);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 4);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste delete refuses the version served at commit time", async () => {
  const h = deletableArtifactHarness({ pinned: true });

  const deleted = await responseJSON(await h.paste().deleteVersion({
    opId: "delete-served", generation: "generation-1", ver: 2,
  }));
  assert.deepStrictEqual(deleted, {
    status: 409, body: { error: "version-served" },
  });
  assert.equal((await versionsOf(h.paste()))[1].deleted, false);
  assert.equal((await rowOf(h.paste())).pinnedVersion, 2);
  assert.equal(await holdsPending(h.paste()), false);
});

test("Paste pin fences unchanged charge and projects the served version", async () => {
  const h = deletableArtifactHarness();

  assert.deepStrictEqual(await responseJSON(await h.paste().pin({
    opId: "pin-1", generation: "generation-1", ver: 1,
  })), {
    status: 200, body: { pinned: true, ver: 1 },
  });
  assert.equal((await rowOf(h.paste())).size, 2);
  const entry = await entryOf(h.identityStorage);
  assert.equal(entry.chargedSize, 6);
  assert.equal(entry.servedSize, 2);
  assert.equal(entry.pinnedVersion, 1);
  assert.equal(await nextAppendGranted(h), true);
});

test("Paste removal fences release before allowing a new incarnation", async () => {
  const h = artifactHarness();
  h.transport.failBefore = 1;

  const removed = await responseJSON(await h.paste().remove({
    opId: "remove-1", generation: "generation-1", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true, uploads: [] } });
  assert.equal(await rowOf(h.paste()), undefined);
  assert.equal(await putIncarnation(h), 409);
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 2);

  await h.paste().alarm();
  assert.equal(await entryOf(h.identityStorage), undefined);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(await putIncarnation(h), 204);

  assert.deepStrictEqual(await responseJSON(await h.paste().remove({
    opId: "remove-1", generation: "generation-1", identity: "owner", createdAt: 7,
  })), removed);
  assert.equal((await rowOf(h.paste())).generation, "generation-2");
});

test("Paste receipts are scoped to one artifact generation", async () => {
  const h = artifactHarness();
  assert.equal((await h.paste().append(appendBody("shared-op"))).status, 200);
  assert.equal((await h.paste().remove({
    opId: "remove-generation-1", generation: "generation-1",
    identity: "owner", createdAt: 7,
  })).status, 200);

  const identity = new Identity(state(h.identityStorage));
  assert.equal((await identity.reserve({
    slug: "slugone1", generation: "generation-2", size: 1,
    userCap: 10, status: "ready", now: 9,
    intent: {
      id: "create:slugone1:generation-2", kind: "create_paste", subject: "slugone1",
      fingerprint: "fingerprint-2",
    },
  })).status, 200);
  assert.equal((await h.paste().put({
    generation: "generation-2",
    fingerprint: "fingerprint-2",
    row: {
      slug: "slugone1", identity: "owner", kind: "html",
      size: 1, status: "ready", createdAt: 9, updatedAt: 9,
    },
  })).status, 204);

  const appended = await responseJSON(await h.paste().append({
    ...appendBody("shared-op"), generation: "generation-2", size: 1,
  }));
  assert.deepStrictEqual(appended, {
    status: 200,
    body: { appended: true, ver: 2, wasPinned: false, totalSize: 2 },
  });
  assert.equal((await versionsOf(h.paste())).length, 2);
});

test("Paste rejects an old incarnation without mutating the replacement allocation", async () => {
  const identitySeed = artifactIdentitySeed({ generation: "generation-2" });
  const h = artifactHarness({ identitySeed });

  assert.equal((await h.paste().append(appendBody("stale-generation"))).status, 409);
  assert.equal((await versionsOf(h.paste())).length, 1);
  const entry = await entryOf(h.identityStorage);
  assert.equal(entry.generation, "generation-2");
  assert.equal(entry.chargedSize, 2);
});

test("Paste row without a generation answers any mutation with generation-mismatch", async () => {
  const pasteSeed = artifactPasteSeed();
  delete pasteSeed.get("row").generation;
  const h = artifactHarness({ pasteSeed });
  for (const generation of ["", "generation-1"]) {
    const refused = await responseJSON(await h.paste().append({ ...appendBody(`op-${generation}`), generation }));
    assert.deepStrictEqual(refused, { status: 409, body: { error: "generation-mismatch" } });
  }
  assert.equal((await versionsOf(h.paste())).length, 1);
  assert.equal((await rowOf(h.paste())).generation, undefined);
});

test("Paste empty generation is a generation-mismatch", async () => {
  const h = artifactHarness();
  const refused = await responseJSON(await h.paste().append({
    ...appendBody("empty-generation"), generation: "",
  }));
  assert.equal(refused.status, 409);
  assert.equal(refused.body.error, "generation-mismatch");
  assert.equal((await versionsOf(h.paste())).length, 1);
});

test("Paste append answers an accounting conflict with 409 whatever Identity answered", async () => {
  const h = artifactHarness({ identitySeed: new Map() });
  const conflict = await responseJSON(await h.paste().append({ ...appendBody(), uploadId: "up-2" }));
  assert.deepStrictEqual(conflict, { status: 409, body: { error: "artifact-accounting-conflict" } });
  assert.equal((await versionsOf(h.paste())).length, 1);
  assert.equal(await holdsPending(h.paste()), true);
});

test("Paste refuses a mutation behind another operation's pending work with 423 and persists nothing", async () => {
  const h = artifactHarness();
  h.transport.failBefore = 100;
  assert.equal((await h.paste().append({ ...appendBody("append-stuck"), uploadId: "up-2" })).status, 502);

  const before = clone(h.pasteStorage.data);
  const commits = h.pasteStorage.commits;
  const busy = { status: 423, body: { error: "artifact-operation-pending" } };
  for (const [name, run] of [
    ["append", () => h.paste().append({ ...appendBody("append-other"), uploadId: "up-3" })],
    ["delete version", () => h.paste().deleteVersion({ opId: "delete-other", generation: "generation-1", ver: 1 })],
    ["pin", () => h.paste().pin({ opId: "pin-other", generation: "generation-1", ver: 1 })],
    ["remove", () => h.paste().remove({
      opId: "remove-other", generation: "generation-1", identity: "owner", createdAt: 7,
    })],
    ["fail", () => h.paste().fail({ opId: "fail-other", generation: "generation-1" })],
    ["other generation", () => h.paste().append({
      ...appendBody("append-stuck"), generation: "generation-2", uploadId: "up-2",
    })],
  ]) {
    assert.deepStrictEqual(await responseJSON(await run()), busy, name);
  }
  assert.deepStrictEqual(h.pasteStorage.data, before);
  assert.equal(h.pasteStorage.commits, commits);

  const own = await responseJSON(await h.paste().append({ ...appendBody("append-stuck"), uploadId: "up-2" }));
  assert.deepStrictEqual(own, { status: 502, body: { error: "artifact-operation-pending" } });
});

test("Paste rename persists a guarded projection through response loss", async () => {
  const h = artifactHarness();
  h.transport.failAfter = 1;

  const renamed = await responseJSON(await h.paste().rename({
    name: "new name", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(renamed, { status: 200, body: { changed: true } });
  assert.equal((await rowOf(h.paste())).name, "new name");
  assert.equal((await entryOf(h.identityStorage)).name, "new name");
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste failure converges through target-zero accounting", async () => {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("row").status = "pending";
  const identitySeed = artifactIdentitySeed();
  identitySeed.get("entries").slugone1.status = "pending";
  const h = artifactHarness({ pasteSeed, identitySeed });
  h.transport.failBefore = 1;
  const body = {
    status: "failed", opId: "fail-generation-1", generation: "generation-1",
  };

  assert.deepStrictEqual(await responseJSON(await h.paste().setStatus(body)), {
    status: 200, body: { changed: true, status: "failed" },
  });
  assert.equal((await rowOf(h.paste())).status, "failed");
  assert.equal((await entryOf(h.identityStorage)).chargedSize, 2);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(await entryOf(h.identityStorage), undefined);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
  assert.deepStrictEqual(await responseJSON(await h.paste().setStatus(body)), {
    status: 200, body: { changed: true, status: "failed" },
  });
});

test("Paste removal clears a failed incarnation whose allocation is already absent", async () => {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("row").status = "pending";
  const identitySeed = artifactIdentitySeed();
  identitySeed.get("entries").slugone1.status = "pending";
  const h = artifactHarness({ pasteSeed, identitySeed });

  assert.equal((await h.paste().setStatus({
    status: "failed", opId: "fail-before-remove", generation: "generation-1",
  })).status, 200);
  assert.equal(await entryOf(h.identityStorage), undefined);

  const removed = await responseJSON(await h.paste().remove({
    opId: "remove-failed", generation: "generation-1",
    identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true, uploads: [] } });
  assert.equal(await rowOf(h.paste()), undefined);
  assert.equal(await holdsPending(h.paste()), false);
  assert.equal(h.pasteStorage.alarm, null);
  assert.equal(await putIncarnation(h), 204);
});

test("Paste failure converges after every local commit crash", async () => {
  await assertCrashConverges({
    harness() {
      const pasteSeed = artifactPasteSeed();
      pasteSeed.get("row").status = "pending";
      const identitySeed = artifactIdentitySeed();
      identitySeed.get("entries").slugone1.status = "pending";
      return artifactHarness({ pasteSeed, identitySeed });
    },
    invoke: (h, run) => h.paste().setStatus({
      status: "failed", generation: "generation-1", opId: `fail-${run}`,
    }),
    async converged(h) {
      assert.equal((await rowOf(h.paste())).status, "failed");
      assert.equal(await entryOf(h.identityStorage), undefined);
      assert.equal(await holdsPending(h.paste()), false);
      assert.equal(h.pasteStorage.alarm, null);
    },
  });
});

test("Identity projection cannot mutate a replacement generation", async () => {
  const storage = new FakeStorage(artifactIdentitySeed({ generation: "generation-2" }));
  const before = clone(storage.data);
  const response = await new Identity(state(storage)).artifactProject({
    slug: "slugone1",
    generation: "generation-1",
    version: 0,
    servedSize: 9,
  });
  assert.equal(response.status, 409);
  assert.deepStrictEqual(storage.data, before);
});

// Version 1 tombstoned, version 2 from before upload ids with a sha-only
// manifest, version 3 newest. `pinned` serves version 2 instead.
function uploadedArtifactHarness({ pinned = false } = {}) {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.delete("versions");
  const v2Manifest = { "/": { sha: "legacy-v2" } };
  const v3Manifest = { "/": { key: "uploads/up-3/0" } };
  pasteSeed.set("ver:1", {
    ver: 1, kind: "html", size: 2, createdAt: 7, deleted: true, uploadId: "up-1",
  });
  pasteSeed.set("ver:2", { ver: 2, kind: "html", size: 2, createdAt: 8, deleted: false });
  pasteSeed.set("manifest:2", v2Manifest);
  pasteSeed.set("ver:3", {
    ver: 3, kind: "markdown", size: 4, createdAt: 9, deleted: false, uploadId: "up-3",
  });
  pasteSeed.set("manifest:3", v3Manifest);
  pasteSeed.set("maxVer", 3);
  Object.assign(pasteSeed.get("row"), pinned
    ? { size: 2, manifest: v2Manifest, pinnedVersion: 2 }
    : { kind: "markdown", size: 4, manifest: v3Manifest });
  const identitySeed = artifactIdentitySeed({ charge: 6 });
  Object.assign(identitySeed.get("entries").slugone1, { servedSize: pinned ? 2 : 4, latestVersion: 3 });
  return artifactHarness({ pasteSeed, identitySeed });
}

async function appendUploads(h, vers) {
  for (const ver of vers) {
    const appended = await h.paste().append({
      ...appendBody(`append-${ver}`), size: 1, uploadId: `up-${ver}`,
    });
    assert.equal(appended.status, 200);
  }
}

test("Paste.put records the create's upload id on version 1", async () => {
  const row = {
    slug: "newslug2", identity: "owner", kind: "html", size: 3,
    createdAt: 7, updatedAt: 7, status: "ready", uploadId: "up-1",
  };
  const create = (storage, sent) => new Paste(state(storage)).put({
    row: sent, generation: "generation-1", fingerprint: "fingerprint-1",
  });

  const storage = new FakeStorage();
  assert.equal((await create(storage, row)).status, 204);
  const paste = new Paste(state(storage));
  assert.deepStrictEqual((await versionsOf(paste)).map((version) => version.uploadId), ["up-1"]);
  assert.equal((await rowOf(paste)).uploadId, "up-1");

  const older = new FakeStorage();
  const { uploadId: _uploadId, ...oldRow } = row;
  assert.equal((await create(older, oldRow)).status, 204);
  assert.deepStrictEqual((await versionsOf(new Paste(state(older)))).map((version) => version.uploadId), [""]);

  const malformed = new FakeStorage();
  assert.equal((await create(malformed, { ...row, uploadId: 7 })).status, 400);
  assert.equal(malformed.commits, 0);
});

test("Paste append records its upload id and versions list every version's id", async () => {
  const h = artifactHarness();
  const legacy = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(legacy.body.map((version) => version.uploadId), [""]);

  assert.equal((await h.paste().append({ ...appendBody("append-bad"), uploadId: 7 })).status, 400);
  assert.equal(await holdsPending(h.paste()), false);

  assert.equal((await h.paste().append({ ...appendBody(), uploadId: "up-2" })).status, 200);
  const listed = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(listed.body.map(({ ver, uploadId }) => ({ ver, uploadId })), [
    { ver: 2, uploadId: "up-2" },
    { ver: 1, uploadId: "" },
  ]);
});

test("Paste head carries the served version's upload id through append, pin, and unpin", async () => {
  const h = artifactHarness();
  const head = async () => (await rowOf(h.paste())).uploadId;
  assert.equal((await h.paste().append({ ...appendBody("append-2"), uploadId: "up-2" })).status, 200);
  assert.equal(await head(), "up-2");
  assert.equal((await h.paste().append({ ...appendBody("append-3"), uploadId: "up-3" })).status, 200);
  assert.equal(await head(), "up-3");

  const pin = (opId, ver) => h.paste().pin({ opId, generation: "generation-1", ver });
  assert.equal((await pin("pin-2", 2)).status, 200);
  assert.equal(await head(), "up-2");
  assert.equal((await pin("unpin", 0)).status, 200);
  assert.equal(await head(), "up-3");
});

const hasContentSha = (value) => Object.hasOwn(value, "contentSha");

test("Paste and Identity accept a sent contentSha without storing or answering it", async () => {
  const created = createIntentHarness();
  const reserve = { ...createIntentBody(), contentSha: "abc" };
  assert.equal((await created.identity.reserve(reserve)).status, 200);
  assert.equal((await created.paste.put({
    generation: reserve.generation,
    fingerprint: reserve.intent.fingerprint,
    row: {
      slug: reserve.slug, identity: "owner", status: "pending", kind: "html",
      contentSha: "abc", size: reserve.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 204);
  assert.equal(hasContentSha(await entryOf(created.identityStorage, reserve.slug)), false);
  assert.equal(hasContentSha(await rowOf(created.paste)), false);
  assert.equal((await versionsOf(created.paste)).some(hasContentSha), false);

  const h = artifactHarness();
  assert.equal((await h.paste().append({ ...appendBody(), contentSha: "v2" })).status, 200);
  assert.equal(hasContentSha(await rowOf(h.paste())), false);
  const versions = await responseJSON(await h.paste().listVersions());
  assert.equal(versions.body.some(hasContentSha), false);

  const identity = new Identity(state(h.identityStorage));
  assert.equal((await identity.artifactProject({
    slug: "slugone1", generation: "generation-1", version: 1, servedSize: 4, contentSha: "v2",
  })).status, 200);
  const listed = await responseJSON(await identity.list());
  assert.equal(listed.body.some(hasContentSha), false);
});

test("Paste and Identity drop a stored contentSha when they rewrite the head or listing entry", async () => {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("row").contentSha = "v1";
  pasteSeed.get("versions")[0].contentSha = "v1";
  const identitySeed = artifactIdentitySeed();
  identitySeed.get("entries").slugone1.contentSha = "v1";
  const h = artifactHarness({ pasteSeed, identitySeed });

  assert.equal((await h.paste().append(appendBody())).status, 200);
  assert.equal(hasContentSha(await rowOf(h.paste())), false);
  assert.equal(hasContentSha(await entryOf(h.identityStorage)), false);
});

test("Paste removal names every version's upload and replays it after re-creation", async () => {
  const h = artifactHarness();
  await appendUploads(h, [2, 3]);
  assert.equal((await h.paste().deleteVersion({
    opId: "delete-2", generation: "generation-1", ver: 2,
  })).status, 200);
  const body = { opId: "remove-1", generation: "generation-1", identity: "owner", createdAt: 7 };

  const removed = await responseJSON(await h.paste().remove(body));
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true, uploads: ["up-2", "up-3"] } });

  assert.equal((await h.paste().put({
    generation: "generation-2",
    fingerprint: "fingerprint-2",
    row: {
      slug: "slugone1", identity: "other", kind: "html",
      size: 1, status: "ready", createdAt: 9, updatedAt: 9, uploadId: "new-1",
    },
  })).status, 204);
  assert.deepStrictEqual(await responseJSON(await h.paste().remove(body)), removed);
});

test("Paste removal reads a single-value history's upload ids", async () => {
  const h = deletableArtifactHarness();
  const legacy = h.pasteStorage.data.get("versions");
  legacy[1].uploadId = "up-2";
  h.pasteStorage.data.set("versions", legacy);
  assert.deepStrictEqual(await responseJSON(await h.paste().remove({
    opId: "remove-legacy", generation: "generation-1", identity: "owner", createdAt: 7,
  })), { status: 200, body: { removed: true, uploads: ["up-2"] } });
});

test("Paste removal keeps its named uploads after every local commit crash", async () => {
  await assertCrashConverges({
    harness: uploadedArtifactHarness,
    invoke: (h, run) => h.paste().remove({
      opId: `remove-${run}`, generation: "generation-1", identity: "owner", createdAt: 7,
    }),
    async converged(h, run) {
      assert.deepStrictEqual(await responseJSON(await h.paste().remove({
        opId: `remove-${run}`, generation: "generation-1", identity: "owner", createdAt: 7,
      })), { status: 200, body: { removed: true, uploads: ["up-1", "up-3"] } });
      assert.equal(await rowOf(h.paste()), undefined);
      assert.equal(await holdsPending(h.paste()), false);
      assert.equal(await entryOf(h.identityStorage), undefined);
      assert.equal(await putIncarnation(h), 204);
    },
  });
});

test("Paste version delete names the tombstoned upload, and a replay the same", async () => {
  const h = artifactHarness();
  await appendUploads(h, [2, 3]);
  const body = { opId: "delete-2", generation: "generation-1", ver: 2 };
  const expected = {
    status: 200,
    body: { deleted: true, totalSize: 3, upload: "up-2", prefixes: ["uploads/up-2/"], keys: [], freed: 1 },
  };

  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion(body)), expected);
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion(body)), expected);
  // A new operation on a tombstone names nothing: a later version may share
  // what the first delete kept.
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion({
    ...body, opId: "delete-2-again",
  })), { status: 200, body: { deleted: true, totalSize: 3, upload: "", prefixes: [], keys: [], freed: 0 } });
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion({
    opId: "delete-1", generation: "generation-1", ver: 1,
  })), { status: 200, body: { deleted: true, totalSize: 1, upload: "", prefixes: [], keys: [], freed: 2 } });
});

function budgetSeed(a = 4, b = 4) {
  return new Map([
    ["roomAllocated", a + b],
    ["roomAllocation:11111111-1111-4111-8111-111111111111", { version: 0, allocated: a, target: a }],
    ["roomAllocation:r2", { version: 0, allocated: b, target: b }],
  ]);
}

async function decide(paste, body) {
  const response = await paste.roomDecide({ room: "11111111-1111-4111-8111-111111111111", appCap: 10, ...body });
  return { status: response.status, body: await response.json() };
}

function roomHarness({ room = roomDoc(), pasteSeed = budgetSeed() } = {}) {
  const roomStorage = new FakeStorage(room ? new Map([["state", room]]) : new Map());
  const pasteStorage = new FakeStorage(pasteSeed);
  const paste = new Paste(state(pasteStorage));
  const transport = { failBefore: 0, failAfter: 0 };
  const endpoint = {
    async fetch(request) {
      if (transport.failBefore-- > 0) {
        throw new Error("coordinator unavailable");
      }
      const response = await paste.fetch(request);
      if (transport.failAfter-- > 0) {
        throw new Error("coordinator response lost");
      }
      return response;
    },
  };
  const env = {
    PASTES: {
      idFromName(name) { return name; },
      get() { return endpoint; },
    },
    ROOMS: {
      idFromName(name) {
        assert.equal(name, `appslug1|${ROOM_ID}`);
        return fakeCellID(ROOM_CELL_ID);
      },
    },
  };
  return {
    roomStorage,
    paste,
    transport,
    room: () => new Room(state(roomStorage), env),
  };
}

async function responseJSON(response) {
  return { status: response.status, body: await response.json() };
}

// Every room's allocated bytes, as the app's coordinator admits against them.
async function appRoomBytes(paste) {
  return (await responseJSON(await paste.roomPreflight({ appCap: 0 }))).body.total;
}

// What a reader of the room sees: its sequence and value bytes.
async function roomView(room) {
  const response = await room.fetch(new Request("https://cell/room/scan"));
  if (response.status === 404) {
    return undefined;
  }
  const { values, seq } = await response.json();
  const bytes = Object.values(values).reduce((n, value) => n + Buffer.from(value, "base64").length, 0);
  return { seq, bytes };
}

test("Subnet serializes concurrent admissions at the limit", async () => {
  const storage = new FakeStorage();
  const subnet = new Subnet(serialState(storage));
  const admit = (identity) => subnet.fetch(new Request("https://cell/subnet/admit", {
    method: "POST",
    body: JSON.stringify({ identity, now: 1, window: 1000, limit: 1 }),
  }));
  const results = await Promise.all([admit("one"), admit("two")]);
  assert.deepStrictEqual((await Promise.all(results.map(responseJSON)))
    .map((result) => result.body.admitted).sort(), [false, true]);
  const snapshot = await subnet.fetch(new Request("https://cell/subnet/snapshot?now=1&window=1000"));
  assert.equal((await snapshot.json()).freshCount, 1);
});

test("Paste room coordinator persists grants and exact replays", async () => {
  const storage = new FakeStorage(budgetSeed());
  const paste = new Paste(state(storage));

  assert.deepStrictEqual(await decide(paste, { version: 1, targetBytes: 6 }), {
    status: 200,
    body: { granted: true, version: 1, allocated: 6, total: 10 },
  });
  const complete = clone(storage.data);
  assert.deepStrictEqual(await decide(paste, { version: 1, targetBytes: 6, appCap: 1 }), {
    status: 200,
    body: { granted: true, version: 1, allocated: 6, total: 10 },
  });
  assert.deepStrictEqual(storage.data, complete);

  assert.equal((await decide(paste, { version: 1, targetBytes: 7 })).status, 409);
  assert.equal((await decide(paste, { version: 0, targetBytes: 4 })).status, 400);
  assert.deepStrictEqual(storage.data, complete);
});

test("Paste room coordinator fences refused, stale, and reordered calls", async () => {
  const storage = new FakeStorage(budgetSeed());
  const paste = new Paste(state(storage));

  let result = await decide(paste, { version: 1, targetBytes: 7 });
  assert.equal(result.status, 507);
  assert.deepStrictEqual(await decide(paste, { version: 1, targetBytes: 7 }), result);
  assert.equal(await appRoomBytes(paste), 8);

  const release = await paste.roomDecide({
    room: "r2", version: 1, targetBytes: 0, appCap: 10,
  });
  assert.equal(release.status, 200);
  assert.equal(await appRoomBytes(paste), 4);

  result = await decide(paste, { version: 1, targetBytes: 7 });
  assert.equal(result.status, 507);
  assert.equal(await appRoomBytes(paste), 4);

  result = await decide(paste, { version: 2, targetBytes: 6 });
  assert.equal(result.status, 200);
  assert.equal(await appRoomBytes(paste), 6);
  const complete = clone(storage.data);
  assert.equal((await decide(paste, { version: 1, targetBytes: 7 })).status, 409);
  assert.deepStrictEqual(storage.data, complete);

  const fresh = new Paste(state(new FakeStorage(budgetSeed())));
  assert.equal((await decide(fresh, { version: 2, targetBytes: 6 })).status, 409);
});

test("Paste room coordinator rejects malformed decisions without mutation", async () => {
  for (const body of [
    { version: -1, targetBytes: 1 },
    { version: 1.5, targetBytes: 1 },
    { version: 1, targetBytes: -1 },
    { version: 1, targetBytes: 1.5 },
    { version: 1, targetBytes: Number.NaN },
  ]) {
    const storage = new FakeStorage(budgetSeed());
    const before = clone(storage.data);
    assert.equal((await decide(new Paste(state(storage)), body)).status, 400);
    assert.deepStrictEqual(storage.data, before);
  }
});

test("Paste room coordinator commits record and aggregate atomically", async () => {
  await assertCrashAtomic({
    seed: budgetSeed(),
    invoke: (storage) => new Paste(state(storage)).roomDecide({
      room: "11111111-1111-4111-8111-111111111111", version: 1, targetBytes: 6, appCap: 10,
    }),
  });
});

test("Room growth reserves before commit", async () => {
  const h = roomHarness();
  const result = await responseJSON(await h.room().put(putBody("aaaaaa")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 6 } });
  assert.deepStrictEqual(await roomView(h.room()), { seq: 8, bytes: 6 });
  assert.equal(h.roomStorage.alarm, null);
  assert.equal(await appRoomBytes(h.paste), 10);
});

test("Room growth refusal advances only the budget version", async () => {
  const h = roomHarness();
  const result = await responseJSON(await h.room().put(putBody("aaaaaaa")));
  assert.equal(result.status, 507);
  assert.deepStrictEqual(await roomView(h.room()), { seq: 7, bytes: 4 });
  assert.equal(h.roomStorage.alarm, null);
  assert.equal(await appRoomBytes(h.paste), 8);

  assert.deepStrictEqual(await responseJSON(await h.room().put(putBody("aaaaa"))), {
    status: 200, body: { seq: 8, bytes: 5 },
  });
  assert.equal(await appRoomBytes(h.paste), 9);
});

test("Room alarm recovers a crash after growth preparation", async () => {
  const h = roomHarness();
  h.roomStorage.crashAfter = 1;
  await assert.rejects(() => h.room().put(putBody("aaaaaa")), SimulatedCrash);

  assert.deepStrictEqual(await roomView(h.room()), { seq: 7, bytes: 4 });
  assert.notEqual(h.roomStorage.alarm, null);
  assert.equal(await appRoomBytes(h.paste), 8);

  h.roomStorage.commits = 0;
  h.roomStorage.crashAfter = Infinity;
  await h.room().alarm();
  assert.deepStrictEqual(await roomView(h.room()), { seq: 8, bytes: 6 });
  assert.equal(await appRoomBytes(h.paste), 10);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room alarm replays a committed decision after response loss", async () => {
  const h = roomHarness();
  h.transport.failAfter = 1;
  const result = await responseJSON(await h.room().put(putBody("aaaaaa")));
  assert.equal(result.status, 502);
  assert.equal((await roomView(h.room())).bytes, 4);
  assert.equal(await appRoomBytes(h.paste), 10);
  assert.notEqual(h.roomStorage.alarm, null);

  await h.room().alarm();
  assert.deepStrictEqual(await roomView(h.room()), { seq: 8, bytes: 6 });
  assert.equal(await appRoomBytes(h.paste), 10);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room alarm retries a transient coordinator failure", async () => {
  const h = roomHarness();
  h.transport.failBefore = 1;
  const result = await responseJSON(await h.room().put(putBody("aaaaaa")));
  assert.equal(result.status, 502);
  assert.equal((await roomView(h.room())).bytes, 4);
  assert.notEqual(h.roomStorage.alarm, null);

  await h.room().alarm();
  assert.equal((await roomView(h.room())).bytes, 6);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room shrink commits before releasing sibling capacity", async () => {
  const h = roomHarness({ room: roomDoc({ bytes: 6 }), pasteSeed: budgetSeed(6, 4) });
  const result = await responseJSON(await h.room().put(putBody("aa")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 2 } });
  assert.equal((await roomView(h.room())).bytes, 2);
  assert.equal(await appRoomBytes(h.paste), 6);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room equal-size mutations bypass the coordinator", async () => {
  const h = roomHarness();
  h.transport.failBefore = Infinity;
  let result = await responseJSON(await h.room().put(putBody("bbbb")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 4 } });
  result = await responseJSON(await h.room().del({ key: "absent", now: 10 }));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 9, bytes: 4 } });
  assert.equal(h.roomStorage.alarm, null);
});

test("Room serializes concurrent equal-size mutations", async () => {
  const storage = new FakeStorage(new Map([["state", roomDoc()]]));
  const room = new Room(serialState(storage), roomHarness().room().env);
  const results = await Promise.all([
    room.put(putBody("bbbb")),
    room.put(putBody("cccc")),
  ]);
  assert.deepStrictEqual(await Promise.all(results.map(responseJSON)), [
    { status: 200, body: { seq: 8, bytes: 4 } },
    { status: 200, body: { seq: 9, bytes: 4 } },
  ]);
  assert.equal((await roomView(room)).seq, 9);
});

test("Room shrink succeeds after commit when allocation release is unavailable", async () => {
  const h = roomHarness({ room: roomDoc({ bytes: 6 }), pasteSeed: budgetSeed(6, 4) });
  h.transport.failBefore = 1;
  const result = await responseJSON(await h.room().put(putBody("aa")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 2 } });
  assert.equal((await roomView(h.room())).bytes, 2);
  assert.notEqual(h.roomStorage.alarm, null);
  assert.equal(await appRoomBytes(h.paste), 10);

  await h.room().alarm();
  assert.equal(await appRoomBytes(h.paste), 6);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room rejects client frames that impersonate durable control", async () => {
  const sent = [];
  const closed = [];
  const sender = { close: (...args) => closed.push(args) };
  const peer = { send: (message) => sent.push(message) };
  const room = new Room({
    storage: new FakeStorage(),
    getWebSockets: () => [sender, peer],
  }, {});

  await room.webSocketMessage(sender, JSON.stringify({ type: "put", seq: 99 }));
  assert.deepStrictEqual(sent, []);
  assert.deepStrictEqual(closed, [[1008, "reserved control frame"]]);

  await room.webSocketMessage(sender, JSON.stringify({ type: "presence", x: 1 }));
  assert.deepStrictEqual(sent, [JSON.stringify({ type: "presence", x: 1 })]);
});

test("Room creation does not persist when its ledger is unavailable", async () => {
  const roomStorage = new FakeStorage();
  const endpoint = {
    async fetch(request) {
      const op = new URL(request.url).pathname.split("/").pop();
      if (op === "roompreflight") {
        return Response.json({ total: 0 });
      }
      return Response.json({ error: "unavailable" }, { status: 503 });
    },
  };
  const room = new Room(state(roomStorage), {
    PASTES: {
      idFromName(name) { return name; },
      get() { return endpoint; },
    },
  });
  const response = await room.create({
    appSlug: "appslug1", id: "r3", createdAt: 1, updatedAt: 1,
    subnet: "192.0.2.0/24", at: 1, appCap: 10,
  });
  assert.equal(response.status, 502);
  assert.equal(await roomView(room), undefined);
});

test("Paste ops are reachable by id through the op query when the path names none", async () => {
  const h = artifactHarness();
  const call = async (path, init) => responseJSON(await h.paste().fetch(new Request(`https://cell${path}`, init)));

  const row = await call("/do/Paste:abc?op=get");
  assert.equal(row.status, 200);
  assert.equal(row.body.slug, "slugone1");
  const versions = await call("/do/Paste:abc?op=versions");
  assert.deepStrictEqual(versions.body.map((version) => version.ver), [1]);
  assert.equal((await h.paste().fetch(new Request("https://cell/do/Paste:abc?op=nope"))).status, 404);
  assert.equal((await h.paste().fetch(new Request("https://cell/do/Paste:abc"))).status, 404);

  const byPath = await call("/paste/get?op=versions");
  assert.equal(byPath.body.slug, "slugone1");
});

test("Room creation refuses a full app before persisting", async () => {
  const h = roomHarness({ room: null, pasteSeed: budgetSeed(6, 4) });
  const result = await responseJSON(await h.room().create({
    appSlug: "appslug1",
    id: "r3",
    createdAt: 1,
    updatedAt: 1,
    subnet: "192.0.2.0/24",
    at: 1,
    appCap: 10,
  }));
  assert.equal(result.status, 507);
  assert.equal(await roomView(h.room()), undefined);
  const counts = await h.paste.roomCounts(new URL("https://cell/paste/roomcounts?now=1&window=1000&subnet=192.0.2.0/24"));
  assert.deepStrictEqual(await counts.json(), { perSubnet: 0, perApp: 0 });
});

test("Paste append splits a long legacy history within the 128-entry write limit", async () => {
  const seed = artifactPasteSeed();
  seed.set("versions", Array.from({ length: 100 }, (_, i) => ({
    ver: i + 1, kind: "html", size: 1, createdAt: 7, deleted: false, manifest: { "/": { blob: `v${i + 1}` } },
  })));
  seed.set("maxVer", 100);
  const h = artifactHarness({ pasteSeed: seed });
  assert.equal((await h.paste().append({ ...appendBody("append-1", 1000), size: 1 })).status, 200);
  assert.equal(h.pasteStorage.data.get("versions"), undefined);
  assert.equal((await versionsOf(h.paste())).length, 101);
});

test("Paste put re-creates a slug whose old history passed the 128-key delete limit", async () => {
  const seed = new Map([["maxVer", 100]]);
  for (let ver = 1; ver <= 100; ver++) {
    seed.set(`ver:${ver}`, { ver, kind: "html", size: 1, createdAt: 7, deleted: false });
    seed.set(`manifest:${ver}`, { "/": { blob: `v${ver}` } });
  }
  const h = artifactHarness({ pasteSeed: seed });
  assert.equal((await h.paste().put({
    generation: "generation-2",
    fingerprint: "fingerprint-2",
    row: {
      slug: "slugone1", identity: "other", kind: "html",
      size: 1, status: "ready", createdAt: 9, updatedAt: 9,
    },
  })).status, 204);
  assert.deepStrictEqual((await versionsOf(h.paste())).map((v) => v.ver), [1]);
  assert.equal(h.pasteStorage.data.get("manifest:100"), undefined);
});

// A site manifest as Go encodes it: path -> entry with its object span.
function siteManifest(files) {
  const Files = {};
  for (const [path, [Key, CompressedSize, Offset, Length]] of Object.entries(files)) {
    Files[path] = { Key, CompressedSize, ...(Length ? { Offset, Length } : {}) };
  }
  return { Files };
}

async function appendSite(h, { opId, uploadId, base, files }) {
  const manifest = siteManifest(files);
  const size = Object.values(files).reduce((sum, [, n]) => sum + n, 0);
  return responseJSON(await h.paste().append({
    ...appendBody(opId, 1000), kind: "site", size, uploadId, manifest,
    ...(base !== undefined ? { base } : {}),
  }));
}

async function chargedSize(h) {
  return (await entryOf(h.identityStorage)).chargedSize;
}

// docs/SPEC.md "Redeploy reuse": an append against the head shares its
// standalone objects and charges only what it adds; a version delete frees
// exactly what neither live neighbour holds, whole prefixes where it can.
test("Paste redeploy reuse shares standalone objects and frees from neighbours", async () => {
  const h = artifactHarness();
  const audio = ["uploads/u2/0", 100];
  const v2 = await appendSite(h, {
    opId: "a2", uploadId: "u2",
    files: { "a.m4a": audio, "x.js": ["uploads/u2/1", 10, 0, 10], "y.js": ["uploads/u2/1", 5, 10, 5] },
  });
  assert.equal(v2.body.totalSize, 2 + 115);

  const packed = { "x.js": ["uploads/u3/0", 10, 0, 10], "y.js": ["uploads/u3/0", 5, 10, 5] };
  assert.deepStrictEqual(await appendSite(h, { opId: "stale", uploadId: "u3", base: 1, files: { "a.m4a": audio, ...packed } }),
    { status: 409, body: { error: "base-moved" } });
  assert.deepStrictEqual(await appendSite(h, {
    opId: "packshare", uploadId: "u3", base: 2, files: { "a.m4a": audio, "x.js": ["uploads/u2/1", 10, 0, 10] },
  }), { status: 409, body: { error: "bad-reuse" } });

  const v3 = await appendSite(h, { opId: "a3", uploadId: "u3", base: 2, files: { "a.m4a": audio, ...packed } });
  assert.equal(v3.body.totalSize, 2 + 115 + 15);
  assert.equal(await chargedSize(h), 132);

  const del2 = await responseJSON(await h.paste().deleteVersion({ opId: "d2", generation: "generation-1", ver: 2 }));
  assert.deepStrictEqual(del2.body, {
    deleted: true, totalSize: 117, upload: "", prefixes: [], keys: ["uploads/u2/1"], freed: 15,
  });
  assert.equal((await versionsOf(h.paste())).find((version) => version.ver === 3).charge, 115);

  await appendSite(h, { opId: "a4", uploadId: "u4", base: 3, files: { "a.m4a": audio, "x.js": ["uploads/u4/0", 10, 0, 10] } });
  const del3 = await responseJSON(await h.paste().deleteVersion({ opId: "d3", generation: "generation-1", ver: 3 }));
  assert.deepStrictEqual(del3.body, {
    deleted: true, totalSize: 112, upload: "u3", prefixes: ["uploads/u3/"], keys: [], freed: 15,
  });

  assert.equal((await h.paste().pin({ opId: "p1", generation: "generation-1", ver: 1 })).status, 200);
  const del4 = await responseJSON(await h.paste().deleteVersion({ opId: "d4", generation: "generation-1", ver: 4 }));
  assert.deepStrictEqual(del4.body, {
    deleted: true, totalSize: 2, upload: "u4", prefixes: ["uploads/u2/", "uploads/u4/"], keys: [], freed: 110,
  });
  assert.equal(await chargedSize(h), 2);

  // A repeat under a new operation names nothing: those bytes are already
  // accounted for.
  const again = await responseJSON(await h.paste().deleteVersion({ opId: "d4-again", generation: "generation-1", ver: 4 }));
  assert.deepStrictEqual(again.body, { deleted: true, totalSize: 2, upload: "", prefixes: [], keys: [], freed: 0 });
});

test("Paste head answers the latest live version and its manifest", async () => {
  const h = artifactHarness();
  await appendSite(h, { opId: "a2", uploadId: "u2", files: { "a.m4a": ["uploads/u2/0", 100] } });
  const head = await responseJSON(await h.paste().head());
  assert.equal(head.body.ver, 2);
  assert.deepStrictEqual(head.body.manifest, siteManifest({ "a.m4a": ["uploads/u2/0", 100] }));
});
