// hostthis on celld: the metadata plane as Durable Objects.
//
// One cell per SCOPE for the intent log, which is the owner identity. That is
// not an arbitrary choice: hostthis keeps a durable intent log precisely
// because a paste's row and its owner index cannot co-locate, and celld
// reproduces that condition by putting them in different cells. Recovery reads
// one scope, so the scope is the cell (docs/design/celld-port-audit.md
// "Finding 5").
//
// Storage is the KV surface rather than SQL: a scope holds few outstanding
// intents, ordering is applied on read, and the KV API is the one whose
// behaviour has been measured on this runtime.

const INTENT_PREFIX = "intent:";

function intentKey(id) {
  return INTENT_PREFIX + id;
}

// A stored intent is a private shape, so the Go port's type can change without
// a stored-format migration. Mirrors internal/storage's intentRow.
function toRow(body) {
  return {
    kind: body.kind ?? "",
    subject: body.subject ?? "",
    reached: Array.isArray(body.reached) ? body.reached : [],
    // Guard is opaque bytes, carried base64 so JSON cannot mangle it.
    guard: body.guard ?? "",
    startedAt: body.startedAt ?? 0,
  };
}

function fromRow(id, scope, row) {
  return {
    id,
    scope,
    kind: row.kind,
    subject: row.subject,
    reached: row.reached ?? [],
    guard: row.guard ?? "",
    startedAt: row.startedAt ?? 0,
  };
}

export class IntentLog {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const scope = url.searchParams.get("scope") ?? "";
    const op = url.pathname.split("/").pop();

    switch (op) {
      case "begin":
        return this.begin(await request.json());
      case "advance":
        return this.advance(await request.json());
      case "complete":
        return this.complete(await request.json());
      case "outstanding":
        return this.outstanding(scope);
      default:
        return new Response("unknown op\n", { status: 404 });
    }
  }

  // Recording the same id twice must converge on ONE intent rather than
  // accumulate, which is why the id is the key and not a generated one.
  async begin(body) {
    await this.state.storage.put(intentKey(body.id), toRow(body));
    return new Response(null, { status: 204 });
  }

  // Advancing past a step already recorded is a no-op, and advancing an intent
  // that is already complete is a race rather than a fault.
  async advance(body) {
    const key = intentKey(body.id);
    const row = await this.state.storage.get(key);
    if (row === undefined || row === null) {
      return new Response(null, { status: 204 });
    }
    if (!row.reached.includes(body.step)) {
      row.reached = [...row.reached, body.step];
      await this.state.storage.put(key, row);
    }
    return new Response(null, { status: 204 });
  }

  // Forgetting an intent twice is normal when two resolvers race, so a missing
  // key is success.
  async complete(body) {
    await this.state.storage.delete(intentKey(body.id));
    return new Response(null, { status: 204 });
  }

  // The ONLY read, and it is scope-bounded by construction: a cell cannot see
  // another scope's storage, so the property the KV backend has to maintain by
  // key layout is structural here.
  async outstanding(scope) {
    const stored = await this.state.storage.list({ prefix: INTENT_PREFIX });
    const out = [];
    for (const [key, row] of stored) {
      out.push(fromRow(key.slice(INTENT_PREFIX.length), scope, row));
    }
    out.sort((a, b) => a.startedAt - b.startedAt);
    return Response.json(out);
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return new Response("ok\n");
    }
    if (!url.pathname.startsWith("/intents/")) {
      return new Response("not found\n", { status: 404 });
    }
    const scope = url.searchParams.get("scope");
    if (!scope) {
      return new Response("scope required\n", { status: 400 });
    }
    // The scope IS the cell. idFromName is deterministic, so every node routes
    // one owner's intents to one cell without coordinating.
    const id = env.INTENTS.idFromName(scope);
    return env.INTENTS.get(id).fetch(request);
  },
};
