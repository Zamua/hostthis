// /_hostthis/<name>, the renderer assets the shells load. On Cloudflare the
// static assets layer answers a known name before the Worker runs; what
// reaches here is an unknown name, a name under a deeper path, or an asset too
// large for static assets.
import { decodePath } from "./path.js";
import { baseName } from "./request.js";
import { internalError, methodNotAllowed, notFound } from "./responses.js";
import { asset } from "./shells.js";

const IMMUTABLE = "public, max-age=31536000, immutable";

// Whether a clean escaped path is under /_hostthis/, matched as ServeMux
// matches it: the first segment decoded, and something after its slash.
export function isAssetPath(pathname) {
  const rest = pathname.slice(1);
  const slash = rest.indexOf("/");
  return slash >= 0 && decodePath(rest.slice(0, slash)) === "_hostthis";
}

// hostthisd resolves the name from the path's last segment, so
// /_hostthis/mdshell/md.js is md.js.
export async function serveAsset(request, env, decodedPath) {
  const { method } = request;
  if (method !== "GET" && method !== "HEAD") {
    return methodNotAllowed();
  }
  const name = baseName(decodedPath);
  const found = asset(name);
  if (!found) {
    return notFound();
  }
  const headers = { "Content-Type": found.contentType, "Cache-Control": IMMUTABLE };
  if (env.ASSETS && !found.r2) {
    const res = await env.ASSETS.fetch(new Request(new URL(`/_hostthis/${name}`, request.url), { method }));
    if (!res.ok) {
      await res.body?.cancel();
      console.warn(`warn: asset 500: ${name}: static assets answered ${res.status}`);
      return internalError();
    }
    return new Response(res.body, { headers });
  }
  if (method === "HEAD") {
    return new Response(null, { headers });
  }
  // Stored as is by the deploy, not in the at-rest format.
  const object = await env.PAYLOADS.get(found.key);
  if (object === null) {
    console.warn(`warn: asset 500: ${name}: object ${found.key} missing`);
    return internalError();
  }
  return new Response(object.body, { headers });
}
