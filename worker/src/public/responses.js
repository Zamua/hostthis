// Responses shaped as Go's net/http writes them, so an error reads the same
// from hostthisd and the Worker. Pure, no I/O.

const encoder = new TextEncoder();

// A response whose whole body is text in hand, stating its length as Go's
// server does for a small body. Without it the runtime sends the body chunked,
// and a HEAD, which drops the body, would state no length at all.
export function sized(text, init = {}) {
  const body = encoder.encode(text);
  return new Response(body, {
    ...init,
    headers: { ...init.headers, "Content-Length": String(body.byteLength) },
  });
}

// Go's http.Error.
export function goError(status, message, headers = {}) {
  return sized(`${message}\n`, {
    status,
    headers: {
      ...headers,
      "Content-Type": "text/plain; charset=utf-8",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

// Go's http.NotFound.
export function notFound() {
  return goError(404, "404 page not found");
}

export function internalError(message = "internal error") {
  return goError(500, message);
}

export function methodNotAllowed() {
  return goError(405, "Method Not Allowed", { Allow: "GET, HEAD" });
}
