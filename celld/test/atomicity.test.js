import assert from "node:assert/strict";
import test from "node:test";

import worker, { Identity, Paste, Room, Subnet } from "../src/index.js";

class SimulatedCrash extends Error {}

const PASTE_CELL_ID = "a".repeat(64);
const ROOM_CELL_ID = "b".repeat(64);
const ROOM_ID = "11111111-1111-4111-8111-111111111111";

function fakeCellID(value) {
  return { toString() { return value; } };
}

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

class FakeStorage {
  constructor(seed = new Map(), staged = false, alarm = null) {
    this.data = new Map([...seed].map(([key, value]) => [key, clone(value)]));
    this.staged = staged;
    this.alarm = alarm;
    this.commits = 0;
    this.crashAfter = Infinity;
  }

  async get(key) {
    return clone(this.data.get(key));
  }

  async put(keyOrEntries, value) {
    const entries = typeof keyOrEntries === "string"
      ? [[keyOrEntries, value]]
      : keyOrEntries instanceof Map
        ? [...keyOrEntries]
        : Object.entries(keyOrEntries);
    for (const [key, entry] of entries) {
      this.data.set(String(key), clone(entry));
    }
    this.afterCommit();
  }

  async delete(keyOrKeys) {
    const keys = Array.isArray(keyOrKeys) ? keyOrKeys : [keyOrKeys];
    for (const key of keys) {
      this.data.delete(String(key));
    }
    this.afterCommit();
  }

  async list({ prefix = "" } = {}) {
    return new Map([...this.data]
      .filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, clone(value)]));
  }

  async getAlarm() {
    return this.alarm;
  }

  async setAlarm(at) {
    this.alarm = at;
    this.afterCommit();
  }

  async deleteAlarm() {
    this.alarm = null;
    this.afterCommit();
  }

  async transaction(callback) {
    const tx = new FakeStorage(this.data, true, this.alarm);
    const result = await callback(tx);
    this.data = tx.data;
    this.alarm = tx.alarm;
    this.afterCommit();
    return result;
  }

  afterCommit() {
    if (this.staged) {
      return;
    }
    this.commits++;
    if (this.commits === this.crashAfter) {
      throw new SimulatedCrash("process stopped after a durable commit");
    }
  }
}

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

function state(storage) {
  return {
    storage,
    acceptWebSocket() {},
    getWebSockets() { return []; },
    blockConcurrencyWhile(callback) { return callback(); },
  };
}

test("Identity.reserve commits quota, first-seen, and intent together", async () => {
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
      contentSha: "abc", size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 204);

  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2.status, "pending");
  assert.equal(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.alarm, null);
});

test("Identity create alarm releases a definitely absent paste", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

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

  await h.identity.alarm();
  assert.notEqual(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get("entries").newslug2.generation, body.generation);
  assert.notEqual(h.identityStorage.alarm, null);
});

test("Identity create alarm fences a late put before releasing quota", async () => {
  const h = createIntentHarness();
  const body = createIntentBody();
  assert.equal((await h.identity.reserve(body)).status, 200);

  await h.identity.alarm();
  assert.equal(h.identityStorage.data.get("entries").newslug2, undefined);
  assert.equal((await h.paste.put({
    generation: body.generation,
    fingerprint: body.intent.fingerprint,
    row: {
      slug: body.slug, identity: "owner", status: "pending", kind: "html",
      contentSha: "abc", size: body.size, createdAt: 9, updatedAt: 9,
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
      contentSha: "abc", size: body.size, createdAt: 9, updatedAt: 9,
    },
  })).status, 204);
  const entries = h.identityStorage.data.get("entries");
  entries.newslug2.status = "ready";
  h.identityStorage.data.set("entries", entries);

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
      contentSha: "different", size: 99, createdAt: 10, updatedAt: 10,
    },
  })).status, 204);

  await h.identity.alarm();
  assert.notEqual(h.identityStorage.data.get(`intent:${body.intent.id}`), undefined);
  assert.equal(h.identityStorage.data.get("entries").newslug2.status, "pending");
  assert.notEqual(h.identityStorage.alarm, null);
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
    slug: "newslug2", identity: "owner", kind: "html", contentSha: "original",
    size: 3, createdAt: 7, updatedAt: 7, status: "pending",
  };
  assert.equal((await paste.put({
    row: original, generation: "generation-1", fingerprint: "fingerprint-1",
  })).status, 204);
  const replay = { ...original, contentSha: "replacement", size: 99 };
  assert.equal((await paste.put({
    row: original, generation: "generation-1", fingerprint: "fingerprint-1",
  })).status, 204);
  assert.equal((await paste.put({
    row: replay, generation: "generation-1", fingerprint: "fingerprint-other",
  })).status, 409);
  assert.equal(storage.data.get("row").contentSha, "original");
  assert.equal(storage.data.get("versions").length, 1);
  assert.equal((await paste.put({
    row: replay, generation: "generation-2", fingerprint: "fingerprint-2",
  })).status, 409);
});

test("Paste.put commits row, version seed, and counter together", async () => {
  const row = {
    identity: "owner", kind: "html", contentSha: "abc", size: 3,
    createdAt: 7, updatedAt: 7, status: "ready",
  };
  await assertCrashAtomic({
    invoke: (storage) => new Paste(state(storage)).put({
      row, generation: "generation-1", fingerprint: "fingerprint-1",
    }),
  });
});

test("Paste append converges after every local commit crash", async () => {
  for (const boundary of [1, 2, 3]) {
    const h = artifactHarness();
    h.pasteStorage.crashAfter = boundary;
    await assert.rejects(() => h.paste().append(appendBody(`append-crash-${boundary}`)), SimulatedCrash);

    h.pasteStorage.commits = 0;
    h.pasteStorage.crashAfter = Infinity;
    if (h.pasteStorage.data.get("artifactPending")) {
      await h.paste().alarm();
    }
    assert.equal(h.pasteStorage.data.get("versions").length, 2);
    assert.equal(h.pasteStorage.data.get("maxVer"), 2);
    assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);
    assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
    assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
    assert.equal(h.pasteStorage.alarm, null);
  }
});

test("Paste serializes concurrent version appends", async () => {
  const h = artifactHarness({ serial: true });
  const paste = h.paste();
  const append = (opId, contentSha) => paste.fetch(new Request("https://cell/paste/append", {
    method: "POST",
    body: JSON.stringify({
      opId, generation: "generation-1", userCap: 10,
      kind: "html", contentSha, size: 2, now: 8,
    }),
  }));
  const results = await Promise.all([append("append-2", "v2"), append("append-3", "v3")]);
  assert.deepStrictEqual((await Promise.all(results.map(responseJSON))).map((result) => result.body.ver), [2, 3]);
  assert.equal(h.pasteStorage.data.get("maxVer"), 3);
  assert.deepStrictEqual(h.pasteStorage.data.get("versions").map((version) => version.ver), [1, 2, 3]);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
});

test("Paste delete converges after every local commit crash", async () => {
  for (const boundary of [1, 2, 3]) {
    const h = deletableArtifactHarness();
    h.pasteStorage.crashAfter = boundary;
    await assert.rejects(() => h.paste().deleteVersion({
      opId: `delete-crash-${boundary}`, generation: "generation-1", ver: 1,
    }), SimulatedCrash);

    h.pasteStorage.commits = 0;
    h.pasteStorage.crashAfter = Infinity;
    if (h.pasteStorage.data.get("artifactPending")) {
      await h.paste().alarm();
    }
    assert.equal(h.pasteStorage.data.get("versions")[0].deleted, true);
    assert.equal(h.pasteStorage.data.get("row").size, 4);
    assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 4);
    assert.equal(h.identityStorage.data.get("entries").slugone1.servedSize, 4);
    assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
    assert.equal(h.pasteStorage.alarm, null);
  }
});

function artifactPasteSeed({ generation = "generation-1", charge = 2 } = {}) {
  return new Map([
    ["row", {
      slug: "slugone1", identity: "owner", kind: "html", contentSha: "v1", size: 2,
      createdAt: 7, updatedAt: 7, status: "ready", pinnedVersion: 0,
      generation, accountingVersion: 0,
    }],
    ["versions", [{
      ver: 1, kind: "html", contentSha: "v1", size: charge,
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
      latestVersion: 1, kind: "html", name: "", contentSha: "v1",
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

function deletableArtifactHarness() {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("versions").push({
    ver: 2, kind: "markdown", contentSha: "v2", size: 4,
    createdAt: 8, deleted: false, manifest: null,
  });
  pasteSeed.set("maxVer", 2);
  const row = pasteSeed.get("row");
  row.kind = "markdown";
  row.contentSha = "v2";
  row.size = 4;
  const identitySeed = artifactIdentitySeed({ charge: 6 });
  const entry = identitySeed.get("entries").slugone1;
  entry.servedSize = 4;
  entry.latestVersion = 2;
  entry.contentSha = "v2";
  return artifactHarness({ pasteSeed, identitySeed });
}

function appendBody(opId = "append-1", userCap = 10) {
  return {
    opId, generation: "generation-1", userCap, kind: "markdown",
    contentSha: "v2", size: 4, now: 8,
  };
}

test("Paste append recovers a response-lost reservation without duplicating the version", async () => {
  const h = artifactHarness();
  h.transport.failAfter = 1;

  assert.equal((await h.paste().append(appendBody())).status, 502);
  assert.equal(h.pasteStorage.data.get("versions").length, 1);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(h.pasteStorage.data.get("versions").length, 2);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);

  const replay = await responseJSON(await h.paste().append(appendBody()));
  assert.deepStrictEqual(replay, {
    status: 200,
    body: { appended: true, ver: 2, wasPinned: false, totalSize: 6 },
  });
  assert.equal(h.pasteStorage.data.get("versions").length, 2);
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
  assert.equal(row.contentSha, "v2");
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
  assert.equal(h.pasteStorage.data.get("versions").length, 1);
  assert.equal(h.pasteStorage.data.get("row").accountingVersion, 1);

  const entries = h.identityStorage.data.get("entries");
  delete entries.other;
  await h.identityStorage.put("entries", entries);

  assert.deepStrictEqual(await responseJSON(await h.paste().append(appendBody("append-refused"))), refused);
  assert.equal(h.pasteStorage.data.get("versions").length, 1);
  assert.equal((await h.paste().append(appendBody("append-2"))).status, 200);
  assert.equal(h.pasteStorage.data.get("versions").length, 2);
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
  assert.deepStrictEqual(deleted, { status: 200, body: { deleted: true, totalSize: 4 } });
  assert.equal(h.pasteStorage.data.get("versions")[0].deleted, true);
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 6);
  assert.notEqual(h.pasteStorage.alarm, null);

  await h.paste().alarm();
  assert.equal(h.identityStorage.data.get("entries").slugone1.chargedSize, 4);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste delete refuses the version served at commit time", async () => {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("versions").push({
    ver: 2, kind: "markdown", contentSha: "v2", size: 4,
    createdAt: 8, deleted: false, manifest: null,
  });
  pasteSeed.set("maxVer", 2);
  const row = pasteSeed.get("row");
  row.pinnedVersion = 2;
  row.kind = "markdown";
  row.contentSha = "v2";
  row.size = 4;
  const identitySeed = artifactIdentitySeed({ charge: 6 });
  identitySeed.get("entries").slugone1.servedSize = 4;
  const h = artifactHarness({ pasteSeed, identitySeed });

  const deleted = await responseJSON(await h.paste().deleteVersion({
    opId: "delete-served", generation: "generation-1", ver: 2,
  }));
  assert.deepStrictEqual(deleted, {
    status: 409, body: { error: "version-served" },
  });
  assert.equal(h.pasteStorage.data.get("versions")[1].deleted, false);
  assert.equal(h.pasteStorage.data.get("row").pinnedVersion, 2);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
});

test("Paste pin fences unchanged charge and projects the served version", async () => {
  const pasteSeed = artifactPasteSeed();
  pasteSeed.get("versions").push({
    ver: 2, kind: "markdown", contentSha: "v2", size: 4,
    createdAt: 8, deleted: false, manifest: null,
  });
  pasteSeed.set("maxVer", 2);
  pasteSeed.get("row").size = 4;
  pasteSeed.get("row").kind = "markdown";
  pasteSeed.get("row").contentSha = "v2";
  const identitySeed = artifactIdentitySeed({ charge: 6 });
  identitySeed.get("entries").slugone1.servedSize = 4;
  const h = artifactHarness({ pasteSeed, identitySeed });

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
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true } });
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
      slug: "slugone1", identity: "other", kind: "html", contentSha: "new",
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
      slug: "slugone1", identity: "owner", kind: "html", contentSha: "new-v1",
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
  assert.equal(h.pasteStorage.data.get("versions").length, 2);
});

test("Paste rejects an old incarnation without mutating the replacement allocation", async () => {
  const identitySeed = artifactIdentitySeed({ generation: "generation-2" });
  const h = artifactHarness({ identitySeed });

  assert.equal((await h.paste().append(appendBody("stale-generation"))).status, 409);
  assert.equal(h.pasteStorage.data.get("versions").length, 1);
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
  assert.equal(h.pasteStorage.data.get("versions").length, 1);
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
  assert.deepStrictEqual(removed, { status: 200, body: { removed: true } });
  assert.equal(h.pasteStorage.data.get("row"), undefined);
  assert.equal(h.pasteStorage.data.get("artifactTombstone"), undefined);
  assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
  assert.equal(h.pasteStorage.alarm, null);
});

test("Paste failure converges after every local commit crash", async () => {
  for (const boundary of [1, 2, 3]) {
    const pasteSeed = artifactPasteSeed();
    pasteSeed.get("row").status = "pending";
    const identitySeed = artifactIdentitySeed();
    identitySeed.get("entries").slugone1.status = "pending";
    const h = artifactHarness({ pasteSeed, identitySeed });
    h.pasteStorage.crashAfter = boundary;
    const body = {
      status: "failed", generation: "generation-1",
      opId: `fail-crash-${boundary}`,
    };

    await assert.rejects(() => h.paste().setStatus(body), SimulatedCrash);
    h.pasteStorage.commits = 0;
    h.pasteStorage.crashAfter = Infinity;
    if (h.pasteStorage.data.get("artifactPending")) {
      await h.paste().alarm();
    }
    assert.equal(h.pasteStorage.data.get("row").status, "failed");
    assert.equal(h.identityStorage.data.get("entries").slugone1, undefined);
    assert.equal(h.pasteStorage.data.get("artifactPending"), undefined);
    assert.equal(h.pasteStorage.alarm, null);
  }
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

function roomDoc(bytes = 4) {
  const text = "a".repeat(bytes);
  return {
    meta: { appSlug: "appslug1", id: "11111111-1111-4111-8111-111111111111", createdAt: 1, updatedAt: 1 },
    kv: { key: Buffer.from(text).toString("base64") },
    wire: { key: JSON.stringify(text) },
    bytes,
    seq: 7,
    budgetVersion: 0,
  };
}

function putBody(text, appCap = 10) {
  return {
    key: "key",
    value: Buffer.from(text).toString("base64"),
    wire: JSON.stringify(text),
    roomCap: 100,
    keyCap: 10,
    appCap,
    now: 9,
  };
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
  const h = roomHarness({ room: roomDoc(6), pasteSeed: budgetSeed(6, 4) });
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
  const h = roomHarness({ room: roomDoc(6), pasteSeed: budgetSeed(6, 4) });
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
