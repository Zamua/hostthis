// The read path's caches, as decorators around it (docs/SPEC.md "The public
// surface"): whole responses keyed by URL, and decoded payloads keyed by
// object key. Each takes the runtime's Cache API as a port.
import { CONTENT_CACHE_CONTROL } from "./reads.js";

const IMMUTABLE = "public, max-age=31536000, immutable";
const CONDITIONALS = ["If-None-Match", "If-Modified-Since"];

// Two copies of source that read it in step: a chunk is read only once every
// live copy has taken the last one, so neither copy holds more than one chunk
// however slowly the other is read. A cancelled copy drops out; the source is
// cancelled with the last one. release(i) drops copy i the same way, for a
// consumer that stops reading without cancelling it (a failed cache put).
export function lockstepTee(source) {
  const reader = source.getReader();
  const copies = [];
  let reading = false;
  const live = () => copies.filter((c) => !c.cancelled);

  async function step() {
    const waiting = live();
    if (reading || waiting.length === 0 || !waiting.every((c) => c.waiter)) {
      return;
    }
    reading = true;
    let next;
    let failure;
    try {
      next = await reader.read();
    } catch (error) {
      failure = error;
    }
    reading = false;
    copies.forEach((c, i) => {
      if (c.cancelled || !c.waiter) {
        return;
      }
      const wake = c.waiter;
      c.waiter = null;
      if (failure) {
        c.controller.error(failure);
      } else if (next.done) {
        c.controller.close();
      } else {
        // Each copy gets its own buffer: a consumer may transfer what it reads.
        c.controller.enqueue(i === 0 ? next.value : next.value.slice());
      }
      wake();
    });
  }

  function release(i, reason) {
    const c = copies[i];
    if (c.cancelled) {
      return Promise.resolve();
    }
    c.cancelled = true;
    c.waiter?.();
    c.waiter = null;
    return live().length === 0 ? reader.cancel(reason).catch(() => {}) : step();
  }

  const copy = (i) => new ReadableStream({
    start(controller) {
      copies[i] = { controller, waiter: null, cancelled: false };
    },
    pull() {
      return new Promise((resolve) => {
        copies[i].waiter = resolve;
        step();
      });
    },
    cancel: (reason) => release(i, reason),
  }, { highWaterMark: 0 });
  return { copies: [copy(0), copy(1)], release };
}

// Stores the tee's second copy under key in the background. However the put
// ends, the copy is released, so a put that stops reading never stalls the
// client.
function keep(ctx, cache, key, tee, init) {
  const put = cache.put(key, new Response(tee.copies[1], init))
    .catch(() => {})
    .finally(() => tee.release(1));
  ctx?.waitUntil?.(put);
}

// The key a response is stored under: the request's URL on the public host,
// so a hostname purge of <slug>.<apex> reaches it.
function responseKey(request, host) {
  const url = new URL(request.url);
  url.hostname = host;
  url.port = "";
  return url.toString();
}

const NULL_BODY_STATUSES = new Set([204, 205, 304]);

// What the client gets from a stored response: the browser's cache posture,
// not the edge's. A 304 or a HEAD gets no body, whatever the cache handed back.
function forBrowser(stored, method) {
  const headers = new Headers(stored.headers);
  headers.set("Cache-Control", CONTENT_CACHE_CONTROL);
  headers.delete("Cache-Tag");
  let body = stored.body;
  if (method === "HEAD" || NULL_BODY_STATUSES.has(stored.status)) {
    body?.cancel().catch(() => {});
    body = null;
  }
  return new Response(body, { status: stored.status, headers });
}

// Whole read responses for ttl seconds. A hit answers GET, HEAD and
// conditional requests (the cache answers the conditionals itself). Only an
// unconditional GET fills it, and only with a 200 carrying the content
// posture: never an error, a 404, or a pending or failed page.
export function withEdgeCache(read, { cache, ttl }) {
  return async function cachedRead(request, env, ctx, target) {
    const { method } = request;
    if (method !== "GET" && method !== "HEAD") {
      return read(request, env, ctx, target);
    }
    const key = responseKey(request, target.host);
    const conditional = {};
    for (const name of CONDITIONALS) {
      const value = request.headers.get(name);
      if (value !== null) conditional[name] = value;
    }
    // A cache that fails is a miss: the read path still answers.
    const hit = await cache.match(new Request(key, { headers: conditional })).catch(() => undefined);
    if (hit) {
      return forBrowser(hit, method);
    }
    const res = await read(request, env, ctx, target);
    const fills = method === "GET" && Object.keys(conditional).length === 0;
    if (!fills || res.status !== 200 || res.headers.get("Cache-Control") !== CONTENT_CACHE_CONTROL || !res.body) {
      return res;
    }
    const tee = lockstepTee(res.body);
    const headers = new Headers(res.headers);
    headers.set("Cache-Control", `public, max-age=${ttl}`);
    headers.set("Cache-Tag", target.slug);
    keep(ctx, cache, key, tee, { status: 200, headers });
    return new Response(tee.copies[0], { status: res.status, headers: res.headers });
  };
}

// Decoded payloads of at most maxBytes, by object key. A key never names
// different bytes, so an entry needs no invalidation. origin is a URL prefix
// no read path is keyed under.
export function withBlobCache(open, { cache, maxBytes, origin }) {
  return async function cachedOpen(env, entry, ctx) {
    if (!(Number.isSafeInteger(entry.size) && entry.size >= 0 && entry.size <= maxBytes)) {
      return open(env, entry, ctx);
    }
    const key = `${origin}/${encodeURIComponent(entry.key)}`;
    const hit = await cache.match(key).catch(() => undefined);
    if (hit?.status === 200) {
      return hit.body ?? new ReadableStream({ start: (c) => c.close() });
    }
    const tee = lockstepTee(await open(env, entry, ctx));
    keep(ctx, cache, key, tee, { headers: { "Cache-Control": IMMUTABLE } });
    return tee.copies[0];
  };
}
