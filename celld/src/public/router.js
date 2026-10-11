// The router for public hostnames (docs/SPEC.md "Public and internal hosts",
// "The public surface"). It never passes a client request to a cell or the
// payload bucket: any call it makes is built from values it parsed.

import { serveRooms } from "../rooms/routes.js";
import { byteStringBytes, goText } from "../rooms/wire.js";
import { isAssetPath, serveAsset } from "./assets.js";
import { serveHealth } from "./health.js";
import { FORWARDED_HOST, normalizeHost, parseSlug, slugOfHost } from "./host.js";
import { cleanRedirect, decodePath, subtreeRedirect } from "./path.js";
import { queryFlags } from "./request.js";

// Go's http.NotFound, so a 404 reads the same from hostthisd and the Worker.
export function notFound() {
  return new Response("404 page not found\n", {
    status: 404,
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

// The answer Go's server gives a request line it cannot parse.
function badRequest() {
  return new Response("400 Bad Request", {
    status: 400,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
}

// The app a request addresses, { slug, path } with path decoded, or null.
function appOf(host, apex, urlMode, path) {
  const slug = slugOfHost(host, apex);
  if (slug) {
    return { slug, path };
  }
  if (urlMode !== "path" || host !== apex || !path.startsWith("/p/")) {
    return null;
  }
  const after = path.slice(3);
  const slash = after.indexOf("/");
  const pathSlug = parseSlug(slash < 0 ? after : after.slice(0, slash));
  return pathSlug && { slug: pathSlug, path: `/${slash < 0 ? "" : after.slice(slash + 1)}` };
}

const CLIENT_IP_SOURCES = ["cf-connecting-ip", "xff-rightmost"];
let clientIPSourceWarned = false;

// A direct request's client address comes from HOSTTHIS_CLIENT_IP_SOURCE;
// without it the room creation limit puts every client in one bucket. Logged
// once per isolate, never refused.
function warnClientIPSource(env) {
  if (clientIPSourceWarned || CLIENT_IP_SOURCES.includes(env.HOSTTHIS_CLIENT_IP_SOURCE)) {
    return;
  }
  clientIPSourceWarned = true;
  console.warn(`warn: HOSTTHIS_CLIENT_IP_SOURCE is ${JSON.stringify(env.HOSTTHIS_CLIENT_IP_SOURCE ?? null)}, `
    + `not one of ${CLIENT_IP_SOURCES.join(", ")}: every direct client shares one room creation bucket`);
}

// host is the normalized public host: the request's own, or the forwarded one.
// surface.reads serves an app's read path (src/public/surface.js), and
// surface.ctx is the request's execution context.
export async function publicRouter(request, env, host, forwarded = false, surface = {}) {
  if (!forwarded) {
    warnClientIPSource(env);
  }
  const res = await route(request, env, host, forwarded, surface);
  // As Go's server does, a HEAD gets the GET's status and headers, no body.
  if (request.method !== "HEAD" || res.body === null || res.status < 200 || res.webSocket) {
    return res;
  }
  await res.body.cancel();
  return new Response(null, { status: res.status, statusText: res.statusText, headers: res.headers });
}

async function route(request, env, host, forwarded, surface) {
  const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
  const pathMode = env.HOSTTHIS_URL_MODE === "path";
  const url = new URL(request.url);
  const path = decodePath(url.pathname);
  if (path === null) {
    return badRequest();
  }
  const redirect = subtreeRedirect(request.method, url.pathname, url.search)
    ?? cleanRedirect(request.method, url.pathname, url.search);
  if (redirect) {
    return redirect;
  }
  // As in hostthisd, the assets and health answer on every host.
  if (isAssetPath(url.pathname)) {
    return serveAsset(request, env, path);
  }
  const health = serveHealth(path);
  if (health) {
    return health;
  }
  const app = appOf(host, apex, pathMode ? "path" : "subdomain", path);
  if (!app) {
    return notFound();
  }
  const rawHost = forwarded ? request.headers.get(FORWARDED_HOST).trim() : url.host;
  const rooms = await serveRooms(request, env, app.slug, app.path, { forwarded, host: rawHost });
  if (rooms) {
    return rooms;
  }
  if (!surface.reads) {
    return notFound();
  }
  return surface.reads(request, env, surface.ctx, {
    slug: app.slug,
    path: goText(byteStringBytes(app.path)),
    host,
    flags: queryFlags(url.search.slice(1)),
  });
}
