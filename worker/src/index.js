// The Worker entry: the host-picked routers, and the cell classes the runtime
// binds by name (wrangler.jsonc durable_objects).
import { BLOB_PATH, handleBlob } from "./blobs.js";
import { FORWARDED_HOST, isHostname, normalizeHost, withinApex } from "./public/host.js";
import { publicRouter } from "./public/router.js";
import { publicReads } from "./public/surface.js";

export { Denylist } from "./cells/denylist.js";
export { Identity } from "./cells/identity.js";
export { IntentLog } from "./cells/intentlog.js";
export { Paste } from "./cells/paste.js";
export { Room } from "./cells/room.js";
export { Subnet } from "./cells/subnet.js";

const BEARER = /^bearer +/i;

// HOSTTHIS_WORKER_TOKEN, when set, is the bearer token every route but /healthz
// requires. Both sides are hashed first, so the compare runs over two equal
// length digests and its timing says nothing about the token.
async function authorized(request, env) {
  const token = env.HOSTTHIS_WORKER_TOKEN;
  if (!token) {
    return true;
  }
  const header = request.headers.get("Authorization") ?? "";
  const scheme = BEARER.exec(header);
  if (!scheme) {
    return false;
  }
  const [want, got] = await Promise.all([token, header.slice(scheme[0].length)]
    .map(async (text) => new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)))));
  let diff = 0;
  for (let i = 0; i < want.length; i++) {
    diff |= want[i] ^ got[i];
  }
  return diff === 0;
}

// Cell-internal paths are reachable only through a class binding, so the
// router refuses any path naming one, however it is spelled.
function namesCellInternal(pathname) {
  return pathname.split("/").some((seg) => {
    try {
      return decodeURIComponent(seg) === "__cell";
    } catch {
      return false;
    }
  });
}

const internalRouter = {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return new Response("ok\n");
    }
    if (!(await authorized(request, env))) {
      return new Response("unauthorized\n", {
        status: 401,
        headers: { "WWW-Authenticate": "Bearer" },
      });
    }
    if (namesCellInternal(url.pathname)) {
      return new Response("not found\n", { status: 404 });
    }
    if (url.pathname === "/blob" || url.pathname.startsWith(BLOB_PATH)) {
      return handleBlob(request, env, url);
    }
    if (url.pathname.startsWith("/room/")) {
      const room = url.searchParams.get("room");
      if (!room) {
        return new Response("room required\n", { status: 400 });
      }
      return env.ROOMS.get(env.ROOMS.idFromName(room)).fetch(request);
    }
    if (url.pathname.startsWith("/subnet/")) {
      const subnet = url.searchParams.get("subnet");
      if (!subnet) {
        return new Response("subnet required\n", { status: 400 });
      }
      return env.SUBNETS.get(env.SUBNETS.idFromName(subnet)).fetch(request);
    }
    if (url.pathname.startsWith("/denylist/")) {
      // One cell holds the whole list.
      return env.DENYLIST.get(env.DENYLIST.idFromName("denylist")).fetch(request);
    }
    if (url.pathname.startsWith("/paste/")) {
      const slug = url.searchParams.get("slug");
      if (!slug) {
        return new Response("slug required\n", { status: 400 });
      }
      return env.PASTES.get(env.PASTES.idFromName(slug)).fetch(request);
    }
    if (!url.pathname.startsWith("/identity/")) {
      return new Response("not found\n", { status: 404 });
    }
    const scope = url.searchParams.get("scope");
    if (!scope) {
      return new Response("scope required\n", { status: 400 });
    }
    // One owner maps deterministically to one cell holding intents, quota, and index.
    return env.IDENTITY.get(env.IDENTITY.idFromName(scope)).fetch(request);
  },
};

// The host picks the router before any path is read (docs/SPEC.md "Public and
// internal hosts"). Without an apex the internal router answers everything.
export default {
  async fetch(request, env, ctx) {
    const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
    if (!apex) {
      return internalRouter.fetch(request, env);
    }
    const host = normalizeHost(new URL(request.url).hostname);
    if (withinApex(host, apex)) {
      return publicRouter(request, env, host, false, { ctx, reads: publicReads(env) });
    }
    if (!env.HOSTTHIS_WORKER_TOKEN) {
      return new Response("internal routes need HOSTTHIS_WORKER_TOKEN\n", { status: 503 });
    }
    const forwarded = request.headers.get(FORWARDED_HOST);
    if (forwarded !== null && (await authorized(request, env))) {
      const target = normalizeHost(forwarded);
      if (!isHostname(target) || !withinApex(target, apex)) {
        return new Response("forwarded host outside the apex\n", { status: 400 });
      }
      return publicRouter(request, env, target, true, { ctx, reads: publicReads(env) });
    }
    return internalRouter.fetch(request, env);
  },
};
