// The Worker entry: the host-picked routers, and the cell classes the runtime
// binds by name (wrangler.jsonc durable_objects).
import { abuseRoute } from "./abuse-routes.js";
import { countingPageReads } from "./activity.js";
import { bearerIs, credentialScope, scopePermits } from "./auth.js";
import { BLOB_PATH, handleBlob } from "./blobs.js";
import { DENYLIST_CELL, denylistPoll } from "./cells/denylist.js";
import { TURN_CELL } from "./cells/turn.js";
import { FORWARDED_HOST, isHostname, normalizeHost, withinApex } from "./public/host.js";
import { CONTENT_CACHE_CONTROL } from "./public/reads.js";
import { publicRouter } from "./public/router.js";
import { publicReads } from "./public/surface.js";

export { Abuse } from "./cells/abuse.js";
export { Denylist } from "./cells/denylist.js";
export { Feed } from "./cells/feed.js";
export { Identity } from "./cells/identity.js";
export { IntentLog } from "./cells/intentlog.js";
export { Paste } from "./cells/paste.js";
export { Room } from "./cells/room.js";
export { Subnet } from "./cells/subnet.js";
export { Turn } from "./cells/turn.js";

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

const UNAUTHORIZED = () => new Response("unauthorized\n", {
  status: 401,
  headers: { "WWW-Authenticate": "Bearer" },
});

const internalRouter = {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return new Response("ok\n");
    }
    const scope = await credentialScope(request, env);
    // The watcher's token opens these routes and nothing else.
    if (url.pathname.startsWith("/watcher/")) {
      if (scope !== "full" && !(await bearerIs(request, env.HOSTTHIS_WATCHER_TOKEN))) {
        return UNAUTHORIZED();
      }
      return abuseRoute(request, env, ctx, url, "watcher");
    }
    // One answer for a missing token, a wrong one, the full token from an
    // address off its list, and a narrower token outside its operations.
    if (!scopePermits(scope, request.method, url)) {
      return UNAUTHORIZED();
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
    if (url.pathname.startsWith("/abuse/")) {
      return abuseRoute(request, env, ctx, url, "operator");
    }
    if (url.pathname.startsWith("/turn/")) {
      // One cell issues every credential, so its caps and budget are global.
      return env.TURN.get(env.TURN.idFromName(TURN_CELL)).fetch(request);
    }
    if (url.pathname.startsWith("/denylist/")) {
      // hostthisd's poll reads the list's snapshot; every other operation
      // goes to the one cell that holds the list.
      if (request.method === "GET" && url.pathname === "/denylist/list") {
        return denylistPoll(env);
      }
      return env.DENYLIST.get(env.DENYLIST.idFromName(DENYLIST_CELL)).fetch(request);
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
    const owner = url.searchParams.get("scope");
    if (!owner) {
      return new Response("scope required\n", { status: 400 });
    }
    // One owner maps deterministically to one cell holding intents, quota, and index.
    return env.IDENTITY.get(env.IDENTITY.idFromName(owner)).fetch(request);
  },
};

function forwardPermitted(scope, method) {
  return scope === "full" || ((scope === "admin" || scope === "read") && (method === "GET" || method === "HEAD"));
}

// The host picks the router before any path is read (docs/SPEC.md "Public and
// internal hosts"). Without an apex the internal router answers everything.
export default {
  async fetch(request, env, ctx) {
    const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
    if (!apex) {
      return internalRouter.fetch(request, env, ctx);
    }
    const host = normalizeHost(new URL(request.url).hostname);
    if (withinApex(host, apex)) {
      // Direct reads only: hostthisd answers a forwarded read's reader itself.
      return publicRouter(request, env, host, false, { ctx, reads: countingPageReads(publicReads(env), CONTENT_CACHE_CONTROL) });
    }
    if (!env.HOSTTHIS_WORKER_TOKEN) {
      return new Response("internal routes need HOSTTHIS_WORKER_TOKEN\n", { status: 503 });
    }
    // A forwarded request is served as the public host it names and trusts the
    // client address it carries, so only the full token may send one; the
    // narrower tokens may forward reads.
    const forwarded = request.headers.get(FORWARDED_HOST);
    if (forwarded !== null && forwardPermitted(await credentialScope(request, env), request.method)) {
      const target = normalizeHost(forwarded);
      if (!isHostname(target) || !withinApex(target, apex)) {
        return new Response("forwarded host outside the apex\n", { status: 400 });
      }
      return publicRouter(request, env, target, true, { ctx, reads: publicReads(env) });
    }
    return internalRouter.fetch(request, env, ctx);
  },
};
