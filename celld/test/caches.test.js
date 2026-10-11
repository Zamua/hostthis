import assert from "node:assert/strict";
import test from "node:test";

import { lockstepTee, withEdgeCache } from "../src/public/caches.js";
import { publicReads } from "../src/public/surface.js";
import { decoderStats } from "../src/public/zstd.js";
import { APEX, at, documentRow, FORWARDED, headersOf, hiRow, SLUG, TOKEN, world } from "./public-world.js";
import { stored } from "./stored.js";

// A Cache API double: put reads the whole body (and fails when it errors), and
// match answers a conditional request with a 304 by RFC 9110's rules.
class FakeCache {
  constructor() {
    this.entries = new Map();
    this.calls = [];
  }

  async match(request) {
    const req = typeof request === "string" ? new Request(request) : request;
    this.calls.push(`match ${req.url}`);
    const entry = this.entries.get(req.url);
    if (!entry) return undefined;
    const headers = new Headers(entry.headers);
    const tag = (t) => t.trim().replace(/^W\//, "");
    const inm = req.headers.get("If-None-Match");
    const ims = req.headers.get("If-Modified-Since");
    const unchanged = inm !== null
      ? inm.split(",").some((t) => tag(t) === tag(headers.get("ETag") ?? ""))
      : ims !== null && Date.parse(headers.get("Last-Modified")) <= Date.parse(ims);
    if (unchanged) return new Response(null, { status: 304, headers });
    return new Response(entry.body, { status: entry.status, headers });
  }

  async put(request, response) {
    const url = typeof request === "string" ? request : request.url;
    this.calls.push(`put ${url}`);
    const body = new Uint8Array(await response.arrayBuffer());
    this.entries.set(url, { status: response.status, headers: [...response.headers], body });
  }
}

// Runs fn with a global caches whose default is cache, as the runtime has.
async function withCaches(cache, fn) {
  globalThis.caches = { default: cache };
  try {
    return await fn();
  } finally {
    delete globalThis.caches;
  }
}

async function settle(w) {
  await Promise.all(w.waits);
  w.waits.length = 0;
}

function htmlWorld(vars) {
  const w = world(vars);
  w.row(SLUG, hiRow());
  w.blob("uploads/a/0", "<h1>hi</h1>");
  return w;
}

const TTL = { HOSTTHIS_EDGE_CACHE_TTL_S: "86400" };

test("off unless configured, and off without a Cache API", async () => {
  const cache = new FakeCache();
  const w = htmlWorld();
  await withCaches(cache, async () => {
    for (let i = 0; i < 2; i++) assert.equal(await (await w.fetch(at("/"))).text(), "<h1>hi</h1>");
  });
  assert.deepEqual(cache.calls, []);
  assert.equal(w.cellRequests.length, 2);
  // Configured, but the runtime has no caches global.
  const bare = htmlWorld({ ...TTL, HOSTTHIS_BLOB_CACHE_MAX_BYTES: "1000000" });
  for (let i = 0; i < 2; i++) assert.equal(await (await bare.fetch(at("/"))).text(), "<h1>hi</h1>");
  assert.equal(bare.cellRequests.length, 2);
  for (const value of ["", "0", "-5", "1.5", "x"]) {
    let read = publicReads({ HOSTTHIS_EDGE_CACHE_TTL_S: value }, { default: cache });
    assert.equal(read.name, "read", value);
    read = publicReads({ HOSTTHIS_EDGE_CACHE_TTL_S: "60" }, { default: cache });
    assert.equal(read.name, "cachedRead");
  }
});

test("a GET fills the edge cache, and a hit answers GET, HEAD and conditionals alone", async () => {
  const cache = new FakeCache();
  const w = htmlWorld(TTL);
  await withCaches(cache, async () => {
    const miss = await w.fetch(at("/?raw"));
    assert.equal(miss.headers.get("Cache-Control"), "public, no-cache");
    assert.equal(miss.headers.get("Cache-Tag"), null);
    const missHeaders = headersOf(miss);
    assert.equal(await miss.text(), "<h1>hi</h1>");
    await settle(w);

    assert.equal(cache.entries.size, 1);
    const [[key, entry]] = cache.entries;
    assert.equal(new URL(key).hostname, `${SLUG}.paste.test`, "a hostname purge reaches it");
    const kept = new Headers(entry.headers);
    assert.equal(kept.get("Cache-Control"), "public, max-age=86400");
    assert.equal(kept.get("Cache-Tag"), SLUG);
    assert.equal(new TextDecoder().decode(entry.body), "<h1>hi</h1>");

    const calls = [w.cellRequests.length, w.payloadCalls.length];
    const hit = await w.fetch(at("/?raw"));
    assert.equal(hit.status, 200);
    assert.deepEqual(headersOf(hit), missHeaders);
    assert.equal(await hit.text(), "<h1>hi</h1>");
    const head = await w.fetch(at("/?raw"), { method: "HEAD" });
    assert.equal(head.status, 200);
    assert.equal(await head.text(), "");
    const conditional = await w.fetch(at("/?raw"), { headers: { "If-None-Match": "\"uploads/a/0\"" } });
    assert.equal(conditional.status, 304);
    assert.equal(conditional.headers.get("Cache-Control"), "public, no-cache");
    assert.equal(conditional.headers.get("Cache-Tag"), null);
    assert.deepEqual([w.cellRequests.length, w.payloadCalls.length], calls, "a hit makes no cell call or payload read");
  });
});

test("a stored 304, and a hit for a HEAD, reach the client without a body", async () => {
  const headers = { "ETag": "\"k\"", "Cache-Control": "public, max-age=60", "Cache-Tag": SLUG };
  const bodies = [];
  // A runtime may hand back a 304 that still carries an (empty) body stream.
  const cache = {
    async match(request) {
      const body = new ReadableStream({ cancel: () => { body.cancelled = true; } });
      bodies.push(body);
      return { status: request.headers.has("If-None-Match") ? 304 : 200, headers: new Headers(headers), body };
    },
    put: async () => {},
  };
  const read = withEdgeCache(() => assert.fail("a hit reached the read path"), { cache, ttl: 60, build: "b" });
  const target = { slug: SLUG, path: "/", host: `${SLUG}.paste.test`, mount: "", flags: { raw: false, files: false } };
  const revalidated = await read(new Request(at("/"), { headers: { "If-None-Match": "\"k\"" } }), {}, {}, target);
  assert.equal(revalidated.status, 304);
  assert.equal(revalidated.body, null);
  assert.equal(revalidated.headers.get("Cache-Control"), "public, no-cache");
  const head = await read(new Request(at("/"), { method: "HEAD" }), {}, {}, target);
  assert.equal(head.status, 200);
  assert.equal(head.body, null);
  assert.equal(head.headers.get("Cache-Tag"), null);
  assert.ok(bodies.every((b) => b.cancelled), "a body the client never gets is cancelled");
});

test("a conditional request or a HEAD that misses fills nothing", async () => {
  const cache = new FakeCache();
  const w = htmlWorld(TTL);
  await withCaches(cache, async () => {
    assert.equal((await w.fetch(at("/"), { headers: { "If-None-Match": "\"uploads/a/0\"" } })).status, 304);
    assert.equal((await w.fetch(at("/"), { method: "HEAD" })).status, 200);
    await settle(w);
  });
  assert.equal(cache.entries.size, 0);
  assert.deepEqual(w.payloadCalls, []);
});

test("errors, misses and lifecycle pages are never stored", async () => {
  const cache = new FakeCache();
  await withCaches(cache, async () => {
    const empty = world(TTL);
    assert.equal((await empty.fetch(at("/"))).status, 404);
    for (const status of ["pending", "failed"]) {
      const w = world(TTL);
      w.row(SLUG, documentRow("markdown", status));
      assert.ok([200, 410].includes((await w.fetch(at("/"))).status));
    }
    const missing = world(TTL);
    missing.row(SLUG, hiRow());
    const warn = console.warn;
    console.warn = () => {};
    try {
      assert.equal((await missing.fetch(at("/"))).status, 500);
    } finally {
      console.warn = warn;
    }
    assert.equal((await missing.fetch(at("/"), { method: "POST", body: "x" })).status, 405);
  });
  assert.equal(cache.entries.size, 0);
  assert.ok(!cache.calls.some((c) => c.startsWith("put")));
});

test("a forwarded read is stored under the public host, where a direct read finds it", async () => {
  const cache = new FakeCache();
  const w = htmlWorld(TTL);
  await withCaches(cache, async () => {
    const res = await w.fetch("http://celld.internal/", {
      headers: { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${SLUG}.paste.test` },
    });
    assert.equal(await res.text(), "<h1>hi</h1>");
    await settle(w);
    assert.equal(await (await w.fetch(at("/"))).text(), "<h1>hi</h1>");
  });
  assert.deepEqual([...cache.entries.keys()].map((k) => new URL(k).hostname), [`${SLUG}.paste.test`]);
  assert.equal(w.cellRequests.length, 1);
});

test("the edge key is the slug, path and parsed flags: a client's query adds no entries", async () => {
  const cache = new FakeCache();
  const w = world({ ...TTL, HOSTTHIS_URL_MODE: "path" });
  w.row(SLUG, documentRow("markdown", "ready", 3));
  w.blob("uploads/a/0", "# x");
  w.row("xyz23456", documentRow("markdown", "ready", 3));
  await withCaches(cache, async () => {
    const shell = ["/", "/?utm=1", "/?x=1&y", "/?RAW", "/?raw;x", `/?${crypto.randomUUID()}`];
    const raw = ["/?raw", "/?raw=0&z=9", "/?r%61w", `/?${crypto.randomUUID()}&raw`];
    for (const query of [...shell, ...raw]) {
      await (await w.fetch(at(query))).arrayBuffer();
      await settle(w);
    }
    assert.equal(cache.entries.size, 2, [...cache.entries.keys()].join("\n"));
    assert.equal(w.cellRequests.length, 2, "every request after the first of each key is a hit");
    // Path mode keys each slug apart under the one apex host.
    await (await w.fetch(`http://${APEX}/p/${SLUG}/`)).arrayBuffer();
    await (await w.fetch(`http://${APEX}/p/xyz23456/`)).arrayBuffer();
    await settle(w);
    assert.equal(cache.entries.size, 4);
  });
});

test("a client that stops reading stops the edge fill, the decoder and the payload read", async () => {
  for (const vars of [TTL, { ...TTL, HOSTTHIS_BLOB_CACHE_MAX_BYTES: String(8 * 1024 * 1024) }]) {
    const cache = new FakeCache();
    const w = world(vars);
    const content = new Uint8Array(3 * 1024 * 1024).fill(0x62);
    w.row(SLUG, documentRow("html", "ready", content.length));
    w.blob("uploads/a/0", content);
    await withCaches(cache, async () => {
      const reader = (await w.fetch(at("/"))).body.getReader();
      assert.ok((await reader.read()).value.length > 0);
      await reader.cancel();
      await settle(w);
    });
    assert.equal(cache.entries.size, 0, "a partial body is never stored");
    assert.equal(decoderStats().busy, 0, "the decoder went back");
    assert.equal(w.payloadBodies.length, 1);
    const [body] = w.payloadBodies;
    assert.ok(body.cancelled, "the payload body was cancelled");
    assert.ok(body.sent < 1024 * 1024, `${body.sent} payload bytes read`);
  }
});

const putHosts = (cache) => cache.calls.filter((c) => c.startsWith("put ")).map((c) => new URL(c.slice(4)).hostname);

test("an edge fill is capped by the response's size", async () => {
  const cache = new FakeCache();
  const w = world({ ...TTL, HOSTTHIS_EDGE_CACHE_MAX_BYTES: "11" });
  w.row(SLUG, hiRow());
  w.blob("uploads/a/0", "<h1>hi</h1>");
  w.row("xyz23456", { ...hiRow(), manifest: { Files: { "/": { Key: "uploads/b/0", Size: 12 } } } });
  w.blob("uploads/b/0", "<h1>hi!</h1>");
  // A shell states no size, and is far over the cap.
  w.row("mnp23456", documentRow("markdown"));
  await withCaches(cache, async () => {
    for (const slug of [SLUG, "xyz23456", "mnp23456"]) {
      assert.equal((await (await w.fetch(at("/", slug))).arrayBuffer()).byteLength > 0, true, slug);
      await settle(w);
    }
  });
  // A stated size over the cap starts no fill; an unstated one is abandoned past it.
  assert.deepEqual(putHosts(cache), [`${SLUG}.paste.test`, "mnp23456.paste.test"]);
  assert.deepEqual([...cache.entries.keys()].map((k) => new URL(k).hostname), [`${SLUG}.paste.test`]);
});

test("the edge fill cap defaults to 8 MiB", async () => {
  const cache = new FakeCache();
  const w = world(TTL);
  for (const [slug, size] of [[SLUG, 8 * 1024 * 1024], ["xyz23456", 8 * 1024 * 1024 + 1]]) {
    w.row(slug, { ...documentRow("html", "ready", size), manifest: { Files: { "/": { Key: `k-${slug}`, Size: size } } } });
    w.blob(`k-${slug}`, "short");
  }
  const warn = console.warn;
  console.warn = () => {};
  try {
    await withCaches(cache, async () => {
      for (const slug of [SLUG, "xyz23456"]) {
        await (await w.fetch(at("/", slug))).arrayBuffer().catch(() => {});
        await settle(w);
      }
    });
  } finally {
    console.warn = warn;
  }
  assert.deepEqual(putHosts(cache), [`${SLUG}.paste.test`]);
  assert.equal(cache.entries.size, 0, "a short body is never stored");
});

test("a cache that fails is a miss", async () => {
  const broken = { match: async () => { throw new Error("cache down"); }, put: async () => { throw new Error("cache down"); } };
  const w = htmlWorld({ ...TTL, HOSTTHIS_BLOB_CACHE_MAX_BYTES: "1000" });
  await withCaches(broken, async () => {
    assert.equal(await (await w.fetch(at("/"))).text(), "<h1>hi</h1>");
    await settle(w);
  });
});

test("the blob cache keeps decoded payloads under the size cap, by object key", async () => {
  const cache = new FakeCache();
  const w = world({ HOSTTHIS_BLOB_CACHE_MAX_BYTES: "100" });
  w.row(SLUG, {
    kind: "site", status: "ready", updatedAt: 1, uploadId: "us",
    manifest: { Files: {
      "index.html": { Key: "k-small", Size: 5 },
      "big.txt": { Key: "k-big", Size: 101 },
      "empty.txt": { Key: "k-empty", Size: 0 },
    } },
  });
  w.blob("k-small", "small");
  w.blob("k-big", "b".repeat(101));
  w.blob("k-empty", "");
  await withCaches(cache, async () => {
    for (const path of ["/", "/big.txt", "/empty.txt"]) await (await w.fetch(at(path))).text();
    await settle(w);
    assert.deepEqual([...cache.entries.keys()], ["https://paste.test/_blobs/k-small", "https://paste.test/_blobs/k-empty"]);
    assert.equal(new TextDecoder().decode(cache.entries.get("https://paste.test/_blobs/k-small").body), "small");
    w.payloadCalls.length = 0;
    assert.equal(await (await w.fetch(at("/"))).text(), "small");
    assert.equal(await (await w.fetch(at("/big.txt"))).text(), "b".repeat(101));
    assert.deepEqual(w.payloadCalls, ["get k-big"]);
  });
});

test("a payload that fails to decode is not kept", async () => {
  const cache = new FakeCache();
  const w = world({ HOSTTHIS_BLOB_CACHE_MAX_BYTES: "1000000", ...TTL });
  w.row(SLUG, documentRow("html", "ready", 300 * 1024));
  const whole = stored(new Uint8Array(300 * 1024).fill(0x61));
  w.object("uploads/a/0", whole.subarray(0, whole.length - 10));
  const warn = console.warn;
  console.warn = () => {};
  try {
    await withCaches(cache, async () => {
      const res = await w.fetch(at("/"));
      await assert.rejects(res.arrayBuffer());
      await settle(w);
    });
  } finally {
    console.warn = warn;
  }
  assert.equal(cache.entries.size, 0);
});

function chunks(n, onCancel) {
  let i = 0;
  const stats = { pulled: 0 };
  const stream = new ReadableStream({
    pull(controller) {
      if (i === n) {
        controller.close();
        return;
      }
      stats.pulled = ++i;
      controller.enqueue(Uint8Array.of(i));
    },
    cancel: onCancel,
  }, { highWaterMark: 0 });
  return { stream, stats };
}

const pending = (promise) => Promise.race([promise.then(() => false), new Promise((r) => setTimeout(() => r(true), 20))]);

test("a lockstep copy runs at most one chunk ahead of the other", async () => {
  const { stream, stats } = chunks(5);
  const [a, b] = lockstepTee(stream).copies;
  const ra = a.getReader();
  const rb = b.getReader();
  assert.deepEqual([...(await Promise.all([ra.read(), rb.read()])).map((r) => r.value[0])], [1, 1]);
  const next = ra.read();
  assert.ok(await pending(next), "a ran ahead of b");
  assert.equal(stats.pulled, 1);
  assert.equal((await rb.read()).value[0], 2);
  assert.equal((await next).value[0], 2);
  // A copy's chunk is its own.
  const [x, y] = await Promise.all([ra.read(), rb.read()]);
  x.value[0] = 99;
  assert.equal(y.value[0], 3);
  // Cancelling the second leaves the first to read alone.
  await rb.cancel();
  assert.equal((await ra.read()).value[0], 4);
  assert.equal((await ra.read()).value[0], 5);
  assert.equal((await ra.read()).done, true);
});

test("cancelling the first copy cancels the source and errors the second", async () => {
  let cancelled = false;
  const [c, d] = lockstepTee(chunks(5, () => { cancelled = true; }).stream).copies;
  const rc = c.getReader();
  const rd = d.getReader();
  await Promise.all([rc.read(), rd.read()]);
  const waiting = rd.read();
  await rc.cancel();
  assert.ok(cancelled);
  await assert.rejects(waiting);
});

test("a released copy no longer holds the other back", async () => {
  const tee = lockstepTee(chunks(3).stream);
  const reader = tee.copies[0].getReader();
  const first = reader.read();
  assert.ok(await pending(first));
  await tee.release(1);
  assert.equal((await first).value[0], 1);
  assert.equal((await reader.read()).value[0], 2);
});

test("a source error reaches both copies", async () => {
  const failing = new ReadableStream({ pull: (c) => c.error(new Error("boom")) });
  const [a, b] = lockstepTee(failing).copies;
  await assert.rejects(Promise.all([a.getReader().read(), b.getReader().read()]), /boom/);
});
