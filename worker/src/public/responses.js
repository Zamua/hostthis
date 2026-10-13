// Responses shaped as Go's net/http writes them, so an error reads the same
// from hostthisd and the Worker. Pure, no I/O.

// Go's http.Error.
export function goError(status, message, headers = {}) {
  return new Response(`${message}\n`, {
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
