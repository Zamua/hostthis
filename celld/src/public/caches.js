// The read path's caches, as decorators around it (docs/SPEC.md "The public
// surface"): whole responses keyed by what the read path answers from, and
// decoded payloads keyed by object key. Each takes the runtime's Cache API as
// a port.
import { statedLength } from "./length.js";
import { CONTENT_CACHE_CONTROL } from "./reads.js";

const IMMUTABLE = "public, max-age=31536000, immutable";
const CONDITIONALS = ["If-None-Match", "If-Modified-Since"];

// The largest response the edge cache stores when the deployment names none.
export const EDGE_CACHE_MAX_BYTES = 8 * 1024 * 1024;

// Two copies of source that read it in step: a chunk is read only once every
// live copy has taken the last one, so neither copy holds more than one chunk
// however slowly the other is read. The first copy leads: cancelling it
// cancels the source and errors the second, so a copy kept for a cache never
// outlives its reader nor ends as if whole. Cancelling the second, or
// release(1) for a consumer that stops reading without cancelling it (a
// failed cache put), leaves the first to read alone.
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

  function drop(c, error) {
    c.cancelled = true;
    if (error) {
      try {
        c.controller.error(error);
      } catch {
        // already closed or errored
      }
    }
    c.waiter?.();
    c.waiter = null;
  }

  function release(i, reason) {
    const c = copies[i];
    if (c.cancelled) {
      return Promise.resolve();
    }
    drop(c);
    if (i === 0) {
      if (!copies[1].cancelled) {
        drop(copies[1], new Error("tee: the leading copy was cancelled"));
      }
      return reader.cancel(reason).catch(() => {});
    }
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

// An edge fill must end within this long of the read that began it: hostthisd
// purges a slug again 15 s after every purge, and that second purge has to
// find any entry a read begun before the write stored.
export const EDGE_FILL_WINDOW_MS = 10_000;

// copy as a cache put's body, erroring (so the put stores nothing) once it
// runs past maxBytes, or when a chunk or its end arrives after deadline (ms,
// by now).
function bounded(copy, { maxBytes, deadline = Infinity, now = Date.now }) {
  const reader = copy.getReader();
  let seen = 0;
  return new ReadableStream({
    async pull(controller) {
      const next = await reader.read();
      seen += next.done ? 0 : next.value.byteLength;
      const late = now() > deadline;
      if (seen > maxBytes || late) {
        reader.cancel().catch(() => {});
        throw new Error(late ? "cache fill: past its deadline" : `cache fill: past ${maxBytes} bytes`);
      }
      if (next.done) {
        controller.close();
      } else {
        controller.enqueue(next.value);
      }
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });
}

// Stores the tee's second copy under key in the background, within limits
// (bounded's). However the put ends, the copy is released, so a put that
// stops reading never stalls the client.
function keep(ctx, cache, key, tee, init, limits) {
  const put = cache.put(key, new Response(bounded(tee.copies[1], limits), init))
    .catch(() => {})
    .finally(() => tee.release(1));
  ctx?.waitUntil?.(put);
}

// The key a response is stored under. Its host is the public host, so a
// hostname purge of <slug>.<apex> reaches it (and in path mode, through
// mount, a prefix purge of <apex>/p/<slug>). The rest names the build, so a
// deploy never serves another build's entries, and what the read path answers
// from: the decoded path and the parsed flags, never the client's query.
function responseKey(target, build) {
  const params = new URLSearchParams({ build, path: target.path });
  if (target.flags.raw) params.set("raw", "1");
  if (target.flags.files) params.set("files", "1");
  return `https://${target.host}${target.mount}/?${params}`;
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
// posture and at most maxBytes: never an error, a 404, or a pending or failed
// page. A fill that would end more than EDGE_FILL_WINDOW_MS after its read
// began (by now, in ms) is abandoned. build names the code and shells serving
// the reads.
export function withEdgeCache(read, { cache, ttl, build, maxBytes = EDGE_CACHE_MAX_BYTES, now = Date.now }) {
  return async function cachedRead(request, env, ctx, target) {
    const { method } = request;
    if (method !== "GET" && method !== "HEAD") {
      return read(request, env, ctx, target);
    }
    const key = responseKey(target, build);
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
    // Taken before the read's cell call, so the window never starts late.
    const deadline = now() + EDGE_FILL_WINDOW_MS;
    const res = await read(request, env, ctx, target);
    const fills = method === "GET" && Object.keys(conditional).length === 0;
    if (!fills || res.status !== 200 || res.headers.get("Cache-Control") !== CONTENT_CACHE_CONTROL || !res.body) {
      return res;
    }
    // A stated size over the cap starts no fill; an unstated one is cut off
    // at the cap.
    if ((statedLength(res.headers) ?? 0) > maxBytes) {
      return res;
    }
    const tee = lockstepTee(res.body);
    const headers = new Headers(res.headers);
    headers.set("Cache-Control", `public, max-age=${ttl}`);
    headers.set("Cache-Tag", target.slug);
    keep(ctx, cache, key, tee, { status: 200, headers }, { maxBytes, deadline, now });
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
    keep(ctx, cache, key, tee, { headers: { "Cache-Control": IMMUTABLE } }, { maxBytes });
    return tee.copies[0];
  };
}
