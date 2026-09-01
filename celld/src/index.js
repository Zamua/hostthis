// The owner identity scopes intents, quota reservations, and the paste index.
// Paste rows use slug-scoped cells, so cross-cell creates recover through the
// owner's durable intent log.

const INTENT_PREFIX = "intent:";
const CREATE_ABORT_PREFIX = "create-abort:";
const ARTIFACT_ACCOUNT_PREFIX = "artifact-account:";
const ARTIFACT_PENDING = "artifactPending";
const ARTIFACT_RECEIPT_PREFIX = "artifact-receipt:";
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const BASE64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

function artifactAccountKey(slug, generation) {
  return `${ARTIFACT_ACCOUNT_PREFIX}${slug}:${generation}`;
}

function chargedSize(entry) {
  return entry.chargedSize ?? entry.size ?? 0;
}

function intentKey(id) {
  return INTENT_PREFIX + id;
}

function createAbortKey(generation) {
  return CREATE_ABORT_PREFIX + generation;
}

function setOwn(object, key, value) {
  Object.defineProperty(object, key, {
    value,
    enumerable: true,
    configurable: true,
    writable: true,
  });
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
    generation: body.generation ?? "",
    status: body.status ?? "",
    fingerprint: body.fingerprint ?? "",
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
    generation: row.generation ?? "",
    status: row.status ?? "",
    fingerprint: row.fingerprint ?? "",
    startedAt: row.startedAt ?? 0,
  };
}

// The Identity cell owns the state that must agree for one owner: outstanding
// intents, quota reservations, and the paste index. Every mutating request uses
// blockConcurrencyWhile so its read-check-write sequence cannot interleave.
export class Identity {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const scope = url.searchParams.get("scope") ?? "";
    const op = url.pathname.split("/").pop();

    switch (op) {
      case "outstanding":
        return this.outstanding(scope);
      case "bytes":
        return this.bytes();
      case "list":
        return this.list();
      case "allocations":
        return this.allocations();
      case "firstSeen":
        return this.firstSeen();
    }
    return this.state.blockConcurrencyWhile(async () => {
      switch (op) {
        case "begin":
          return this.begin(await request.json());
        case "advance":
          return this.advance(await request.json());
        case "complete":
          return this.complete(await request.json());
        case "reserve":
          return this.reserve(await request.json());
        case "release":
          return this.release(await request.json());
        case "confirm":
          return this.confirm(await request.json());
        case "drop":
          return this.drop(await request.json());
        case "touch":
          return this.touch(await request.json());
        case "artifactseed":
          return this.artifactSeed(await request.json());
        case "artifactdecide":
          return this.artifactDecide(await request.json());
        case "artifactproject":
          return this.artifactProject(await request.json());
        case "artifactdrop":
          return this.artifactDrop(await request.json());
        case "notesubnet":
          return this.noteSubnet(await request.json());
        case "subnets":
          return this.subnets(Number(url.searchParams.get("now")),
            Number(url.searchParams.get("window")));
        default:
          return new Response("unknown op\n", { status: 404 });
      }
    });
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

  // Quota admission, reservation, and intent creation must commit as one local
  // transaction. updatedAt comes from the paste because it defines list order.
  async reserve(body) {
    if (typeof body.size !== "number" || body.size < 0) {
      return Response.json({ error: "negative-size" }, { status: 400 });
    }
    const generation = body.generation ?? "";
    if (!generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (!body.intent || (
      body.intent.kind !== "create_paste"
      || typeof body.intent.id !== "string" || !body.intent.id
      || body.intent.subject !== body.slug
      || typeof body.intent.fingerprint !== "string" || !body.intent.fingerprint
    )) {
      return Response.json({ error: "malformed-create-intent" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const existing = entries[body.slug];
      if (existing) {
        if (existing.generation !== generation) {
          return Response.json({ error: "slug-taken" }, { status: 409 });
        }
        // Replay tolerance exists for response loss inside one unresolved
        // create. A discharged intent means that create completed, so a
        // repeat is a duplicate insert, not a retry.
        if (await tx.get(intentKey(body.intent.id)) === undefined) {
          return Response.json({ error: "slug-taken" }, { status: 409 });
        }
        if (chargedSize(existing) !== body.size ||
            existing.createFingerprint !== body.intent.fingerprint) {
          return Response.json({ error: "reservation-mismatch" }, { status: 409 });
        }
        const active = Object.values(entries).reduce((n, e) => n + chargedSize(e), 0);
        return Response.json({ active, replayed: true });
      }
      const active = Object.values(entries).reduce((n, e) => n + chargedSize(e), 0);
      if (body.userCap > 0 && active + body.size > body.userCap) {
        return Response.json({ error: "over-quota", active }, { status: 507 });
      }
      entries[body.slug] = {
        generation,
        size: body.size,
        chargedSize: body.size,
        servedSize: body.size,
        accountingVersion: 0,
        status: body.status ?? "pending",
        at: body.now ?? 0,
        updatedAt: body.updatedAt ?? body.now ?? 0,
        latestVersion: 1,
        kind: body.kind ?? "",
        name: body.name ?? "",
        contentSha: body.contentSha ?? "",
        createFingerprint: body.intent?.fingerprint ?? "",
      };
      const firstSeen = await tx.get("firstSeen");
      const updates = new Map([
        ["entries", entries],
        [artifactAccountKey(body.slug, generation), {
          version: 0, allocated: body.size, target: body.size,
        }],
      ]);
      if (firstSeen === undefined || firstSeen === null) {
        updates.set("firstSeen", body.now ?? 0);
      }
      if (body.intent) {
        updates.set(intentKey(body.intent.id), toRow({
          ...body.intent,
          generation,
          status: body.status ?? "pending",
        }));
      }
      await tx.put(updates);
      if (body.intent) {
        await tx.setAlarm(Date.now());
      }
      return Response.json({ active: active + body.size });
    });
  }

  // Step 3: the row landed, so the entry is real and the intent is discharged.
  async confirm(body) {
    if (typeof body.generation !== "string" || !body.generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const entry = entries[body.slug];
      if (!entry) {
        return Response.json({ error: "reservation-absent" }, { status: 404 });
      }
      if (entry.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      if (!entry.status || entry.status === "pending") {
        entry.status = body.status ?? "ready";
      }
      await tx.put("entries", entries);
      if (body.intentId) {
        await tx.delete(intentKey(body.intentId));
      }
      return new Response(null, { status: 204 });
    });
  }

  // Compensation: the create failed, so the owner stops being charged. Dropping
  // the entry rather than marking it is deliberate - a failed paste charges
  // nothing, and the row itself stays in the paste cell to serve an error.
  async release(body) {
    if (typeof body.generation !== "string" || !body.generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const entry = entries[body.slug];
      if (entry && entry.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      if (entry) {
        const key = artifactAccountKey(body.slug, entry.generation);
        const current = await tx.get(key);
        if (current) {
          await tx.put(key, {
            version: current.version + 1,
            allocated: 0,
            target: 0,
          });
        }
        delete entries[body.slug];
      }
      await tx.put("entries", entries);
      if (body.intentId) {
        await tx.delete(intentKey(body.intentId));
      }
      return new Response(null, { status: 204 });
    });
  }

  pasteCell(slug) {
    return this.env.PASTE.get(this.env.PASTE.idFromName(slug));
  }

  async resolveCreateIntent(id, intent) {
    if (
      !intent.subject || !intent.generation
      || typeof intent.fingerprint !== "string" || !intent.fingerprint
    ) {
      return false;
    }
    let response;
    try {
      response = await this.pasteCell(intent.subject).fetch(new Request(
        `https://cell/paste/abortcreate?slug=${encodeURIComponent(intent.subject)}`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ generation: intent.generation }),
        },
      ));
    } catch {
      return false;
    }
    if (!response.ok) {
      return false;
    }
    let state;
    try {
      state = await response.json();
    } catch {
      return false;
    }
    if (state.present === false) {
      const released = await this.release({
        slug: intent.subject,
        generation: intent.generation,
        intentId: id,
      });
      return released.ok;
    }
    if (
      state.present !== true
      || !state.row
      || state.row.generation !== intent.generation
      || state.fingerprint !== intent.fingerprint
    ) {
      return false;
    }
    const confirmed = await this.confirm({
      slug: intent.subject,
      generation: intent.generation,
      status: state.row.status || "pending",
      intentId: id,
    });
    return confirmed.ok;
  }

  async alarm() {
    return this.state.blockConcurrencyWhile(async () => {
      const intents = await this.state.storage.list({ prefix: INTENT_PREFIX });
      let retry = false;
      for (const [key, intent] of intents) {
        if (intent.kind !== "create_paste") {
          retry = true;
          continue;
        }
        const id = key.slice(INTENT_PREFIX.length);
        if (!await this.resolveCreateIntent(id, intent)) {
          retry = true;
        }
      }
      if (retry) {
        await this.state.storage.setAlarm(Date.now() + 1000);
      } else {
        await this.state.storage.deleteAlarm();
      }
    });
  }

  // A point read of a maintained aggregate, not a scan: the identity cell
  // already holds every entry it charges for.
  async bytes() {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const total = Object.values(entries).reduce((n, e) => n + chargedSize(e), 0);
    // A negative charge cannot arise from any legal sequence of reserves and
    // releases: membership makes it unreachable, and every size is checked
    // non-negative on the way in. Refusing it here means a regression to
    // arithmetic release - where a replayed release double-subtracts - is loud
    // at the moment it happens rather than silent until a test happens to look.
    if (total < 0) {
      return Response.json(
        { error: "impossible-state", detail: `charged total ${total} is negative` },
        { status: 500 },
      );
    }
    return Response.json({ bytes: total });
  }

  // The owner's listing, from the maintained summary rather than a scan.
  async list() {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const out = Object.entries(entries).map(([slug, e]) => ({
      slug,
      ...e,
      chargedSize: chargedSize(e),
      servedSize: e.servedSize ?? e.size ?? 0,
    }));
    // MOST RECENTLY UPDATED FIRST. Falls back to creation time for an entry
    // never updated, so a fresh owner still lists in a stable order.
    out.sort((a, b) => ((b.updatedAt ?? b.at ?? 0) - (a.updatedAt ?? a.at ?? 0)));
    return Response.json(out);
  }

  async allocations() {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const stored = await this.state.storage.list({ prefix: ARTIFACT_ACCOUNT_PREFIX });
    const allocations = [];
    let total = 0;
    for (const [key, record] of stored) {
      const scoped = key.slice(ARTIFACT_ACCOUNT_PREFIX.length);
      const separator = scoped.indexOf(":");
      if (separator < 1 || !record ||
          !Number.isSafeInteger(record.version) || record.version < 0 ||
          !Number.isSafeInteger(record.allocated) || record.allocated < 0 ||
          !Number.isSafeInteger(record.target) || record.target < 0) {
        return Response.json({ error: "invalid-artifact-allocation" }, { status: 500 });
      }
      total += record.allocated;
      if (!Number.isSafeInteger(total)) {
        return Response.json({ error: "invalid-artifact-allocation" }, { status: 500 });
      }
      allocations.push({
        slug: scoped.slice(0, separator),
        generation: scoped.slice(separator + 1),
        version: record.version,
        allocated: record.allocated,
        target: record.target,
      });
    }
    allocations.sort((a, b) => a.slug.localeCompare(b.slug) || a.generation.localeCompare(b.generation));
    const projections = Object.entries(entries).map(([slug, entry]) => ({
      slug,
      ...entry,
      chargedSize: chargedSize(entry),
      servedSize: entry.servedSize ?? entry.size ?? 0,
    })).sort((a, b) => a.slug.localeCompare(b.slug));
    const projectionTotal = projections.reduce((sum, entry) => sum + entry.chargedSize, 0);
    if (!Number.isSafeInteger(projectionTotal) || projectionTotal < 0) {
      return Response.json({ error: "invalid-artifact-allocation" }, { status: 500 });
    }
    return Response.json({ total, projectionTotal, allocations, projections });
  }

  async firstSeen() {
    return Response.json({ firstSeen: (await this.state.storage.get("firstSeen")) ?? 0 });
  }

  // Removes an index entry whose paste no longer exists. The caller has already
  // established the absence; the cell only owns the index.
  async drop(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const entry = entries[body.slug];
    if (entry?.generation) {
      return Response.json({ dropped: false, reason: "managed-artifact" });
    }
    const had = entry !== undefined;
    if (had) {
      delete entries[body.slug];
      await this.state.storage.put("entries", entries);
    }
    return Response.json({ dropped: had });
  }

  // Mutable fields are duplicated into the owner index so listing remains a
  // point read. Every mutation must update both cells or the listing goes stale.
  async touch(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const e = entries[body.slug];
    if (!e) {
      return Response.json({ updated: false });
    }
    if (body.generation && e.generation && body.generation !== e.generation) {
      return Response.json({ updated: false, reason: "generation-mismatch" }, { status: 409 });
    }
    if (body.at !== undefined && body.at !== null) {
      e.updatedAt = body.at;
    }
    for (const k of ["name", "status", "kind", "latestVersion", "pinnedVersion", "servedSize", "contentSha"]) {
      if (body[k] !== undefined && body[k] !== null) {
        e[k] = body[k];
      }
    }
    await this.state.storage.put("entries", entries);
    return Response.json({ updated: true });
  }

  async artifactSeed(body) {
    if (typeof body.slug !== "string" || !body.slug ||
        typeof body.generation !== "string" || !body.generation ||
        !Number.isSafeInteger(body.charge) || body.charge < 0 ||
        !Number.isSafeInteger(body.servedSize) || body.servedSize < 0) {
      return Response.json({ error: "invalid-artifact-seed" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const existing = entries[body.slug];
      if (existing?.generation && existing.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      const key = artifactAccountKey(body.slug, body.generation);
      const decision = await tx.get(key);
      if (decision && decision.allocated !== body.charge) {
        return Response.json({
          error: "artifact-seed-conflict", version: decision.version,
          allocated: decision.allocated, target: decision.target,
        }, { status: 409 });
      }
      const version = decision?.version ?? 0;
      entries[body.slug] = {
        ...(existing ?? {}),
        generation: body.generation,
        size: body.charge,
        chargedSize: body.charge,
        servedSize: body.servedSize,
        accountingVersion: version,
        status: body.status ?? existing?.status ?? "ready",
        at: body.createdAt ?? existing?.at ?? 0,
        updatedAt: body.updatedAt ?? existing?.updatedAt ?? body.createdAt ?? 0,
        latestVersion: body.latestVersion ?? existing?.latestVersion ?? 1,
        pinnedVersion: body.pinnedVersion ?? existing?.pinnedVersion ?? 0,
        kind: body.kind ?? existing?.kind ?? "",
        name: body.name ?? existing?.name ?? "",
        contentSha: body.contentSha ?? existing?.contentSha ?? "",
      };
      const updates = new Map([["entries", entries]]);
      if (!decision) {
        updates.set(key, { version: 0, allocated: body.charge, target: body.charge });
      }
      await tx.put(updates);
      return Response.json({ seeded: !decision, version, allocated: body.charge });
    });
  }

  async artifactDecide(body) {
    if (typeof body.slug !== "string" || !body.slug ||
        typeof body.generation !== "string" || !body.generation ||
        !Number.isSafeInteger(body.version) || body.version < 1 ||
        !Number.isSafeInteger(body.target) || body.target < 0) {
      return Response.json({ error: "invalid-artifact-decision" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const entry = entries[body.slug];
      if (!entry) {
        return Response.json({ error: "artifact-absent" }, { status: 404 });
      }
      if (entry.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      const key = artifactAccountKey(body.slug, body.generation);
      const current = await tx.get(key);
      if (!current) {
        return Response.json({ error: "artifact-unseeded" }, { status: 409 });
      }
      const total = Object.values(entries).reduce((sum, item) => sum + chargedSize(item), 0);
      if (body.version < current.version) {
        return Response.json({
          error: "stale-version", version: current.version,
          allocated: current.allocated, total,
        }, { status: 409 });
      }
      if (body.version === current.version) {
        if (body.target !== current.target) {
          return Response.json({
            error: "version-target-mismatch", version: current.version,
            allocated: current.allocated, total,
          }, { status: 409 });
        }
        const granted = current.target === current.allocated;
        return Response.json({
          granted, version: current.version, allocated: current.allocated, total,
        }, { status: granted ? 200 : 507 });
      }
      if (body.version !== current.version + 1) {
        return Response.json({
          error: "version-gap", version: current.version,
          allocated: current.allocated, total,
        }, { status: 409 });
      }
      const nextTotal = total - current.allocated + body.target;
      if (!Number.isSafeInteger(nextTotal) || nextTotal < 0) {
        return Response.json({ error: "invalid-artifact-total" }, { status: 500 });
      }
      const granted = body.target <= current.allocated ||
        body.userCap <= 0 || nextTotal <= body.userCap;
      const record = {
        version: body.version,
        allocated: granted ? body.target : current.allocated,
        target: body.target,
      };
      entry.accountingVersion = body.version;
      if (granted) {
        entry.size = body.target;
        entry.chargedSize = body.target;
      }
      await tx.put(new Map([[key, record], ["entries", entries]]));
      return Response.json({
        granted, version: record.version, allocated: record.allocated,
        total: granted ? nextTotal : total,
      }, { status: granted ? 200 : 507 });
    });
  }

  async artifactProject(body) {
    if (typeof body.slug !== "string" || !body.slug ||
        typeof body.generation !== "string" || !body.generation ||
        !Number.isSafeInteger(body.version) || body.version < 0 ||
        !Number.isSafeInteger(body.servedSize) || body.servedSize < 0) {
      return Response.json({ error: "invalid-artifact-projection" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const entry = entries[body.slug];
      if (!entry) {
        return Response.json({ error: "artifact-absent" }, { status: 404 });
      }
      if (entry.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      const decision = await tx.get(artifactAccountKey(body.slug, body.generation));
      if (!decision || decision.version !== body.version) {
        return Response.json({ error: "projection-version-mismatch" }, { status: 409 });
      }
      entry.servedSize = body.servedSize;
      for (const key of ["status", "kind", "name", "contentSha", "latestVersion", "pinnedVersion", "updatedAt"]) {
        if (body[key] !== undefined && body[key] !== null) {
          entry[key] = body[key];
        }
      }
      await tx.put("entries", entries);
      return Response.json({ updated: true });
    });
  }

  async artifactDrop(body) {
    if (typeof body.slug !== "string" || !body.slug ||
        typeof body.generation !== "string" || !body.generation ||
        !Number.isSafeInteger(body.version) || body.version < 1) {
      return Response.json({ error: "invalid-artifact-drop" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const entries = (await tx.get("entries")) ?? {};
      const entry = entries[body.slug];
      const decision = await tx.get(artifactAccountKey(body.slug, body.generation));
      if (!decision || decision.version !== body.version || decision.allocated !== 0 ||
          decision.target !== 0) {
        return Response.json({ error: "drop-before-release" }, { status: 409 });
      }
      if (entry && entry.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      if (entry) {
        delete entries[body.slug];
        await tx.put("entries", entries);
      }
      return Response.json({ dropped: Boolean(entry) });
    });
  }

  // The reverse index avoids fan-out across every subnet cell.
  async noteSubnet(body) {
    const subnets = (await this.state.storage.get("subnets")) ?? {};
    if (!Object.hasOwn(subnets, body.subnet)) {
      subnets[body.subnet] = body.now;
      await this.state.storage.put("subnets", subnets);
    }
    return new Response(null, { status: 204 });
  }

  async subnets(now, windowMs) {
    const subnets = (await this.state.storage.get("subnets")) ?? {};
    let changed = false;
    let n = 0;
    for (const [cidr, at] of Object.entries(subnets)) {
      if (now - at >= windowMs) {
        delete subnets[cidr];
        changed = true;
      } else {
        n++;
      }
    }
    if (changed) {
      await this.state.storage.put("subnets", subnets);
    }
    return Response.json({ count: n });
  }

  // The ONLY intent read, and it is scope-bounded by construction: a cell cannot see
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


// A Room cell owns one room's KV document, dense mutation sequence, budget
// recovery state, and hibernatable WebSockets. Its app's Paste cell serializes
// cross-room byte allocations.
export class Room {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  // The app's paste cell, which owns the per-app room budget and the creation
  // ledger. Reached CELL TO CELL from inside this room's event: the app cell
  // decides against its own current totals, so the per-app cap is exact, and
  // the caller pays one round trip instead of three.
  // The room's whole state as ONE document: five separate keys made a commit
  // five sequential storage writes, which was most of a write's latency. A
  // room is capped at 256 KiB of values, so one document is always small.
  async loadRoom() {
    let doc = await this.state.storage.get("state");
    if (doc === undefined) {
      // A room written before the single-document layout: assemble once from
      // the legacy keys; the next commit persists the consolidated form.
      const meta = await this.state.storage.get("meta");
      if (meta === undefined) {
        return undefined;
      }
      doc = {
        meta,
        kv: (await this.state.storage.get("kv")) ?? {},
        wire: (await this.state.storage.get("wire")) ?? {},
        bytes: (await this.state.storage.get("bytes")) ?? 0,
        seq: (await this.state.storage.get("seq")) ?? 0,
        budgetVersion: 0,
      };
    }
    doc.budgetVersion ??= 0;
    return doc;
  }

  async saveRoom(doc) {
    await this.state.storage.put("state", doc);
  }

  // Broadcast to every connected socket, dead ones dropped silently: send() on
  // a closing socket throws, and one dead client must not cost the rest their
  // frame.
  broadcast(frame) {
    const data = JSON.stringify(frame);
    for (const ws of this.state.getWebSockets()) {
      try {
        ws.send(data);
      } catch {
        // reaped by the runtime; nothing to do
      }
    }
  }

  appCell(appSlug) {
    return this.env.PASTES.get(this.env.PASTES.idFromName(appSlug));
  }

  async appCall(appSlug, op, body) {
    const res = await this.appCell(appSlug).fetch(
      new Request(`https://cell/paste/${op}?slug=${encodeURIComponent(appSlug)}`, {
        method: "POST",
        body: JSON.stringify(body),
        headers: { "content-type": "application/json" },
      }),
    );
    return res;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.pathname.split("/").pop();
    if (op === "count") {
      return Response.json({
        sockets: this.state.getWebSockets().length,
        // Survives hibernation, so a non-zero value after a move proves the
        // cell was restored rather than freshly created.
        seen: (await this.state.storage.get("seen")) ?? 0,
      });
    }
    switch (op) {
      case "create":
        return this.create(await request.json());
      case "meta":
        return this.meta();
      case "get":
        return this.getValue(url.searchParams.get("key"));
      case "scan":
        return this.scan();
      case "put":
        return this.put(await request.json());
      case "del":
        return this.del(await request.json());
      case "reconcile":
        return this.state.blockConcurrencyWhile(() => this.reconcileAllocation(url.searchParams.get("id")));
    }
    if (request.headers.get("Upgrade") !== "websocket") {
      return new Response("expected websocket\n", { status: 426 });
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    // acceptWebSocket, not server.accept(): the hibernatable form is what lets
    // the runtime evict the cell while the socket stays open.
    this.state.acceptWebSocket(server);
    // The snapshot goes out in the SAME event that attaches the socket, so no
    // mutation can land between the state read and the attach - the atomicity
    // the multi-pod design had to buy with the seq splice contract.
    {
      const doc = await this.loadRoom();
      const state = {};
      if (doc) {
        for (const k of Object.keys(doc.kv)) {
          setOwn(state, k, Object.hasOwn(doc.wire, k) ? JSON.parse(doc.wire[k]) : null);
        }
      }
      server.send(JSON.stringify({ type: "snapshot", seq: doc ? doc.seq : 0, state }));
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  async create(body) {
    return this.state.blockConcurrencyWhile(() => this.createBlocked(body));
  }

  async recordRoomCreated(body) {
    const ledger = await this.appCall(body.appSlug, "roomcreated", {
      id: body.id, subnet: body.subnet, at: body.at,
    });
    return ledger.ok;
  }

  async createBlocked(body) {
    const doc = await this.loadRoom();
    if (doc) {
      if (!await this.recordRoomCreated(body)) {
        return Response.json({ error: "room-ledger-unavailable" }, { status: 502 });
      }
      return Response.json({ created: false });
    }
    if (body.appCap > 0) {
      const preflight = await this.appCall(body.appSlug, "roompreflight", {
        appCap: body.appCap,
      });
      if (preflight.status === 507) {
        return Response.json({ error: "app-full" }, { status: 507 });
      }
      if (!preflight.ok) {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
    }
    if (!await this.recordRoomCreated(body)) {
      return Response.json({ error: "room-ledger-unavailable" }, { status: 502 });
    }
    await this.saveRoom({
      meta: {
        appSlug: body.appSlug, id: body.id,
        createdAt: body.createdAt, updatedAt: body.updatedAt,
      },
      kv: {}, wire: {}, bytes: 0, seq: 0, budgetVersion: 0,
    });
    return Response.json({ created: true });
  }

  async meta() {
    const doc = await this.loadRoom();
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json(doc.meta);
  }

  async getValue(key) {
    const doc = await this.loadRoom();
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    if (!Object.hasOwn(doc.kv, key)) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ value: doc.kv[key] });
  }

  async scan() {
    const doc = await this.loadRoom();
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ values: doc.kv, seq: doc.seq });
  }

  async put(body) {
    return this.state.blockConcurrencyWhile(() => this.putBlocked(body));
  }

  async putBlocked(body) {
    let doc = await this.loadRoom();
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    if (doc.pending) {
      const recovered = await this.resumePending(doc);
      if (recovered.outcome === "unavailable") {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
      doc = recovered.doc;
    }
    const checked = this.checkPut(doc, body);
    if (checked.error) {
      return checked.error;
    }
    if (checked.next === doc.bytes) {
      return this.commitEqualPut(doc, body, checked.next);
    }
    return this.putBudgeted(doc, body, checked.next);
  }

  checkPut(doc, body) {
    const size = b64len(body.value);
    const prior = Object.hasOwn(doc.kv, body.key) ? b64len(doc.kv[body.key]) : 0;
    const next = doc.bytes - prior + size;
    if (!Object.hasOwn(doc.kv, body.key) && body.keyCap > 0 &&
        Object.keys(doc.kv).length >= body.keyCap) {
      return { error: Response.json({ error: "room-full" }, { status: 413 }) };
    }
    if (body.roomCap > 0 && next > body.roomCap) {
      return { error: Response.json({ error: "room-full" }, { status: 413 }) };
    }
    return { next };
  }

  applyPut(doc, body, next) {
    setOwn(doc.kv, body.key, body.value);
    if (body.wire !== undefined && body.wire !== null) {
      setOwn(doc.wire, body.key, body.wire);
    }
    doc.bytes = next;
    doc.seq++;
    doc.meta.updatedAt = body.now;
  }

  async commitEqualPut(doc, body, next) {
    this.applyPut(doc, body, next);
    await this.saveRoom(doc);
    this.broadcastPut(doc, body.key);
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  async putBudgeted(doc, body, targetBytes) {
    const growing = targetBytes > doc.bytes;
    doc.budgetVersion++;
    const pending = { targetBytes, appCap: body.appCap };
    doc.pending = pending;
    if (growing) {
      pending.mutation = {
        key: body.key, value: body.value, wire: body.wire, now: body.now,
      };
    } else {
      this.applyPut(doc, body, targetBytes);
    }
    await this.persistPending(doc);
    if (!growing) {
      this.broadcastPut(doc, body.key);
      await this.resumePending(doc);
      return Response.json({ seq: doc.seq, bytes: doc.bytes });
    }

    const result = await this.resumePending(doc);
    if (result.outcome === "unavailable") {
      return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
    }
    if (result.outcome === "refused") {
      return Response.json({ error: "app-full" }, { status: 507 });
    }
    return Response.json({ seq: result.doc.seq, bytes: result.doc.bytes });
  }

  async del(body) {
    return this.state.blockConcurrencyWhile(() => this.delBlocked(body));
  }

  async delBlocked(body) {
    let doc = await this.loadRoom();
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    if (doc.pending) {
      const recovered = await this.resumePending(doc);
      if (recovered.outcome === "unavailable") {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
      doc = recovered.doc;
    }
    if (!Object.hasOwn(doc.kv, body.key)) {
      return this.commitDelete(doc, body);
    }

    const targetBytes = doc.bytes - b64len(doc.kv[body.key]);
    doc.budgetVersion++;
    doc.pending = { targetBytes, appCap: 0 };
    this.applyDelete(doc, body.key, body.now);
    await this.persistPending(doc);
    this.broadcast({ type: "delete", seq: doc.seq, key: body.key });
    await this.resumePending(doc);
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  async commitDelete(doc, body) {
    this.applyDelete(doc, body.key, body.now);
    await this.saveRoom(doc);
    this.broadcast({ type: "delete", seq: doc.seq, key: body.key });
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  applyDelete(doc, key, now) {
    if (Object.hasOwn(doc.kv, key)) {
      doc.bytes -= b64len(doc.kv[key]);
      delete doc.kv[key];
      delete doc.wire[key];
    }
    doc.seq++;
    doc.meta.updatedAt = now;
  }

  async persistPending(doc) {
    await this.state.storage.transaction(async (tx) => {
      await tx.put("state", doc);
      await tx.setAlarm(Date.now());
    });
  }

  async clearPending(doc) {
    delete doc.pending;
    await this.state.storage.transaction(async (tx) => {
      await tx.put("state", doc);
      await tx.deleteAlarm();
    });
  }

  async retryPendingLater() {
    await this.state.storage.setAlarm(Date.now() + 1000);
    return "unavailable";
  }

  async resumePending(doc, rearm = false) {
    doc ??= await this.loadRoom();
    if (!doc?.pending) {
      return { outcome: "complete", doc };
    }
    const pending = doc.pending;
    const version = doc.budgetVersion;
    const growing = pending.mutation !== undefined;
    if (rearm) {
      await this.state.storage.setAlarm(Date.now());
    }

    let response;
    let decision;
    try {
      response = await this.appCall(doc.meta.appSlug, "roomdecide", {
        room: doc.meta.id,
        version,
        targetBytes: pending.targetBytes,
        appCap: pending.appCap,
      });
      decision = await response.json();
    } catch {
      await this.retryPendingLater();
      return { outcome: "unavailable", doc };
    }
    if (response.status === 507 && growing &&
        decision.granted === false && decision.version === version) {
      await this.clearPending(doc);
      return { outcome: "refused", doc };
    }
    if (!response.ok || decision.granted !== true ||
        decision.version !== version ||
        decision.allocated !== pending.targetBytes) {
      await this.retryPendingLater();
      return { outcome: "unavailable", doc };
    }

    if (growing) {
      this.applyPut(doc, pending.mutation, pending.targetBytes);
      await this.clearPending(doc);
      this.broadcastPut(doc, pending.mutation.key);
    } else {
      await this.clearPending(doc);
    }
    return { outcome: "complete", doc };
  }

  async reconcileAllocation(stableCellID) {
    const doc = await this.loadRoom();
    if (!doc) {
      return Response.json({ empty: true });
    }
    if (doc.pending) {
      return Response.json({ error: "pending-budget-operation" }, { status: 409 });
    }
    // Each check names itself: the route is cluster-internal and a refused
    // migration document is undiagnosable from a bare 422.
    const documentChecks = [
      ["app-slug", typeof doc.meta?.appSlug === "string" && doc.meta.appSlug !== ""],
      ["room-uuid", typeof doc.meta?.id === "string" && UUID_V4.test(doc.meta.id)],
      ["created-at", Number.isSafeInteger(doc.meta?.createdAt) && doc.meta.createdAt >= 0],
      ["updated-at", Number.isSafeInteger(doc.meta?.updatedAt) && doc.meta.updatedAt >= doc.meta.createdAt],
      ["bytes", Number.isSafeInteger(doc.bytes) && doc.bytes >= 0],
      ["seq", Number.isSafeInteger(doc.seq) && doc.seq >= 0],
      ["budget-version", Number.isSafeInteger(doc.budgetVersion) && doc.budgetVersion >= 0],
      ["kv-map", doc.kv !== null && typeof doc.kv === "object" && !Array.isArray(doc.kv)],
      ["wire-map", doc.wire !== null && typeof doc.wire === "object" && !Array.isArray(doc.wire)],
    ];
    const failedCheck = documentChecks.find(([, ok]) => !ok);
    if (failedCheck) {
      return Response.json(
        { error: "invalid-room-document", reason: failedCheck[0] }, { status: 422 },
      );
    }
    const logicalName = `${doc.meta.appSlug}|${doc.meta.id}`;
    let expectedCellID;
    try {
      expectedCellID = this.env.ROOMS.idFromName(logicalName).toString();
    } catch {
      return Response.json({ error: "invalid-room-cell-name" }, { status: 422 });
    }
    if (typeof stableCellID !== "string" || expectedCellID !== stableCellID) {
      return Response.json({ error: "room-cell-name-mismatch" }, { status: 422 });
    }
    const keys = Object.keys(doc.kv);
    if (Object.keys(doc.wire).length !== keys.length ||
        keys.some((key) => !Object.hasOwn(doc.wire, key))) {
      return Response.json(
        { error: "invalid-room-document", reason: "wire-key-set" }, { status: 422 },
      );
    }
    let actual = 0;
    for (const [key, value] of Object.entries(doc.kv)) {
      if (typeof value !== "string" || value.length % 4 !== 0 || !BASE64.test(value)) {
        return Response.json(
          { error: "invalid-room-document", reason: "value-base64" }, { status: 422 },
        );
      }
      if (typeof doc.wire[key] !== "string") {
        return Response.json(
          { error: "invalid-room-document", reason: "wire-value" }, { status: 422 },
        );
      }
      try {
        JSON.parse(doc.wire[key]);
      } catch {
        return Response.json(
          { error: "invalid-room-document", reason: "wire-json" }, { status: 422 },
        );
      }
      actual += b64len(value);
      if (!Number.isSafeInteger(actual)) {
        return Response.json(
          { error: "invalid-room-document", reason: "byte-overflow" }, { status: 422 },
        );
      }
    }
    if (actual !== doc.bytes) {
      return Response.json({
        error: "room-byte-mismatch", stored: doc.bytes, actual,
      }, { status: 422 });
    }

    let response;
    let seeded;
    try {
      response = await this.appCall(doc.meta.appSlug, "roomseed", {
        room: doc.meta.id,
        version: doc.budgetVersion,
        targetBytes: actual,
      });
      seeded = await response.json();
    } catch {
      return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
    }
    if (!response.ok) {
      return Response.json({ error: "allocation-seed-conflict", detail: seeded }, {
        status: response.status,
      });
    }
    return Response.json({
      logicalName,
      appSlug: doc.meta.appSlug,
      room: doc.meta.id,
      bytes: actual,
      keyCount: keys.length,
      seq: doc.seq,
      createdAt: doc.meta.createdAt,
      updatedAt: doc.meta.updatedAt,
      budgetVersion: doc.budgetVersion,
      seeded: seeded.seeded,
      total: seeded.total,
    });
  }

  broadcastPut(doc, key) {
    this.broadcast({
      type: "put", seq: doc.seq, key,
      value: Object.hasOwn(doc.wire, key) ? JSON.parse(doc.wire[key]) : null,
    });
  }

  async alarm() {
    await this.state.blockConcurrencyWhile(async () => {
      await this.resumePending(undefined, true);
    });
  }

  async webSocketMessage(ws, message) {
    if (typeof message === "string") {
      try {
        const frame = JSON.parse(message);
        if (frame !== null && typeof frame === "object" &&
            ["snapshot", "put", "delete"].includes(frame.type)) {
          ws.close(1008, "reserved control frame");
          return;
        }
      } catch {
        // Opaque text is an ephemeral frame, not necessarily JSON.
      }
    }
    for (const peer of this.state.getWebSockets()) {
      if (peer === ws) {
        continue;
      }
      try {
        peer.send(message);
      } catch {
        // reaped by the runtime
      }
    }
  }

  async webSocketClose(ws, code, reason, wasClean) {
    // Recorded so a client-side close code can be checked against what the
    // cell believed happened.
    await this.state.storage.put("lastClose", { code, reason, wasClean });
  }
}

// Decoded length of a base64 payload, without decoding it. Room values are
// arbitrary bytes and travel base64-encoded, but every cap is stated in the
// app's OWN bytes, so the encoded length would charge 4/3 of the truth.
function b64len(b64) {
  if (!b64) {
    return 0;
  }
  let pad = 0;
  if (b64.endsWith("==")) {
    pad = 2;
  } else if (b64.endsWith("=")) {
    pad = 1;
  }
  return (b64.length * 3) / 4 - pad;
}

// The PASTE cell: one row, addressed by slug. Separate from the identity cell
// because a reader arrives holding a slug and does not know the owner, while
// the index reader holds an identity and does not know the slugs. No single
// cell can serve both, which is exactly why the create spans two and needs an
// intent to survive a crash between them.
export class Paste {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.pathname.split("/").pop();
    switch (op) {
      case "get":
        return this.get();
      case "versions":
        return this.listVersions();
      case "roomallocation":
        return this.roomAllocation();
    }
    return this.state.blockConcurrencyWhile(async () => {
      switch (op) {
        case "put":
          return this.put(await request.json());
        case "abortcreate":
          return this.abortCreate(await request.json());
        case "status":
          return this.setStatus(await request.json());
        case "rename":
          return this.rename(await request.json());
        case "roomcreated":
          return this.roomCreated(await request.json());
        case "roomcounts":
          return this.roomCounts(url.searchParams);
        case "roompreflight":
          return this.roomPreflight(await request.json());
        case "roomdecide":
          return this.roomDecide(await request.json());
        case "roomseed":
          return this.roomSeed(await request.json());
        case "remove":
          return this.remove(await request.json());
        case "append":
          return this.append(await request.json());
        case "delversion":
          return this.deleteVersion(await request.json());
        case "pin":
          return this.pin(await request.json());
        case "reconcile":
          return this.reconcile(url.searchParams.get("id"));
        default:
          return new Response("unknown op\n", { status: 404 });
      }
    });
  }

  async put(body) {
    if (typeof body.generation !== "string" || !body.generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (typeof body.fingerprint !== "string" || !body.fingerprint || !body.row) {
      return Response.json({ error: "missing-create-fingerprint" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      // The slug namespace is global, and this cell IS the slug, so uniqueness is
      // enforced here or nowhere: an unconditional write would let a second
      // upload silently clobber the first owner's paste.
      const existing = await tx.get("row");
      if (existing) {
        if (existing.generation === body.generation) {
          const fingerprint = await tx.get("createFingerprint");
          if (fingerprint === body.fingerprint) {
            return new Response(null, { status: 204 });
          }
          return Response.json({ error: "create-mismatch" }, { status: 409 });
        }
        return Response.json({ error: "slug-taken" }, { status: 409 });
      }
      if (await tx.get(createAbortKey(body.generation))) {
        return Response.json({ error: "create-aborted" }, { status: 409 });
      }
      if (await tx.get("artifactTombstone")) {
        return Response.json({ error: "slug-taken" }, { status: 409 });
      }
      body.row.generation = body.generation;
      body.row.accountingVersion = 0;
      // v1 is SEEDED into the version list rather than living only on the row.
      // Keeping it off the list made every reader special-case it - the listing
      // omitted it, and the byte total needed a separate baseSize to add it back.
      // One list with every version in it removes both.
      await tx.put(new Map([
        ["row", body.row],
        ["createFingerprint", body.fingerprint],
        ["versions", [{
          ver: 1, kind: body.row.kind, contentSha: body.row.contentSha,
          size: body.row.size, createdAt: body.row.createdAt, deleted: false,
          manifest: body.row.manifest ?? null,
        }]],
        ["maxVer", 1],
      ]));
      return new Response(null, { status: 204 });
    });
  }

  async abortCreate(body) {
    if (typeof body.generation !== "string" || !body.generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const row = await tx.get("row");
      if (row?.generation === body.generation) {
        return Response.json({
          present: true,
          row,
          fingerprint: (await tx.get("createFingerprint")) ?? "",
        });
      }
      await tx.put(createAbortKey(body.generation), true);
      return Response.json({ present: false });
    });
  }

  // --- app-scoped room accounting -------------------------------------------
  //
  // Rooms belong to an app, and the app slug IS this cell, so the creation
  // ledger and the per-app byte total live here rather than in any room. A room
  // cannot answer "how many rooms did this subnet just create" without seeing
  // its siblings, and cells cannot see each other.
  //
  // This works on a cell with NO row: an app's rooms are not gated on the app
  // having been deployed yet.

  async roomCreated(body) {
    const ledger = (await this.state.storage.get("roomLedger")) ?? [];
    if (!ledger.some((entry) => entry.id === body.id)) {
      ledger.push({ id: body.id, subnet: body.subnet, at: body.at });
      await this.state.storage.put("roomLedger", ledger);
    }
    return new Response(null, { status: 204 });
  }

  // Counts and PRUNES in one pass: rows past the window are dropped as they are
  // walked, so the ledger stays bounded by recent activity with no background
  // sweep. Nothing reads a dropped row - it is outside every window a caller
  // can ask about.
  async roomCounts(params) {
    const now = Number(params.get("now"));
    const windowMs = Number(params.get("window"));
    const subnet = params.get("subnet");
    const ledger = (await this.state.storage.get("roomLedger")) ?? [];
    const live = ledger.filter((e) => now - e.at < windowMs);
    if (live.length !== ledger.length) {
      await this.state.storage.put("roomLedger", live);
    }
    return Response.json({
      perSubnet: live.filter((e) => e.subnet === subnet).length,
      perApp: live.length,
    });
  }

  async roomPreflight(body) {
    const total = (await this.state.storage.get("roomAllocated")) ?? 0;
    if (body.appCap > 0 && total >= body.appCap) {
      return Response.json({ error: "app-full", total }, { status: 507 });
    }
    return Response.json({ total });
  }

  async roomAllocation() {
    const total = (await this.state.storage.get("roomAllocated")) ?? 0;
    if (!Number.isSafeInteger(total) || total < 0) {
      return Response.json({ error: "invalid-room-allocation" }, { status: 500 });
    }
    const stored = await this.state.storage.list({ prefix: "roomAllocation:" });
    const allocations = [];
    for (const [key, record] of stored) {
      if (!record || !Number.isSafeInteger(record.version) || record.version < 0 ||
          !Number.isSafeInteger(record.allocated) || record.allocated < 0 ||
          !Number.isSafeInteger(record.target) || record.target < 0) {
        return Response.json({ error: "invalid-room-allocation" }, { status: 500 });
      }
      allocations.push({
        room: key.slice("roomAllocation:".length),
        version: record.version,
        allocated: record.allocated,
        target: record.target,
      });
    }
    allocations.sort((a, b) => a.room.localeCompare(b.room));
    return Response.json({ total, allocations });
  }

  async roomSeed(body) {
    if (typeof body.room !== "string" || body.room === "" ||
        !Number.isSafeInteger(body.version) || body.version < 0 ||
        !Number.isSafeInteger(body.targetBytes) || body.targetBytes < 0) {
      return Response.json({ error: "invalid-allocation-seed" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const key = `roomAllocation:${body.room}`;
      const current = await tx.get(key);
      const total = (await tx.get("roomAllocated")) ?? 0;
      if (!Number.isSafeInteger(total) || total < 0) {
        return Response.json({ error: "invalid-room-allocation" }, { status: 500 });
      }
      if (current !== undefined) {
        if (current.version === body.version &&
            current.allocated === body.targetBytes &&
            current.target === body.targetBytes) {
          return Response.json({
            seeded: false,
            version: current.version,
            allocated: current.allocated,
            total,
          });
        }
        return Response.json({
          error: "allocation-seed-conflict",
          version: current.version,
          allocated: current.allocated,
          target: current.target,
          total,
        }, { status: 409 });
      }
      const nextTotal = total + body.targetBytes;
      if (!Number.isSafeInteger(nextTotal)) {
        return Response.json({ error: "invalid-allocation-seed" }, { status: 400 });
      }
      await tx.put(new Map([
        [key, { version: body.version, allocated: body.targetBytes, target: body.targetBytes }],
        ["roomAllocated", nextTotal],
      ]));
      return Response.json({
        seeded: true,
        version: body.version,
        allocated: body.targetBytes,
        total: nextTotal,
      });
    });
  }

  async roomDecide(body) {
    if (!Number.isSafeInteger(body.version) || body.version < 1 ||
        !Number.isSafeInteger(body.targetBytes) || body.targetBytes < 0) {
      return Response.json({ error: "invalid-budget-decision" }, { status: 400 });
    }
    return this.state.storage.transaction(async (tx) => {
      const key = `roomAllocation:${body.room}`;
      const current = (await tx.get(key)) ?? {
        version: 0, allocated: 0, target: 0,
      };
      const total = (await tx.get("roomAllocated")) ?? 0;

      if (body.version < current.version) {
        return Response.json({
          error: "stale-version", version: current.version,
          allocated: current.allocated, total,
        }, { status: 409 });
      }
      if (body.version === current.version) {
        if (body.targetBytes !== current.target) {
          return Response.json({
            error: "version-target-mismatch", version: current.version,
            allocated: current.allocated, total,
          }, { status: 409 });
        }
        const granted = current.target === current.allocated;
        return Response.json({
          granted, version: current.version,
          allocated: current.allocated, total,
        }, { status: granted ? 200 : 507 });
      }
      if (body.version !== current.version + 1) {
        return Response.json({
          error: "version-gap", version: current.version,
          allocated: current.allocated, total,
        }, { status: 409 });
      }

      const nextTotal = total - current.allocated + body.targetBytes;
      const granted = body.targetBytes <= current.allocated ||
        body.appCap <= 0 || nextTotal <= body.appCap;
      const record = {
        version: body.version,
        allocated: granted ? body.targetBytes : current.allocated,
        target: body.targetBytes,
      };
      const updates = new Map([[key, record]]);
      if (granted && nextTotal !== total) {
        updates.set("roomAllocated", nextTotal);
      }
      await tx.put(updates);
      return Response.json({
        granted, version: record.version, allocated: record.allocated,
        total: granted ? nextTotal : total,
      }, { status: granted ? 200 : 507 });
    });
  }

  async get() {
    const row = await this.state.storage.get("row");
    if (!row) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json(row);
  }

  async reconcile(stableCellID) {
    if (await this.state.storage.get(ARTIFACT_PENDING)) {
      return Response.json({ error: "pending-artifact-operation" }, { status: 409 });
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      const roomLedger = await this.state.storage.get("roomLedger");
      const roomAllocated = await this.state.storage.get("roomAllocated");
      const roomAllocations = await this.state.storage.list({ prefix: "roomAllocation:" });
      if (roomLedger !== undefined || roomAllocated !== undefined || roomAllocations.size > 0) {
        return Response.json({ artifact: false, coordinator: true });
      }
      const tombstone = await this.state.storage.get("artifactTombstone");
      if (tombstone) {
        return Response.json({ artifact: false, tombstone: true, generation: tombstone.generation });
      }
      return new Response("not found\n", { status: 404 });
    }
    const versions = (await this.state.storage.get("versions")) ?? [];
    const validVersions = Array.isArray(versions) && versions.length > 0 &&
      versions.every((version) => version &&
        Number.isSafeInteger(version.ver) && version.ver > 0 &&
        Number.isSafeInteger(version.size) && version.size >= 0 &&
        typeof version.deleted === "boolean");
    const versionNumbers = validVersions ? versions.map((version) => version.ver) : [];
    const latestVersion = versionNumbers.length ? Math.max(...versionNumbers) : 0;
    if (typeof row.slug !== "string" || !row.slug ||
        typeof row.identity !== "string" || !row.identity ||
        !Number.isSafeInteger(row.size) || row.size < 0 ||
        !validVersions || new Set(versionNumbers).size !== versionNumbers.length) {
      return Response.json({ error: "invalid-artifact-document" }, { status: 422 });
    }
    let expectedCellID;
    try {
      expectedCellID = this.env.PASTES.idFromName(row.slug).toString();
    } catch {
      return Response.json({ error: "invalid-paste-cell-name" }, { status: 422 });
    }
    if (typeof stableCellID !== "string" || expectedCellID !== stableCellID) {
      return Response.json({ error: "paste-cell-name-mismatch" }, { status: 422 });
    }
    this.rollServed(row, versions);
    if (!row.generation) {
      row.generation = `legacy:${stableCellID}`;
      row.accountingVersion = 0;
    }
    await this.state.storage.transaction(async (tx) => {
      await tx.put(new Map([["row", row], ["maxVer", latestVersion]]));
    });
    const charge = row.status === "failed" ? 0 : this.liveCharge(versions);
    let response;
    let seed;
    try {
      response = await this.identityCall(row.identity, "artifactseed", {
        slug: row.slug,
        generation: row.generation,
        charge,
        servedSize: row.size,
        status: row.status,
        createdAt: row.createdAt,
        updatedAt: row.updatedAt,
        latestVersion,
        pinnedVersion: row.pinnedVersion ?? 0,
        kind: row.kind,
        name: row.name ?? "",
        contentSha: row.contentSha ?? "",
      });
      seed = await response.json();
    } catch {
      return Response.json({ error: "identity-unavailable" }, { status: 502 });
    }
    if (!response.ok) {
      return Response.json({ error: "artifact-seed-conflict", detail: seed }, { status: response.status });
    }
    row.accountingVersion = seed.version;
    await this.state.storage.put("row", row);
    let projected;
    try {
      projected = await this.identityCall(row.identity, "artifactproject", {
        slug: row.slug,
        generation: row.generation,
        version: seed.version,
        servedSize: row.size,
        status: row.status,
        kind: row.kind,
        name: row.name ?? "",
        contentSha: row.contentSha ?? "",
        latestVersion,
        pinnedVersion: row.pinnedVersion ?? 0,
        updatedAt: row.updatedAt ?? row.createdAt ?? 0,
      });
    } catch {
      return Response.json({ error: "identity-unavailable" }, { status: 502 });
    }
    if (!projected.ok) {
      return Response.json({ error: "artifact-projection-conflict" }, { status: projected.status });
    }
    return Response.json({
      artifact: true,
      logicalName: row.slug,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      charge,
      servedSize: row.size,
      accountingVersion: seed.version,
      status: row.status,
      kind: row.kind,
      name: row.name ?? "",
      contentSha: row.contentSha ?? "",
      latestVersion,
      pinnedVersion: row.pinnedVersion ?? 0,
      updatedAt: row.updatedAt ?? row.createdAt ?? 0,
      seeded: seed.seeded,
    });
  }

  // Guarded by owner and creation time, so a rename cannot land on a slug that
  // was deleted and re-minted by someone else in between.
  async rename(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ changed: false, reason: "absent" });
    }
    if (row.identity !== body.identity || row.createdAt !== body.createdAt) {
      return Response.json({ changed: false, reason: "not-owner" });
    }
    row.name = body.name;
    if (!await this.queueProjection(row, ["name"])) {
      return Response.json({ changed: false, reason: "projection-unavailable" }, { status: 502 });
    }
    return Response.json({ changed: true });
  }

  identityCell(owner) {
    return this.env.IDENTITY.get(this.env.IDENTITY.idFromName(owner));
  }

  async identityCall(owner, op, body) {
    return this.identityCell(owner).fetch(new Request(`https://cell/identity/${op}?scope=${encodeURIComponent(owner)}`, {
      method: "POST",
      body: JSON.stringify(body),
      headers: { "content-type": "application/json" },
    }));
  }

  receiptKey(generation, opId) {
    return `${ARTIFACT_RECEIPT_PREFIX}${generation}:${opId}`;
  }

  receiptResponse(receipt) {
    return Response.json(receipt.body, { status: receipt.status });
  }

  liveCharge(versions) {
    return versions.reduce((sum, version) => version.deleted ? sum : sum + (version.size ?? 0), 0);
  }

  async retryArtifactPending() {
    await this.state.storage.setAlarm(Date.now() + 1000);
    return { outcome: "unavailable" };
  }

  artifactProjection(row, pending) {
    return {
      slug: pending.slug,
      generation: pending.generation,
      version: pending.version,
      servedSize: row.size ?? 0,
      status: row.status,
      kind: row.kind,
      name: row.name ?? "",
      contentSha: row.contentSha ?? "",
      latestVersion: pending.latestVersion,
      pinnedVersion: row.pinnedVersion ?? 0,
      updatedAt: row.updatedAt ?? row.createdAt ?? 0,
    };
  }

  async clearArtifactPending() {
    await this.state.storage.transaction(async (tx) => {
      await tx.delete(ARTIFACT_PENDING);
      await tx.deleteAlarm();
    });
  }

  async resumeArtifactPending(rearm = false) {
    let pending = await this.state.storage.get(ARTIFACT_PENDING);
    if (!pending) {
      return { outcome: "complete" };
    }
    if (rearm) {
      await this.state.storage.setAlarm(Date.now());
    }

    if (pending.stage === "decide") {
      let response;
      let decision;
      try {
        response = await this.identityCall(pending.owner, "artifactdecide", {
          slug: pending.slug,
          generation: pending.generation,
          version: pending.version,
          target: pending.target,
          userCap: pending.userCap,
        });
        decision = await response.json();
      } catch {
        return this.retryArtifactPending();
      }

      if (["fail", "remove"].includes(pending.kind) && response.status === 404 &&
          decision.error === "artifact-absent") {
        const settled = await this.state.storage.transaction(async (tx) => {
          const current = await tx.get(ARTIFACT_PENDING);
          if (!current || current.opId !== pending.opId ||
              current.generation !== pending.generation || current.stage !== "decide") {
            return false;
          }
          if (pending.kind === "remove") {
            const tombstone = await tx.get("artifactTombstone");
            if (!tombstone || tombstone.generation !== pending.generation) {
              return false;
            }
            await tx.delete("artifactTombstone");
          } else {
            const row = await tx.get("row");
            if (row?.generation === pending.generation) {
              row.accountingVersion = pending.version;
              await tx.put("row", row);
            }
          }
          await tx.delete(ARTIFACT_PENDING);
          await tx.deleteAlarm();
          return true;
        });
        return settled ? { outcome: "complete" } : this.retryArtifactPending();
      }
      if (response.status === 507 && decision.granted === false &&
          decision.version === pending.version) {
        const receipt = { status: 507, body: { error: "over-quota" } };
        await this.state.storage.transaction(async (tx) => {
          const row = await tx.get("row");
          if (row) {
            row.accountingVersion = pending.version;
            await tx.put("row", row);
          }
          await tx.put(this.receiptKey(pending.generation, pending.opId), receipt);
          await tx.delete(ARTIFACT_PENDING);
          await tx.deleteAlarm();
        });
        return { outcome: "refused", receipt };
      }
      if (!response.ok || decision.granted !== true ||
          decision.version !== pending.version || decision.allocated !== pending.target) {
        await this.state.storage.setAlarm(Date.now() + 1000);
        return { outcome: "conflict", status: response.status };
      }

      if (pending.kind === "append") {
        await this.state.storage.transaction(async (tx) => {
          const current = await tx.get(ARTIFACT_PENDING);
          if (!current || current.opId !== pending.opId || current.stage !== "decide") {
            return;
          }
          const row = await tx.get("row");
          const versions = (await tx.get("versions")) ?? [];
          versions.push(pending.mutation);
          row.updatedAt = pending.mutation.createdAt ?? row.updatedAt;
          row.accountingVersion = pending.version;
          this.rollServed(row, versions);
          const receipt = {
            status: 200,
            body: {
              appended: true,
              ver: pending.mutation.ver,
              wasPinned: pending.wasPinned,
              totalSize: pending.target,
            },
          };
          pending.stage = "project";
          await tx.put(new Map([
            ["row", row],
            ["versions", versions],
            ["maxVer", pending.mutation.ver],
            [ARTIFACT_PENDING, pending],
            [this.receiptKey(pending.generation, pending.opId), receipt],
          ]));
        });
      } else {
        await this.state.storage.transaction(async (tx) => {
          const row = await tx.get("row");
          if (row) {
            row.accountingVersion = pending.version;
            await tx.put("row", row);
          }
          pending.stage = ["remove", "fail"].includes(pending.kind) ? "drop" : "project";
          await tx.put(ARTIFACT_PENDING, pending);
        });
      }
      pending = await this.state.storage.get(ARTIFACT_PENDING);
    }

    if (pending.stage === "project") {
      const row = await this.state.storage.get("row");
      if (!row) {
        return this.retryArtifactPending();
      }
      let response;
      try {
        response = await this.identityCall(pending.owner, "artifactproject",
          this.artifactProjection(row, pending));
      } catch {
        return this.retryArtifactPending();
      }
      if (!response.ok) {
        await this.state.storage.setAlarm(Date.now() + 1000);
        return { outcome: "conflict", status: response.status };
      }
      await this.clearArtifactPending();
      return { outcome: "complete" };
    }

    if (pending.stage === "drop") {
      let response;
      try {
        response = await this.identityCall(pending.owner, "artifactdrop", {
          slug: pending.slug,
          generation: pending.generation,
          version: pending.version,
        });
      } catch {
        return this.retryArtifactPending();
      }
      if (!response.ok) {
        await this.state.storage.setAlarm(Date.now() + 1000);
        return { outcome: "conflict", status: response.status };
      }
      await this.state.storage.transaction(async (tx) => {
        const current = await tx.get(ARTIFACT_PENDING);
        if (!current || current.opId !== pending.opId ||
            current.generation !== pending.generation || current.stage !== "drop") {
          return;
        }
        if (pending.kind === "remove") {
          const tombstone = await tx.get("artifactTombstone");
          if (!tombstone || tombstone.generation !== pending.generation) {
            return;
          }
          await tx.delete("artifactTombstone");
        }
        await tx.delete(ARTIFACT_PENDING);
        await tx.deleteAlarm();
      });
      return (await this.state.storage.get(ARTIFACT_PENDING))
        ? this.retryArtifactPending()
        : { outcome: "complete" };
    }

    return this.retryArtifactPending();
  }

  async queueProjection(row, fields) {
    const desired = Object.fromEntries(fields.map((field) => [field, row[field]]));
    const prior = await this.state.storage.get(ARTIFACT_PENDING);
    if (prior) {
      await this.resumeArtifactPending();
      if (await this.state.storage.get(ARTIFACT_PENDING)) {
        return false;
      }
      const recovered = await this.state.storage.get("row");
      if (!recovered || recovered.generation !== row.generation) {
        return false;
      }
      Object.assign(recovered, desired);
      row = recovered;
    }
    if (!row.generation) {
      return false;
    }
    const pending = {
      kind: "projection",
      stage: "project",
      opId: "",
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: row.accountingVersion ?? 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.put(new Map([["row", row], [ARTIFACT_PENDING, pending]]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return true;
  }

  async mutationReceipt(generation, opId) {
    if (typeof generation !== "string" || !generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (typeof opId !== "string" || !opId) {
      return Response.json({ error: "missing-operation-id" }, { status: 400 });
    }
    const receipt = await this.state.storage.get(this.receiptKey(generation, opId));
    if (receipt) {
      return this.receiptResponse(receipt);
    }
    const pending = await this.state.storage.get(ARTIFACT_PENDING);
    if (pending) {
      if (pending.generation !== generation || pending.opId !== opId) {
        return Response.json({ error: "artifact-operation-pending" }, { status: 502 });
      }
      await this.resumeArtifactPending();
      const recovered = await this.state.storage.get(this.receiptKey(generation, opId));
      if (recovered) {
        return this.receiptResponse(recovered);
      }
      return Response.json({ error: "artifact-operation-pending" }, { status: 502 });
    }
    return null;
  }

  // Growth reserves owner capacity before publishing a retained version.
  async append(body) {
    const prior = await this.mutationReceipt(body.generation, body.opId);
    if (prior) {
      return prior;
    }
    if (!Number.isSafeInteger(body.size) || body.size < 0) {
      return Response.json({ error: "invalid-size" }, { status: 400 });
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ appended: false, reason: "absent" });
    }
    if (!row.generation) {
      return Response.json({ error: "artifact-unreconciled" }, { status: 409 });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    const versions = (await this.state.storage.get("versions")) ?? [];
    const nextVer = ((await this.state.storage.get("maxVer")) ?? 1) + 1;
    const target = this.liveCharge(versions) + body.size;
    const pending = {
      kind: "append",
      stage: "decide",
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: body.userCap ?? 0,
      latestVersion: nextVer,
      wasPinned: (row.pinnedVersion ?? 0) !== 0,
      mutation: {
        ver: nextVer,
        kind: body.kind,
        contentSha: body.contentSha,
        size: body.size,
        createdAt: body.now,
        deleted: false,
        manifest: body.manifest ?? null,
      },
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.put(ARTIFACT_PENDING, pending);
      await tx.setAlarm(Date.now());
    });
    const result = await this.resumeArtifactPending();
    const receipt = await this.state.storage.get(this.receiptKey(body.generation, body.opId));
    if (receipt) {
      return this.receiptResponse(receipt);
    }
    if (result.outcome === "conflict") {
      return Response.json({ error: "artifact-accounting-conflict" }, { status: result.status });
    }
    return Response.json({ error: "artifact-accounting-unavailable" }, { status: 502 });
  }

  // NEWEST FIRST, which is the order every reader wants and none should have to
  // impose: a listing sorted by insertion leaks the storage order into the UI.
  async listVersions() {
    const versions = (await this.state.storage.get("versions")) ?? [];
    return Response.json([...versions].sort((a, b) => b.ver - a.ver));
  }

  // Shrink publishes the tombstone before releasing owner capacity.
  async deleteVersion(body) {
    const prior = await this.mutationReceipt(body.generation, body.opId);
    if (prior) {
      return prior;
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ deleted: false, reason: "absent" });
    }
    if (!row.generation) {
      return Response.json({ error: "artifact-unreconciled" }, { status: 409 });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    const versions = (await this.state.storage.get("versions")) ?? [];
    const version = versions.find((candidate) => candidate.ver === body.ver);
    if (!version) {
      return Response.json({ deleted: false, reason: "absent" });
    }
    if (version.deleted) {
      return Response.json({ deleted: true, totalSize: this.liveCharge(versions) });
    }
    const live = versions.filter((candidate) => !candidate.deleted);
    const served = row.pinnedVersion ||
      live.reduce((latest, candidate) => Math.max(latest, candidate.ver), 0);
    if (body.ver === served) {
      return Response.json({ error: "version-served" }, { status: 409 });
    }

    version.deleted = true;
    if (row.pinnedVersion === body.ver) {
      row.pinnedVersion = 0;
    }
    this.rollServed(row, versions);
    const target = this.liveCharge(versions);
    const receipt = { status: 200, body: { deleted: true, totalSize: target } };
    const pending = {
      kind: "deleteVersion",
      stage: "decide",
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.put(new Map([
        ["versions", versions],
        ["row", row],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  // Roll the row onto the version the public URL serves: the pin when set,
  // otherwise the newest live one. The row's kind, sha, manifest and size are a
  // VIEW of that version, so every mutation that can change which version is
  // served has to pass through here or the view goes stale - which is how a pin
  // ended up moving the marker without moving the content.
  //
  // row.size is the SERVED version's size, NOT the sum of live versions. The
  // quota total is a different number, kept in the identity cell, and conflating
  // them makes a multi-version paste report its whole history as its size.
  rollServed(row, versions) {
    const live = versions.filter((v) => !v.deleted);
    if (!live.length) {
      row.contentSha = "";
      row.manifest = null;
      row.size = 0;
      row.pinnedVersion = 0;
      return;
    }
    const pinned = row.pinnedVersion
      ? live.find((v) => v.ver === row.pinnedVersion)
      : null;
    if (row.pinnedVersion && !pinned) {
      row.pinnedVersion = 0;
    }
    const v = pinned ?? live.reduce((a, b) => (b.ver > a.ver ? b : a));
    row.contentSha = v.contentSha;
    row.kind = v.kind;
    row.manifest = v.manifest ?? null;
    row.size = v.size;
  }

  // Pinning keeps the charge unchanged but advances its fence before projection.
  async pin(body) {
    const prior = await this.mutationReceipt(body.generation, body.opId);
    if (prior) {
      return prior;
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ pinned: false, reason: "absent" });
    }
    if (!row.generation) {
      return Response.json({ error: "artifact-unreconciled" }, { status: 409 });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    const versions = (await this.state.storage.get("versions")) ?? [];
    const wanted = body.ver ?? 0;
    if (wanted !== 0 && !versions.some((version) => version.ver === wanted && !version.deleted)) {
      return Response.json({ pinned: false, reason: "absent" });
    }
    row.pinnedVersion = wanted;
    this.rollServed(row, versions);
    const target = this.liveCharge(versions);
    const receipt = { status: 200, body: { pinned: true, ver: wanted } };
    const pending = {
      kind: "pin",
      stage: "decide",
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.put(new Map([
        ["row", row],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  // Deletion tombstones the incarnation before its allocation is released.
  async remove(body) {
    const prior = await this.mutationReceipt(body.generation, body.opId);
    if (prior) {
      return prior;
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ removed: false, reason: "absent" });
    }
    if (row.identity !== body.identity || row.createdAt !== body.createdAt) {
      return Response.json({ removed: false, reason: "not-owner" });
    }
    if (!row.generation) {
      return Response.json({ error: "artifact-unreconciled" }, { status: 409 });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    const receipt = { status: 200, body: { removed: true } };
    const version = (row.accountingVersion ?? 0) + 1;
    const pending = {
      kind: "remove",
      stage: "decide",
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version,
      target: 0,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.delete("row");
      await tx.put(new Map([
        ["artifactTombstone", {
          slug: row.slug,
          identity: row.identity,
          createdAt: row.createdAt,
          generation: row.generation,
          accountingVersion: version,
        }],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  async fail(body) {
    const prior = await this.mutationReceipt(body.generation, body.opId);
    if (prior) {
      return prior;
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ changed: false, reason: "absent" });
    }
    if (!row.generation) {
      return Response.json({ error: "artifact-unreconciled" }, { status: 409 });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    if (!["pending", "failed"].includes(row.status)) {
      return Response.json({ changed: false, reason: "not-pending", status: row.status });
    }
    row.status = "failed";
    const receipt = { status: 200, body: { changed: true, status: "failed" } };
    const pending = {
      kind: "fail",
      stage: "decide",
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target: 0,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    };
    await this.state.storage.transaction(async (tx) => {
      await tx.put(new Map([
        ["row", row],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  async alarm() {
    await this.state.blockConcurrencyWhile(async () => {
      await this.resumeArtifactPending(true);
    });
  }

  // Only a still-PENDING row transitions, so a late finalizer cannot resurrect
  // a paste the reconciler already failed, and a repeat is harmless.
  async setStatus(body) {
    if (typeof body.generation !== "string" || !body.generation) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (body.status === "failed") {
      return this.fail(body);
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ changed: false, reason: "absent" });
    }
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    if (row.status !== "pending") {
      return Response.json({ changed: false, reason: "not-pending", status: row.status });
    }
    row.status = body.status;
    if (!await this.queueProjection(row, ["status"])) {
      return Response.json({ changed: false, reason: "projection-unavailable" }, { status: 502 });
    }
    return Response.json({ changed: true, status: row.status });
  }
}

// Deprecated, retained only so the v1 migration that created it stays
// resolvable. Superseded by Identity, which co-locates intents with the quota
// and index they must agree with. Nothing routes here.
export class IntentLog {
  async fetch() {
    return new Response("IntentLog is superseded by Identity\n", { status: 410 });
  }
}

// A SUBNET cell: the Sybil admission rows for one network.
//
// The subnet is the rate-limit unit, so it is the cell. Mutating and pruning
// decisions run under blockConcurrencyWhile because v0.4 may serve several
// fetches concurrently.
export class Subnet {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.pathname.split("/").pop();
    return this.state.blockConcurrencyWhile(async () => {
      const body = request.method === "POST" ? await request.json() : {};
      switch (op) {
        case "admit":
          return this.admit(body);
        case "snapshot":
          return this.snapshot(Number(url.searchParams.get("now")),
            Number(url.searchParams.get("window")));
        default:
          return new Response("unknown op\n", { status: 404 });
      }
    });
  }

  // Rows outside the window are dropped as they are walked past: nothing reads
  // them again, because no admission decision can turn on a row the window has
  // already excluded.
  async live(now, windowMs) {
    const rows = (await this.state.storage.get("rows")) ?? {};
    let changed = false;
    for (const [id, firstSeen] of Object.entries(rows)) {
      if (now - firstSeen >= windowMs) {
        delete rows[id];
        changed = true;
      }
    }
    if (changed) {
      await this.state.storage.put("rows", rows);
    }
    return rows;
  }

  async admit(body) {
    const rows = await this.live(body.now, body.window);
    if (Object.hasOwn(rows, body.identity)) {
      // Already on file: no rate-limit accounting, and the first-seen stamp
      // must not move or a returning key would refresh its own window.
      return Response.json({ knownAlready: true, admitted: true });
    }
    if (Object.keys(rows).length >= body.limit) {
      return Response.json({ knownAlready: false, admitted: false });
    }
    rows[body.identity] = body.now;
    await this.state.storage.put("rows", rows);
    return Response.json({ knownAlready: false, admitted: true });
  }

  async snapshot(now, windowMs) {
    const rows = await this.live(now, windowMs);
    const stamps = Object.values(rows);
    return Response.json({
      freshCount: stamps.length,
      oldestFirstSeen: stamps.length ? Math.min(...stamps) : 0,
    });
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/admin/room/reconcile") {
      const id = url.searchParams.get("id");
      if (!id) {
        return new Response("cell id required\n", { status: 400 });
      }
      try {
        return env.ROOMS.get(env.ROOMS.idFromString(id)).fetch(request);
      } catch {
        return new Response("invalid cell id\n", { status: 400 });
      }
    }
    if (url.pathname === "/admin/paste/reconcile") {
      const id = url.searchParams.get("id");
      if (!id) {
        return new Response("cell id required\n", { status: 400 });
      }
      try {
        return env.PASTES.get(env.PASTES.idFromString(id)).fetch(request);
      } catch {
        return new Response("invalid cell id\n", { status: 400 });
      }
    }
    if (url.pathname === "/admin/identity/allocation") {
      const owner = url.searchParams.get("owner");
      if (!owner) {
        return new Response("owner required\n", { status: 400 });
      }
      return env.IDENTITY.get(env.IDENTITY.idFromName(owner)).fetch(
        new Request(`https://cell/identity/allocations?scope=${encodeURIComponent(owner)}`),
      );
    }
    if (url.pathname === "/admin/app/allocation") {
      const slug = url.searchParams.get("slug");
      if (!slug) {
        return new Response("slug required\n", { status: 400 });
      }
      return env.PASTES.get(env.PASTES.idFromName(slug)).fetch(
        new Request(`https://cell/paste/roomallocation?slug=${encodeURIComponent(slug)}`),
      );
    }
    if (url.pathname === "/healthz") {
      return new Response("ok\n");
    }
    if (url.pathname.startsWith("/room/")) {
      const room = url.searchParams.get("room");
      if (!room) {
        return new Response("room required\n", { status: 400 });
      }
      return env.ROOMS.get(env.ROOMS.idFromName(room)).fetch(request);
    }
    if (url.pathname.startsWith("/subnet/")) {
      const subnet = url.searchParams.get("subnet");
      if (!subnet) {
        return new Response("subnet required\n", { status: 400 });
      }
      return env.SUBNETS.get(env.SUBNETS.idFromName(subnet)).fetch(request);
    }
    if (url.pathname.startsWith("/paste/")) {
      const slug = url.searchParams.get("slug");
      if (!slug) {
        return new Response("slug required\n", { status: 400 });
      }
      return env.PASTES.get(env.PASTES.idFromName(slug)).fetch(request);
    }
    if (!url.pathname.startsWith("/intents/") && !url.pathname.startsWith("/identity/")) {
      return new Response("not found\n", { status: 404 });
    }
    const scope = url.searchParams.get("scope");
    if (!scope) {
      return new Response("scope required\n", { status: 400 });
    }
    // One owner maps deterministically to one cell holding intents, quota, and index.
    return env.IDENTITY.get(env.IDENTITY.idFromName(scope)).fetch(request);
  },
};
