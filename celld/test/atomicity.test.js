import assert from "node:assert/strict";
import test from "node:test";

import worker, { Identity, Paste, Room, Subnet } from "../src/index.js";
import {
  FakeStorage, PASTE_CELL_ID, ROOM_CELL_ID, ROOM_ID, SimulatedCrash,
  clone, fakeCellID, putBody, roomDoc, state, storedVersions,
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
// recovered through the pending alarm, and handed to `converged`. Counting from
// the reference run keeps a newly added commit inside the sweep.
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
    if (h.pasteStorage.data.get("artifactPending")) {
      await h.paste().alarm();
    }
    await converged(h);
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
    assert.equal(storage.data.get("entries"), undefined, name);
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
  assert.equal(storage.data.get("entries").slugone1.status, "ready");
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
  const entries = storage.data.get("entries");
  assert.equal(Object.keys(entries).length, 1);
  assert.equal(Object.values(entries)[0].size, 6);
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
  assert.equal(storage.data.get("entries").slugone1.generation, "generation-a");

  assert.equal((await identity.confirm({
    slug: "slugone1", generation: "generation-b", status: "ready",
  })).status, 409);
  assert.equal((await identity.release({
    slug: "slugone1", generation: "generation-b",
  })).status, 409);
  assert.equal(storage.data.get("entries").slugone1.generation, "generation-a");
  assert.equal(storage.data.get("entries").slugone1.status, "pending");
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

// Backdates a create intent past the recovery grace so alarm() acts on it.
function ageIntent(h, id) {
  const key = `intent:${id}`;
  h.identityStorage.data.set(key, { ...h.identityStorage.data.get(key), reservedAt: 0 });
}

test("Identity create alarm confirms a matching paste generation", async () => {
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

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2.status, "pending");
  assert.equal(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.alarm, null);
});

test("Identity create alarm releases a definitely absent paste", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2, undefined);
  assert.equal(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get(`artifact-account:${body.slug}:${body.generation}`).allocated, 0);
  assert.equal(h.identityStorage.alarm, null);
});

test("Identity create alarm retains intent across a transient paste failure", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);
  h.transport.failGet = 1;

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.notEqual(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get("entries").newslug2.generation, body.generation);
  assert.notEqual(h.identityStorage.alarm, null);
});

test("Identity create alarm fences a late put before releasing quota", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2, undefined);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: body.intent.fingerprint,
    row: {
      slug: body.slug, identity: "owner", status: "pending", kind: "html",
      size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 409);
  assert.equal(h.pasteStorage.data.get("row"), undefined);
});

test("Identity create alarm confirms the Paste row status monotonically", async () => {
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
  const entries = h.identityStorage.data.get("entries");
  entries.newslug2.status = "ready";
  h.identityStorage.data.set("entries", entries);

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2.status, "ready");
  assert.equal(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
});

test("Identity create alarm retains a conflicting same-generation row", async () => {
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

  ageIntent(h, body.intent.id);
  await h.identity.alarm();
  assert.notEqual(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get("entries").newslug2.status, "pending");
  assert.notEqual(h.identityStorage.alarm, null);
});

test("Identity create alarm leaves an in-flight create alone until the grace elapses", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  const before = Date.now();
  assert.equal((await h.identity.reserve(body)).status, 200);
  assert.ok(h.identityStorage.alarm >= before + 30_000);

  await h.identity.alarm();
  assert.notEqual(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get("entries").newslug2.generation, body.generation);
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
  assert.equal(storage.data.get("row").size, 3);
  assert.equal(storedVersions(storage).length, 1);
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
    converged(h) {
      assert.equal(storedVersions(h.pasteStorage).length, 2);
      assert.equal(storedVersions(h.pasteStorage)[1].uploadId, "up-2");
      assert.equal(h.pasteStorage.data.get("maxVer"), 2);
      assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);
      assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
      assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
      assert.equal(h.pasteStorage.alarm, null);
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
  assert.equal(h.pasteStorage.data.get("maxVer"), 3);
  assert.deepStrictEqual(storedVersions(h.pasteStorage).map((version) => version.ver), [1, 2, 3]);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
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
  assert.deepStrictEqual(storedVersions(h.pasteStorage).map((version) => version.ver), [1, 2, 3, 4, 5, 6]);
  assert.deepStrictEqual(h.pasteStorage.data.get("row").manifest, manifest);
  const listed = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(listed.body.map((version) => version.ver), [6, 5, 4, 3, 2, 1]);
  assert.deepStrictEqual(listed.body[0].manifest, manifest);
  assert.equal(listed.body.at(-1).manifest, null);
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

  const stuck = artifactHarness({
    identitySeed: h.identityStorage.data,
    pasteSeed: new Map([...artifactPasteSeed(), ["artifactPending", {
      kind: "append", stage: "decide", opId: "append-1", slug: "slugone1", owner: "owner",
      generation: "generation-1", version: 1, target: 6, userCap: 10, latestVersion: 2,
      wasPinned: false,
      mutation: { ver: 2, kind: "markdown", size: 4, createdAt: 8, deleted: false, manifest: null },
    }]]),
  });
  stuck.pasteStorage.tooBig = (key) => key === "versions";
  await stuck.paste().alarm();
  assert.deepStrictEqual(storedVersions(stuck.pasteStorage).map((version) => version.ver), [1, 2]);
  assert.equal(stuck.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(stuck.pasteStorage.alarm, null);
  assert.equal(stuck.identityStorage.data.get("entries").slugone1.chargedSize, 6);
});

test("Paste append refused by storage settles its grant and answers 413 for good", async () => {
  const h = artifactHarness();
  h.pasteStorage.tooBig = (key) => key.startsWith("manifest:");
  const body = { ...appendBody("append-huge"), manifest: { "/": { blob: "huge" } } };
  const refused = await responseJSON(await h.paste().append(body));
  assert.deepStrictEqual(refused, { status: 413, body: { error: "version-too-large" } });
  assert.deepStrictEqual(storedVersions(h.pasteStorage).map((version) => version.ver), [1]);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 2);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 2);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  assert.notEqual(h.pasteStorage.data.get("artifactPending"), undefined);

  h.pasteStorage.alarm = now;
  h.pasteStorage.transaction = async () => { throw new Error("handler reset"); };
  await assert.rejects(h.paste().alarm());
  assert.equal(h.pasteStorage.alarm - now, 300000, "a dying handler left the alarm due at once");
});

test("Paste append refuses a version too large to persist before any charge", async () => {
  const h = artifactHarness();
  h.pasteStorage.tooBig = (key) => key === "artifactPending";
  const refused = await responseJSON(await h.paste().append(appendBody("append-huge")));
  assert.deepStrictEqual(refused, { status: 413, body: { error: "version-too-large" } });
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
  assert.deepStrictEqual(h.transport.calls, []);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 2);
});

test("Paste delete converges after every local commit crash", async () => {
  await assertCrashConverges({
    harness: deletableArtifactHarness,
    invoke: (h, run) => h.paste().deleteVersion({
      opId: `delete-${run}`, generation: "generation-1", ver: 1,
    }),
    converged(h) {
      assert.equal(storedVersions(h.pasteStorage)[0].deleted, true);
      assert.equal(h.pasteStorage.data.get("row").size, 4);
      assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 4);
      assert.equal(h.identityStorage.data.get("entries").slugone1.servedSize, 4);
      assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  const transport = { failBefore: 0, failAfter: 0, calls: [] };
  const endpoint = {
    async fetch(request) {
      transport.calls.push(new URL(request.url).pathname);
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
  assert.equal(storedVersions(h.pasteStorage).length, 1);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(storedVersions(h.pasteStorage).length, 2);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);

  const replay = await responseJSON(await h.paste().append(appendBody()));
  assert.deepStrictEqual(replay, {
    status: 200,
    body: { appended: true, ver: 2, wasPinned: false, totalSize: 6 },
  });
  assert.equal(storedVersions(h.pasteStorage).length, 2);
});

test("Paste projection reloads state after recovering prior accounting", async () => {
  const h = artifactHarness();
  h.transport.failAfter = 1;
  assert.equal((await h.paste().append(appendBody())).status, 502);

  const renamed = await responseJSON(await h.paste().rename({
    name: "after recovery", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(renamed, { status: 200, body: { changed: true } });
  const row = h.pasteStorage.data.get("row");
  assert.equal(row.accountingVersion, 1);
  assert.equal(row.kind, "markdown");
  assert.equal(row.size, 4);
  assert.equal(row.name, "after recovery");
  const entry = h.identityStorage.data.get("entries").slugone1;
  assert.equal(entry.name, "after recovery");
  assert.equal(entry.servedSize, 4);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste append keeps a quota refusal permanent across later capacity", async () => {
  const h = artifactHarness({ extra: 6 });
  const refused = await responseJSON(await h.paste().append(appendBody("append-refused")));
  assert.deepStrictEqual(refused, { status: 507, body: { error: "over-quota" } });
  assert.equal(storedVersions(h.pasteStorage).length, 1);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);

  const entries = h.identityStorage.data.get("entries");
  delete entries.other;
  await h.identityStorage.put("entries", entries);

  assert.deepStrictEqual(await responseJSON(await h.paste().append(appendBody("append-refused"))), refused);
  assert.equal(storedVersions(h.pasteStorage).length, 1);
  assert.equal((await h.paste().append(appendBody("append-2"))).status, 200);
  assert.equal(storedVersions(h.pasteStorage).length, 2);
});

test("Paste projection remains available after a quota refusal", async () => {
  const h = artifactHarness({ extra: 6 });
  assert.equal((await h.paste().append(appendBody("append-refused"))).status, 507);

  const renamed = await responseJSON(await h.paste().rename({
    name: "still mutable", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(renamed, { status: 200, body: { changed: true } });
  assert.equal(h.identityStorage.data.get("entries").slugone1.name, "still mutable");
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste delete commits before an unavailable allocation release", async () => {
  const h = deletableArtifactHarness();
  h.transport.failBefore = 1;

  const deleted = await responseJSON(await h.paste().deleteVersion({
    opId: "delete-1", generation: "generation-1", ver: 1,
  }));
  assert.deepStrictEqual(deleted, { status: 200, body: { deleted: true, totalSize: 4, upload: "" } });
  assert.equal(storedVersions(h.pasteStorage)[0].deleted, true);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 4);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  assert.equal(storedVersions(h.pasteStorage)[1].deleted, false);
  assert.equal(h.pasteStorage.data.get("row").pinnedVersion, 2);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
});

test("Paste pin fences unchanged charge and projects the served version", async () => {
  const h = deletableArtifactHarness();

  assert.deepStrictEqual(await responseJSON(await h.paste().pin({
    opId: "pin-1", generation: "generation-1", ver: 1,
  })), {
    status: 200, body: { pinned: true, ver: 1 },
  });
  assert.equal(h.pasteStorage.data.get("row").size, 2);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);
  const entry = h.identityStorage.data.get("entries").slugone1;
  assert.equal(entry.chargedSize, 6);
  assert.equal(entry.servedSize, 2);
  assert.equal(entry.pinnedVersion, 1);
});

test("Paste removal fences release before allowing a new incarnation", async () => {
  const h = artifactHarness();
  h.transport.failBefore = 1;

  const removed = await responseJSON(await h.paste().remove({
    opId: "remove-1", generation: "generation-1", identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true, uploads: [] } });
  assert.equal(h.pasteStorage.data.has("row"), false);
  assert.equal(h.pasteStorage.data.get("artifactTombstone").generation, "generation-1");
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 2);

  await h.paste().alarm();
  assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);
  assert.equal((await h.paste().put({
    generation: "generation-2",
    fingerprint: "fingerprint-2",
    row: {
      slug: "slugone1", identity: "other", kind: "html",
      size: 1, status: "ready", createdAt: 9, updatedAt: 9,
    },
  })).status, 204);

  assert.deepStrictEqual(await responseJSON(await h.paste().remove({
    opId: "remove-1", generation: "generation-1", identity: "owner", createdAt: 7,
  })), removed);
  assert.equal(h.pasteStorage.data.get("row").generation, "generation-2");
});

test("Paste receipts are scoped to one artifact generation", async () => {
  const h = artifactHarness();
  assert.equal((await h.paste().append(appendBody("shared-op"))).status, 200);
  assert.equal((await h.paste().remove({
    opId: "remove-generation-1", generation: "generation-1",
    identity: "owner", createdAt: 7,
  })).status, 200);
  assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);

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
  assert.equal(storedVersions(h.pasteStorage).length, 2);
});

test("Paste rejects an old incarnation without mutating the replacement allocation", async () => {
  const identitySeed = artifactIdentitySeed({ generation: "generation-2" });
  const h = artifactHarness({ identitySeed });

  assert.equal((await h.paste().append(appendBody("stale-generation"))).status, 409);
  assert.equal(storedVersions(h.pasteStorage).length, 1);
  assert.equal(h.identityStorage.data.get("entries").slugone1.generation, "generation-2");
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 2);
});

test("Paste legacy row adopts on first mutation and survives response loss", async () => {
  const pasteSeed = artifactPasteSeed();
  delete pasteSeed.get("row").generation;
  delete pasteSeed.get("row").accountingVersion;
  const identitySeed = new Map([["entries", {
    slugone1: { size: 2, status: "ready", at: 7, updatedAt: 7 },
  }]]);
  const h = artifactHarness({ pasteSeed, identitySeed });
  h.transport.failAfter = 1;

  const first = await h.paste().append({ ...appendBody("legacy-op"), generation: "" });
  assert.equal(first.status, 502);
  const generation = h.pasteStorage.data.get("row").generation;
  assert.ok(generation);
  assert.equal(h.pasteStorage.data.get("legacyAdoptionPending"), true);

  const retried = await responseJSON(await h.paste().append({
    ...appendBody("legacy-op"), generation,
  }));
  assert.equal(retried.status, 200);
  assert.equal(retried.body.appended, true);
  assert.equal(h.pasteStorage.data.get("legacyAdoptionPending"), undefined);
  assert.equal(h.pasteStorage.data.get("row").generation, generation);
  const entry = h.identityStorage.data.get("entries").slugone1;
  assert.equal(entry.generation, generation);
  assert.equal(entry.chargedSize, 6);
});

test("Paste empty generation against an adopted row is a conflict", async () => {
  const h = artifactHarness();
  const refused = await responseJSON(await h.paste().append({
    ...appendBody("empty-generation"), generation: "",
  }));
  assert.equal(refused.status, 409);
  assert.equal(refused.body.error, "generation-mismatch");
  assert.equal(storedVersions(h.pasteStorage).length, 1);
});

test("Paste append answers an accounting conflict with 409 whatever Identity answered", async () => {
  const h = artifactHarness({ identitySeed: new Map() });
  const conflict = await responseJSON(await h.paste().append({ ...appendBody(), uploadId: "up-2" }));
  assert.deepStrictEqual(conflict, { status: 409, body: { error: "artifact-accounting-conflict" } });
  assert.equal(storedVersions(h.pasteStorage).length, 1);
  assert.notEqual(h.pasteStorage.data.get("artifactPending"), undefined);
});

test("Paste legacy adoption answers a refused seed with 409 whatever Identity answered", async () => {
  const pasteSeed = artifactPasteSeed();
  const row = pasteSeed.get("row");
  delete row.generation;
  delete row.accountingVersion;
  delete row.size;
  const h = artifactHarness({ pasteSeed, identitySeed: new Map() });
  const refused = await responseJSON(await h.paste().append({ ...appendBody("legacy-op"), generation: "" }));
  assert.equal(refused.status, 409);
  assert.equal(refused.body.error, "adoption-seed-conflict");
  assert.equal(storedVersions(h.pasteStorage).length, 1);
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
  assert.equal(h.pasteStorage.data.get("row").name, "new name");
  assert.equal(h.identityStorage.data.get("entries").slugone1.name, "new name");
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  assert.equal(h.pasteStorage.data.get("row").status, "failed");
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 2);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);

  const removed = await responseJSON(await h.paste().remove({
    opId: "remove-failed", generation: "generation-1",
    identity: "owner", createdAt: 7,
  }));
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true, uploads: [] } });
  assert.equal(h.pasteStorage.data.get("row"), undefined);
  assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
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
    converged(h) {
      assert.equal(h.pasteStorage.data.get("row").status, "failed");
      assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);
      assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
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
  assert.equal(storage.data.get("ver:1").uploadId, "up-1");
  assert.equal(storage.data.get("row").uploadId, "up-1");

  const older = new FakeStorage();
  const { uploadId: _uploadId, ...oldRow } = row;
  assert.equal((await create(older, oldRow)).status, 204);
  assert.equal(Object.hasOwn(older.data.get("ver:1"), "uploadId"), false);

  const malformed = new FakeStorage();
  assert.equal((await create(malformed, { ...row, uploadId: 7 })).status, 400);
  assert.equal(malformed.commits, 0);
});

test("Paste append records its upload id and versions list every version's id", async () => {
  const h = artifactHarness();
  const legacy = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(legacy.body.map((version) => version.uploadId), [""]);

  assert.equal((await h.paste().append({ ...appendBody("append-bad"), uploadId: 7 })).status, 400);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);

  assert.equal((await h.paste().append({ ...appendBody(), uploadId: "up-2" })).status, 200);
  assert.equal(h.pasteStorage.data.get("ver:2").uploadId, "up-2");
  const listed = await responseJSON(await h.paste().listVersions());
  assert.deepStrictEqual(listed.body.map(({ ver, uploadId }) => ({ ver, uploadId })), [
    { ver: 2, uploadId: "up-2" },
    { ver: 1, uploadId: "" },
  ]);
});

test("Paste head carries the served version's upload id through append, pin, and unpin", async () => {
  const h = artifactHarness();
  const head = () => h.pasteStorage.data.get("row").uploadId;
  assert.equal((await h.paste().append({ ...appendBody("append-2"), uploadId: "up-2" })).status, 200);
  assert.equal(head(), "up-2");
  assert.equal((await h.paste().append({ ...appendBody("append-3"), uploadId: "up-3" })).status, 200);
  assert.equal(head(), "up-3");

  const pin = (opId, ver) => h.paste().pin({ opId, generation: "generation-1", ver });
  assert.equal((await pin("pin-2", 2)).status, 200);
  assert.equal(head(), "up-2");
  assert.equal((await pin("unpin", 0)).status, 200);
  assert.equal(head(), "up-3");
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
  assert.equal(hasContentSha(created.identityStorage.data.get("entries").newslug2), false);
  assert.equal(hasContentSha(created.pasteStorage.data.get("row")), false);
  assert.equal(hasContentSha(created.pasteStorage.data.get("ver:1")), false);
  assert.equal(hasContentSha((await responseJSON(await created.paste.get())).body), false);

  const h = artifactHarness();
  assert.equal((await h.paste().append({ ...appendBody(), contentSha: "v2" })).status, 200);
  assert.equal(hasContentSha(h.pasteStorage.data.get("ver:2")), false);
  assert.equal(hasContentSha(h.pasteStorage.data.get("row")), false);
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
  assert.equal(hasContentSha(h.pasteStorage.data.get("row")), false);
  assert.equal(hasContentSha(h.identityStorage.data.get("entries").slugone1), false);

  const legacy = new FakeStorage(new Map([["entries", {
    slugone1: { size: 2, status: "ready", at: 7, kind: "html", contentSha: "v1" },
  }]]));
  assert.equal((await new Identity(state(legacy)).artifactSeed({
    slug: "slugone1", generation: "generation-1", charge: 2, servedSize: 2,
  })).status, 200);
  assert.equal(hasContentSha(legacy.data.get("entries").slugone1), false);
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
  assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);

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
    converged(h) {
      const receipts = [...h.pasteStorage.data]
        .filter(([key]) => key.startsWith("artifact-receipt:"))
        .map(([, receipt]) => receipt.body);
      assert.deepStrictEqual(receipts, [{ removed: true, uploads: ["up-1", "up-3"] }]);
      assert.equal(h.pasteStorage.data.get("row"), undefined);
      assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);
      assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
      assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);
    },
  });
});

test("Paste version delete names the tombstoned upload on every path", async () => {
  const h = artifactHarness();
  await appendUploads(h, [2, 3]);
  const body = { opId: "delete-2", generation: "generation-1", ver: 2 };
  const expected = { status: 200, body: { deleted: true, totalSize: 3, upload: "up-2" } };

  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion(body)), expected);
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion(body)), expected);
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion({
    ...body, opId: "delete-2-again",
  })), expected);
  assert.deepStrictEqual(await responseJSON(await h.paste().deleteVersion({
    opId: "delete-1", generation: "generation-1", ver: 1,
  })), { status: 200, body: { deleted: true, totalSize: 1, upload: "" } });
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
  const transport = { failBefore: 0, failAfter: 0, calls: [] };
  const endpoint = {
    async fetch(request) {
      transport.calls.push(new URL(request.url).pathname);
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
    pasteStorage,
    transport,
    room: () => new Room(state(roomStorage), env),
  };
}

async function responseJSON(response) {
  return { status: response.status, body: await response.json() };
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
  assert.equal(Object.keys(storage.data.get("rows")).length, 1);
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
  assert.deepStrictEqual(storage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111"), {
    version: 1, allocated: 4, target: 7,
  });

  const release = await paste.roomDecide({
    room: "r2", version: 1, targetBytes: 0, appCap: 10,
  });
  assert.equal(release.status, 200);
  assert.equal(storage.data.get("roomAllocated"), 4);

  result = await decide(paste, { version: 1, targetBytes: 7 });
  assert.equal(result.status, 507);
  assert.equal(storage.data.get("roomAllocated"), 4);

  result = await decide(paste, { version: 2, targetBytes: 6 });
  assert.equal(result.status, 200);
  assert.equal(storage.data.get("roomAllocated"), 6);
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
  const doc = h.roomStorage.data.get("state");
  assert.equal(doc.bytes, 6);
  assert.equal(doc.seq, 8);
  assert.equal(doc.budgetVersion, 1);
  assert.equal(doc.pending, undefined);
  assert.equal(h.roomStorage.alarm, null);
  assert.equal(h.pasteStorage.data.get("roomAllocated"), 10);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 6);
});

test("Room growth refusal advances only the budget version", async () => {
  const h = roomHarness();
  const result = await responseJSON(await h.room().put(putBody("aaaaaaa")));
  assert.equal(result.status, 507);
  const doc = h.roomStorage.data.get("state");
  assert.equal(doc.bytes, 4);
  assert.equal(doc.seq, 7);
  assert.equal(doc.budgetVersion, 1);
  assert.equal(doc.pending, undefined);
  assert.equal(h.roomStorage.alarm, null);
  assert.deepStrictEqual(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111"), {
    version: 1, allocated: 4, target: 7,
  });
});

test("Room alarm recovers a crash after growth preparation", async () => {
  const h = roomHarness();
  h.roomStorage.crashAfter = 1;
  await assert.rejects(() => h.room().put(putBody("aaaaaa")), SimulatedCrash);

  let doc = h.roomStorage.data.get("state");
  assert.equal(doc.bytes, 4);
  assert.equal(doc.seq, 7);
  const growth = putBody("aaaaaa");
  assert.deepStrictEqual(doc.pending, {
    targetBytes: 6,
    appCap: 10,
    mutation: {
      key: "key",
      value: growth.value,
      wire: growth.wire,
      now: 9,
    },
  });
  assert.notEqual(h.roomStorage.alarm, null);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 4);

  h.roomStorage.commits = 0;
  h.roomStorage.crashAfter = Infinity;
  await h.room().alarm();
  doc = h.roomStorage.data.get("state");
  assert.equal(doc.bytes, 6);
  assert.equal(doc.seq, 8);
  assert.equal(doc.pending, undefined);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 6);
});

test("Room alarm replays a committed decision after response loss", async () => {
  const h = roomHarness();
  h.transport.failAfter = 1;
  const result = await responseJSON(await h.room().put(putBody("aaaaaa")));
  assert.equal(result.status, 502);
  assert.equal(h.roomStorage.data.get("state").bytes, 4);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 6);
  assert.notEqual(h.roomStorage.alarm, null);

  await h.room().alarm();
  assert.equal(h.roomStorage.data.get("state").bytes, 6);
  assert.equal(h.roomStorage.data.get("state").seq, 8);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room alarm retries a transient coordinator failure", async () => {
  const h = roomHarness();
  h.transport.failBefore = 1;
  const result = await responseJSON(await h.room().put(putBody("aaaaaa")));
  assert.equal(result.status, 502);
  assert.equal(h.roomStorage.data.get("state").bytes, 4);
  assert.notEqual(h.roomStorage.alarm, null);

  await h.room().alarm();
  assert.equal(h.roomStorage.data.get("state").bytes, 6);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room shrink commits before releasing sibling capacity", async () => {
  const h = roomHarness({ room: roomDoc({ bytes: 6 }), pasteSeed: budgetSeed(6, 4) });
  const result = await responseJSON(await h.room().put(putBody("aa")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 2 } });
  assert.equal(h.roomStorage.data.get("state").bytes, 2);
  assert.equal(h.pasteStorage.data.get("roomAllocated"), 6);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 2);
  assert.equal(h.roomStorage.alarm, null);
});

test("Room equal-size mutations bypass the coordinator", async () => {
  const h = roomHarness();
  let result = await responseJSON(await h.room().put(putBody("bbbb")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 4 } });
  result = await responseJSON(await h.room().del({ key: "absent", now: 10 }));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 9, bytes: 4 } });
  assert.equal(h.roomStorage.data.get("state").budgetVersion, 0);
  assert.equal(h.transport.calls.filter((path) => path.endsWith("/roomdecide")).length, 0);
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
  assert.equal(storage.data.get("state").seq, 9);
});

test("Room shrink succeeds after commit when allocation release is unavailable", async () => {
  const h = roomHarness({ room: roomDoc({ bytes: 6 }), pasteSeed: budgetSeed(6, 4) });
  h.transport.failBefore = 1;
  const result = await responseJSON(await h.room().put(putBody("aa")));
  assert.deepStrictEqual(result, { status: 200, body: { seq: 8, bytes: 2 } });
  const doc = h.roomStorage.data.get("state");
  assert.equal(doc.bytes, 2);
  assert.deepStrictEqual(doc.pending, { targetBytes: 2, appCap: 10 });
  assert.notEqual(h.roomStorage.alarm, null);
  assert.equal(h.pasteStorage.data.get("roomAllocation:11111111-1111-4111-8111-111111111111").allocated, 6);
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
  assert.equal(roomStorage.data.has("state"), false);
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
  assert.equal(h.roomStorage.data.has("state"), false);
  assert.equal(h.pasteStorage.data.has("roomLedger"), false);
});
