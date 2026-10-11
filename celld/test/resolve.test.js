import assert from "node:assert/strict";
import test from "node:test";

import { Identity, Paste } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";

const CLASSES = { IDENTITY: Identity, PASTES: Paste };
const OWNER = "key:SHA256:resolveowner";
const NAME = "a private label";
const ANSWER_FIELDS = new Set(["found", "status", "action", "kind", "updatedAt", "uploadId", "entry", "shell", "files"]);

// Cells over one shared map of storages, so cell-to-cell calls land on the
// state a test inspects. Counts the exclusive sections a cell enters.
function world() {
  const storages = new Map();
  const env = {};
  const exclusive = { count: 0 };
  const storageOf = (ns, name) => {
    const key = `${ns}/${name}`;
    if (!storages.has(key)) {
      storages.set(key, new FakeStorage());
    }
    return storages.get(key);
  };
  const cell = (ns, name) => new CLASSES[ns]({
    ...state(storageOf(ns, name)),
    blockConcurrencyWhile(callback) {
      exclusive.count++;
      return callback();
    },
  }, env);
  for (const ns of Object.keys(CLASSES)) {
    env[ns] = {
      idFromName(name) { return name; },
      get(name) { return { fetch: (request) => cell(ns, name).fetch(request) }; },
    };
  }
  return { cell, storageOf, exclusive };
}

async function call(cell, path, body) {
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  const res = await cell.fetch(new Request(`https://cell${path}`, init));
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    // not every answer is JSON
  }
  return { status: res.status, body: json, text };
}

async function ok(cell, path, body) {
  const res = await call(cell, path, body);
  assert.ok(res.status < 300, `${path}: ${res.status} ${res.text}`);
  return res.body;
}

function manifest(uploadId, files) {
  return {
    Files: Object.fromEntries(Object.entries(files).map(([path, contentType], n) => [path, {
      Key: `uploads/${uploadId}/${n}`, Size: 10 + n, CompressedSize: 5 + n, ContentType: contentType, Kind: "",
    }])),
  };
}

// Creates slug through the cells' own write path: reserve, put, confirm.
async function create(w, slug, { kind, status = "ready", uploadId, files }) {
  const generation = `generation-${slug}`;
  const intentId = `create:${slug}:${generation}`;
  const owner = w.cell("IDENTITY", OWNER);
  await ok(owner, `/identity/reserve?scope=${OWNER}`, {
    slug, generation, size: 3, userCap: 10_000, status, now: 9, updatedAt: 9,
    intent: { id: intentId, kind: "create_paste", subject: slug, fingerprint: `fp-${slug}`, startedAt: 9 },
  });
  await ok(w.cell("PASTES", slug), `/paste/put?slug=${slug}`, {
    generation, fingerprint: `fp-${slug}`,
    row: {
      slug, identity: OWNER, status, kind, size: 3, name: NAME, pinnedVersion: 0,
      createdAt: 9, updatedAt: 1_760_000_000_123, uploadId, manifest: manifest(uploadId, files),
    },
  });
  await ok(owner, `/identity/confirm?scope=${OWNER}`, { slug, generation, status, intentId });
  return { generation, paste: () => w.cell("PASTES", slug) };
}

function resolvePath(slug, path, flags = "") {
  return `/paste/resolve?slug=${slug}&path=${encodeURIComponent(path)}${flags}`;
}

// Every answer is the small shape: no identity, name, generation or other
// version reaches it.
function assertSmall(res, ...secrets) {
  assert.equal(res.status, 200, res.text);
  for (const field of Object.keys(res.body)) {
    assert.ok(ANSWER_FIELDS.has(field), `answer field ${field}`);
  }
  if (res.body.entry) {
    assert.deepEqual(Object.keys(res.body.entry).sort(), ["contentType", "key", "size"]);
  }
  for (const secret of [OWNER, NAME, ...secrets]) {
    assert.ok(!res.text.includes(secret), `answer carries ${secret}`);
  }
}

test("resolve answers a stored document", async () => {
  const w = world();
  const { generation, paste } = await create(w, "docslug1", {
    kind: "markdown", uploadId: "upload-doc", files: { "/": "" },
  });
  const common = { kind: "markdown", updatedAt: 1_760_000_000_123, uploadId: "upload-doc" };

  const bare = await call(paste(), resolvePath("docslug1", "/"));
  assertSmall(bare, generation);
  assert.deepEqual(bare.body, { action: "shell", ...common, shell: "markdown" });

  const raw = await call(paste(), resolvePath("docslug1", "/", "&raw=1"));
  assertSmall(raw, generation);
  assert.deepEqual(raw.body, {
    action: "raw", ...common, entry: { key: "uploads/upload-doc/0", contentType: "", size: 10 },
  });

  assert.deepEqual((await call(paste(), resolvePath("docslug1", "/deeper"))).body, { action: "notfound", ...common });
});

test("resolve answers a stored site from its served version only", async () => {
  const w = world();
  const { generation, paste } = await create(w, "siteslg1", {
    kind: "site", uploadId: "upload-old", files: { "index.html": "text/html; charset=utf-8" },
  });
  await ok(paste(), "/paste/append?slug=siteslg1", {
    opId: "append-1", generation, userCap: 10_000, kind: "site", size: 4, now: 10, uploadId: "upload-new",
    manifest: manifest("upload-new", {
      "index.html": "text/html; charset=utf-8",
      "a b/café.css": "text/css; charset=utf-8",
    }),
  });

  const file = await call(paste(), resolvePath("siteslg1", "/a b/café.css"));
  assertSmall(file, generation, "upload-old");
  assert.equal(file.body.action, "file");
  assert.equal(file.body.uploadId, "upload-new");
  assert.deepEqual(file.body.entry, { key: "uploads/upload-new/1", contentType: "text/css; charset=utf-8", size: 11 });

  const route = await call(paste(), resolvePath("siteslg1", "/users/1"));
  assertSmall(route, generation, "upload-old");
  assert.equal(route.body.entry.key, "uploads/upload-new/0");

  const missing = await call(paste(), resolvePath("siteslg1", "/missing.js"));
  assertSmall(missing, generation, "upload-old");
  assert.equal(missing.body.action, "notfound");
});

test("resolve answers a stored knowledge base", async () => {
  const w = world();
  const { generation, paste } = await create(w, "kbslug11", {
    kind: "knowledgebase", uploadId: "upload-kb",
    files: { "README.md": "text/plain; charset=utf-8", "img/a.png": "image/png" },
  });

  const doc = await call(paste(), resolvePath("kbslug11", "/README.md", "&raw=1"));
  assertSmall(doc, generation);
  assert.equal(doc.body.action, "kbraw");
  assert.equal(doc.body.entry.key, "uploads/upload-kb/0");

  const shell = await call(paste(), resolvePath("kbslug11", "/README.md"));
  assertSmall(shell, generation);
  assert.equal(shell.body.action, "kbshell");

  const list = await call(paste(), resolvePath("kbslug11", "/img/a.png", "&files=1"));
  assertSmall(list, generation);
  assert.deepEqual(list.body.files, ["README.md", "img/a.png"]);
});

test("a pending document resolves to its status and exists", async () => {
  const w = world();
  const { paste } = await create(w, "pendslg1", {
    kind: "markdown", status: "pending", uploadId: "upload-pending", files: { "/": "" },
  });
  assert.deepEqual((await call(paste(), resolvePath("pendslg1", "/"))).body, { status: "pending" });
  assert.deepEqual((await call(paste(), "/paste/exists?slug=pendslg1")).body, { exists: true });
});

test("a missing slug is not found and does not exist", async () => {
  const paste = new Paste(state(new FakeStorage()));
  const resolved = await call(paste, resolvePath("nothere1", "/"));
  assert.equal(resolved.status, 200);
  assert.deepEqual(resolved.body, { found: false });
  const exists = await call(paste, "/paste/exists?slug=nothere1");
  assert.equal(exists.status, 200);
  assert.deepEqual(exists.body, { exists: false });
});

test("resolve refuses a path that is absent or not absolute", async () => {
  const paste = new Paste(state(new FakeStorage()));
  for (const path of ["/paste/resolve?slug=noslug11", resolvePath("noslug11", ""), resolvePath("noslug11", "x")]) {
    const res = await call(paste, path);
    assert.equal(res.status, 400, path);
    assert.deepEqual(res.body, { error: "invalid-path" });
  }
});

test("resolve and exists are reads: no exclusive section, no commit", async () => {
  const w = world();
  const { paste } = await create(w, "readslg1", { kind: "html", uploadId: "upload-read", files: { "/": "" } });
  const storage = w.storageOf("PASTES", "readslg1");
  const commits = storage.commits;
  const exclusive = w.exclusive.count;
  await ok(paste(), resolvePath("readslg1", "/"));
  await ok(paste(), "/paste/exists?slug=readslg1");
  assert.equal(storage.commits, commits);
  assert.equal(w.exclusive.count, exclusive);
});

test("resolve is reachable by the op query", async () => {
  const w = world();
  const { paste } = await create(w, "opqslug1", { kind: "html", uploadId: "upload-op", files: { "/": "" } });
  const res = await call(paste(), `/do/Paste:opqslug1?op=resolve&path=${encodeURIComponent("/")}`);
  assert.equal(res.body.action, "html");
});
