// The router for public hostnames (docs/SPEC.md "Public and internal hosts").
// It never passes a client request to a cell or the payload bucket: any call
// it makes is built from values it parsed.

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

// host is the normalized public host: the request's own, or the forwarded one.
export async function publicRouter(_request, _env, _host) {
  return notFound();
}
