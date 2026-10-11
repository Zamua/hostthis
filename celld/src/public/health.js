// hostthisd's /healthz and /readyz, on every public host and any method.

const BODIES = {
  "/healthz": ["ok\n", "text/plain; charset=utf-8"],
  "/readyz": ["{\"ready\":true}\n", "application/json; charset=utf-8"],
};

// The response for a health path, or null for any other path.
export function serveHealth(decodedPath) {
  if (!Object.hasOwn(BODIES, decodedPath)) {
    return null;
  }
  const [body, contentType] = BODIES[decodedPath];
  return new Response(body, {
    headers: { "Content-Type": contentType, "Cache-Control": "no-store" },
  });
}
