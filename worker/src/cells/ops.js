// Each cell class routes through an op table: op name to { run, mutating,
// body, room }. A mutating op runs under blockConcurrencyWhile so its
// read-check-write sequence cannot interleave; body parses the request JSON;
// room loads the room document and answers 404 without one. run receives
// (self, body, url, doc).
export const read = (run, flags = {}) => ({ run, ...flags });
export const mutation = (run, flags = {}) => ({ mutating: true, body: true, run, ...flags });

export function runOp(self, entry, body, url) {
  const run = async () => {
    let doc;
    if (entry.room) {
      doc = await self.loadRoom();
      if (!doc) {
        return new Response("not found\n", { status: 404 });
      }
    }
    return entry.run(self, body, url, doc);
  };
  return entry.mutating ? self.state.blockConcurrencyWhile(run) : run();
}

// Null for an op the table does not name, so a class can fall through to its
// own default. The `op` query names the op when the path cannot: celld's
// internal `/do/<cell>` route addresses a cell by id and takes no further path,
// which is how an operator tool reaches a cell without knowing its name.
export async function dispatch(self, table, request) {
  const url = new URL(request.url);
  const entry = table[url.pathname.split("/").pop()] ?? table[url.searchParams.get("op")];
  if (!entry) {
    return null;
  }
  return runOp(self, entry, entry.body ? await request.json() : undefined, url);
}

// One cell-to-cell POST: the cell named id in namespace ns, addressed by the
// query key the Worker's router expects for that namespace.
export function cellCall(ns, id, path, query, body) {
  return ns.get(ns.idFromName(id)).fetch(new Request(
    `https://cell${path}?${query}=${encodeURIComponent(id)}`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    },
  ));
}

// Field checks on a request body: "string" is a non-empty string, "uint" a
// safe integer at or above zero, "posint" one at or above one.
export function requireShape(body, shape) {
  for (const [key, kind] of Object.entries(shape)) {
    const value = body[key];
    const ok = kind === "string"
      ? typeof value === "string" && value !== ""
      : Number.isSafeInteger(value) && value >= (kind === "posint" ? 1 : 0);
    if (!ok) {
      return false;
    }
  }
  return true;
}
