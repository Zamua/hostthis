// The read path: one slug's answer from its Paste cell, mapped to the response
// hostthisd gives for the same case (docs/SPEC.md "The public surface").
import { reportingErrors } from "./blobs.js";
import { notModified } from "./conditional.js";
import { goStringList } from "./gojson.js";
import { formatHTTPDate } from "./httpdate.js";
import { exactLength } from "./length.js";
import { FAILED_PAGE, LOADING_PAGE } from "./pages.js";
import { internalError, methodNotAllowed, notFound } from "./responses.js";
import { rawContentType, renderShell } from "./shells.js";

// Every content response's: stored by any cache, revalidated before each reuse.
export const CONTENT_CACHE_CONTROL = "public, no-cache";

const PERMISSIONS_POLICY = "camera=(), microphone=(), geolocation=(), usb=(), payment=()";
const HTML = "text/html; charset=utf-8";

function sandbox() {
  return { "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer" };
}

function warn(slug, cause) {
  console.warn(`warn: read 500: slug=${slug} ${cause}`);
}

// The answer for target, from a request built of parsed values alone.
async function resolveAnswer(env, { slug, path, flags }) {
  const query = new URLSearchParams({ slug, path });
  if (flags.raw) query.set("raw", "1");
  if (flags.files) query.set("files", "1");
  const cell = env.PASTES.get(env.PASTES.idFromName(slug));
  const res = await cell.fetch(new Request(`https://cell/paste/resolve?${query}`));
  if (!res.ok) {
    throw new Error(`resolve answered ${res.status}`);
  }
  return res.json();
}

function statusPage(status, page, extra) {
  const headers = { ...sandbox(), "Content-Type": HTML, "Cache-Control": "no-store", ...extra };
  return new Response(page, { status, headers });
}

// openBlob(env, entry, ctx) returns the entry's decoded bytes as a stream, or
// throws.
export function readPath(openBlob) {
  return async function read(request, env, ctx, target) {
    const { method } = request;
    if (method !== "GET" && method !== "HEAD") {
      return methodNotAllowed();
    }
    let answer;
    try {
      answer = await resolveAnswer(env, target);
    } catch (error) {
      warn(target.slug, `metadata resolve: ${error.message}`);
      return internalError();
    }
    if (answer.found === false) {
      return notFound();
    }
    if (answer.status === "pending") {
      return statusPage(200, LOADING_PAGE, { "Retry-After": "1" });
    }
    if (answer.status === "failed") {
      return statusPage(410, FAILED_PAGE, {});
    }

    const { kind, updatedAt, entry } = answer;
    // The validators and the cache posture, on a 200 and a 304 alike.
    const content = (etag) => ({
      ...sandbox(),
      "Permissions-Policy": PERMISSIONS_POLICY,
      "Cache-Control": CONTENT_CACHE_CONTROL,
      "ETag": etag,
      "Last-Modified": formatHTTPDate(updatedAt),
    });
    const unchanged = (headers) => notModified(request.headers, headers.ETag, updatedAt);

    const stored = async (contentType, what) => {
      const headers = content(`"${entry.key}"`);
      if (unchanged(headers)) {
        return new Response(null, { status: 304, headers });
      }
      headers["Content-Type"] = contentType;
      if (method === "HEAD") {
        return new Response(null, { headers });
      }
      let body;
      try {
        body = await openBlob(env, entry, ctx);
      } catch (error) {
        warn(target.slug, `${what} blob read: ${error.message}`);
        return internalError();
      }
      // The manifest's size is the decoded length; held to it, damage found
      // after the headers leaves a body short of its Content-Length.
      const size = entry.size;
      if (Number.isSafeInteger(size) && size >= 0) {
        headers["Content-Length"] = String(size);
        body = exactLength(body, size);
      }
      const reported = reportingErrors(body, (error) => warn(target.slug, `${what} blob read: ${error.message}`));
      return new Response(reported, { headers });
    };

    const shellPage = (shellKind) => {
      const shell = renderShell(shellKind);
      if (!shell) {
        warn(target.slug, `unsupported stored kind ${JSON.stringify(kind)}`);
        return internalError("unsupported kind");
      }
      const headers = content(`"${shell.version}"`);
      if (unchanged(headers)) {
        return new Response(null, { status: 304, headers });
      }
      headers["Content-Security-Policy"] = shell.csp;
      headers["Content-Type"] = HTML;
      return new Response(shell.body, { headers });
    };

    switch (answer.action) {
      case "notfound":
        return notFound();
      case "html":
        return stored(HTML, "html");
      case "raw":
        return stored(rawContentType(kind) ?? "text/plain; charset=utf-8", `raw ${kind}`);
      case "file":
        return stored(entry.contentType || "application/octet-stream", "file");
      case "kbraw":
        return stored(rawContentType("knowledgebase"), "document");
      case "shell":
      case "kbshell":
        return shellPage(answer.shell);
      case "files": {
        const headers = content(`"files-${answer.uploadId}"`);
        if (unchanged(headers)) {
          return new Response(null, { status: 304, headers });
        }
        headers["Content-Type"] = "application/json; charset=utf-8";
        return new Response(goStringList(answer.files), { headers });
      }
      default:
        warn(target.slug, `unknown resolve action ${JSON.stringify(answer.action)}`);
        return internalError();
    }
  };
}
