// The router for public hostnames (docs/SPEC.md "Public and internal hosts").
// It never passes a client request to a cell or the payload bucket: any call
// it makes is built from values it parsed.

import { serveRooms } from "../rooms/routes.js";
import { FORWARDED_HOST, normalizeHost, parseSlug, slugOfHost } from "./host.js";
import { cleanRedirect, decodePath } from "./path.js";

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

// host is the normalized public host: the request's own, or the forwarded one.
export async function publicRouter(request, env, host, forwarded = false) {
  const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
  const pathMode = env.HOSTTHIS_URL_MODE === "path";
  if (!slugOfHost(host, apex) && !(pathMode && host === apex)) {
    return notFound();
  }
  const url = new URL(request.url);
  const path = decodePath(url.pathname);
  if (path === null) {
    return badRequest();
  }
  const redirect = cleanRedirect(request.method, url.pathname, url.search);
  if (redirect) {
    return redirect;
  }
  const app = appOf(host, apex, pathMode ? "path" : "subdomain", path);
  if (!app) {
    return notFound();
  }
  const rawHost = forwarded ? request.headers.get(FORWARDED_HOST).trim() : url.host;
  return (await serveRooms(request, env, app.slug, app.path, { forwarded, host: rawHost })) ?? notFound();
}
