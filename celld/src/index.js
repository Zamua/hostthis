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

// The IDENTITY cell. It owns the three things that must agree about one owner:
// outstanding intents, the quota reservation, and the paste index.
//
// Co-locating them is what makes the create sequence work. A cell is
// single-threaded, so "check quota, reserve it, record the intent" is ONE
// serialized event with no interleaving. If the intent lived in its own cell it
// would be a third cross-cell write atomic with neither side, which adds a
// coordination problem instead of solving one.
export class Identity {
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
      case "reserve":
        return this.reserve(await request.json());
      case "release":
        return this.release(await request.json());
      case "confirm":
        return this.confirm(await request.json());
      case "bytes":
        return this.bytes();
      case "list":
        return this.list();
      case "firstSeen":
        return this.firstSeen();
      case "drop":
        return this.drop(await request.json());
      case "touch":
        return this.touch(await request.json());
      case "notesubnet":
        return this.noteSubnet(await request.json());
      case "subnets":
        return this.subnets(Number(new URL(request.url).searchParams.get("now")),
          Number(new URL(request.url).searchParams.get("window")));
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

  // Step 1 of a create, and the reason the intent lives here: the quota check,
  // the reservation and the intent record are one event on one thread, so a
  // concurrent upload by the same owner cannot interleave between them. That is
  // what makes the per-identity cap exact rather than best-effort.
  // updatedAt is recorded from the paste itself, not from the clock: the owner
  // listing is ordered by it, and a paste created with an explicit UpdatedAt
  // must sort where that value says, not where its insertion happened to fall.
  async reserve(body) {
    if (typeof body.size !== "number" || body.size < 0) {
      return Response.json({ error: "negative-size" }, { status: 400 });
    }
    const entries = (await this.state.storage.get("entries")) ?? {};
    const active = Object.values(entries).reduce((n, e) => n + (e.size ?? 0), 0);
    if (body.userCap > 0 && active + body.size > body.userCap) {
      return Response.json({ error: "over-quota", active }, { status: 409 });
    }
    // A denormalised SUMMARY, not a pointer. Answering ListByOwner by fetching
    // each paste cell would be N cross-cell reads on a request path; the
    // identity cell therefore carries what a listing needs, written once here.
    entries[body.slug] = {
      size: body.size,
      status: body.status ?? "pending",
      at: body.now ?? 0,
      updatedAt: body.updatedAt ?? body.now ?? 0,
      // Denormalised so the owner listing needs no fan-out over paste cells:
      // one entry per slug already, and the version number rides along with it.
      latestVersion: 1,
      kind: body.kind ?? "",
      name: body.name ?? "",
      contentSha: body.contentSha ?? "",
    };
    const firstSeen = await this.state.storage.get("firstSeen");
    if (firstSeen === undefined || firstSeen === null) {
      await this.state.storage.put("firstSeen", body.now ?? 0);
    }
    await this.state.storage.put("entries", entries);
    if (body.intent) {
      await this.state.storage.put(intentKey(body.intent.id), toRow(body.intent));
    }
    return Response.json({ active: active + body.size });
  }

  // Step 3: the row landed, so the entry is real and the intent is discharged.
  async confirm(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    if (entries[body.slug]) {
      entries[body.slug].status = body.status ?? "ready";
      await this.state.storage.put("entries", entries);
    }
    if (body.intentId) {
      await this.state.storage.delete(intentKey(body.intentId));
    }
    return new Response(null, { status: 204 });
  }

  // Compensation: the create failed, so the owner stops being charged. Dropping
  // the entry rather than marking it is deliberate - a failed paste charges
  // nothing, and the row itself stays in the paste cell to serve an error.
  async release(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    delete entries[body.slug];
    await this.state.storage.put("entries", entries);
    if (body.intentId) {
      await this.state.storage.delete(intentKey(body.intentId));
    }
    return new Response(null, { status: 204 });
  }

  // A point read of a maintained aggregate, not a scan: the identity cell
  // already holds every entry it charges for.
  async bytes() {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const total = Object.values(entries).reduce((n, e) => n + (e.size ?? 0), 0);
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
    const out = Object.entries(entries).map(([slug, e]) => ({ slug, ...e }));
    // MOST RECENTLY UPDATED FIRST. Falls back to creation time for an entry
    // never updated, so a fresh owner still lists in a stable order.
    out.sort((a, b) => ((b.updatedAt ?? b.at ?? 0) - (a.updatedAt ?? a.at ?? 0)));
    return Response.json(out);
  }

  async firstSeen() {
    return Response.json({ firstSeen: (await this.state.storage.get("firstSeen")) ?? 0 });
  }

  // Removes an index entry whose paste no longer exists. The caller has already
  // established the absence; the cell only owns the index.
  async drop(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const had = Object.hasOwn(entries, body.slug);
    if (had) {
      delete entries[body.slug];
      await this.state.storage.put("entries", entries);
    }
    return Response.json({ dropped: had });
  }

  // Keeps the denormalised summary in step with a mutation of the paste row.
  // Denormalising anything MUTABLE makes every mutation a two-cell write; the
  // alternative is a listing that serves stale contents, and the shale backend
  // does not, so neither may this one.
  async touch(body) {
    const entries = (await this.state.storage.get("entries")) ?? {};
    const e = entries[body.slug];
    if (!e) {
      return Response.json({ updated: false });
    }
    if (body.at !== undefined && body.at !== null) {
      e.updatedAt = body.at;
    }
    for (const k of ["name", "status", "size", "kind", "latestVersion", "pinnedVersion"]) {
      if (body[k] !== undefined && body[k] !== null) {
        e[k] = body[k];
      }
    }
    await this.state.storage.put("entries", entries);
    return Response.json({ updated: true });
  }

  // The reverse keygate index. Without it, "how many subnets is this key on"
  // would have to visit every subnet cell - the fan-out the shale adapter
  // avoids with an identity-sharded index for exactly the same reason.
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


// A room cell: hibernatable WebSocket fan-out, which is what hostthis rooms
// need and the part of celld with the thinnest upstream test coverage (their
// limitations page flags close codes and cross-node reconnection).
//
// Hibernatable sockets are INBOUND and accepted by the runtime, so the cell can
// be evicted while clients stay connected. That is the opposite of an OUTBOUND
// Durable Object socket, which upstream says does not survive a cell move.
// A ROOM cell: one app room's key/value state and its live sockets.
//
// The cell IS the room, so the dense per-room sequence and the per-room caps
// need no coordination: a cell handles one event at a time, which is exactly
// the serialization a "+1 per committed mutation" counter requires. On a
// sharded store the same guarantee costs a CAS per write.
//
// The per-APP cap cannot live here, because the app spans rooms. The caller
// passes the app's OTHER rooms' bytes so both caps are still decided in one
// event; that figure can be stale under concurrent writes to sibling rooms,
// which is a bounded overshoot on the app cap only. The per-ROOM cap stays
// exact.
export class Room {
  constructor(state) {
    this.state = state;
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
    }
    if (request.headers.get("Upgrade") !== "websocket") {
      return new Response("expected websocket\n", { status: 426 });
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    // acceptWebSocket, not server.accept(): the hibernatable form is what lets
    // the runtime evict the cell while the socket stays open.
    this.state.acceptWebSocket(server);
    return new Response(null, { status: 101, webSocket: client });
  }

  async create(body) {
    if (await this.state.storage.get("meta")) {
      return Response.json({ created: false });
    }
    await this.state.storage.put("meta", {
      appSlug: body.appSlug, id: body.id,
      createdAt: body.createdAt, updatedAt: body.updatedAt,
    });
    return Response.json({ created: true });
  }

  async meta() {
    const meta = await this.state.storage.get("meta");
    if (!meta) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json(meta);
  }

  async getValue(key) {
    if (!(await this.state.storage.get("meta"))) {
      return new Response("not found\n", { status: 404 });
    }
    const kv = (await this.state.storage.get("kv")) ?? {};
    if (!Object.hasOwn(kv, key)) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ value: kv[key] });
  }

  async scan() {
    if (!(await this.state.storage.get("meta"))) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({
      values: (await this.state.storage.get("kv")) ?? {},
      seq: (await this.state.storage.get("seq")) ?? 0,
    });
  }

  // Both caps are decided BEFORE anything is written, so a rejected write
  // leaves the prior state exactly as it was - which is the contract callers
  // rely on to retry.
  async put(body) {
    const meta = await this.state.storage.get("meta");
    if (!meta) {
      return new Response("not found\n", { status: 404 });
    }
    const kv = (await this.state.storage.get("kv")) ?? {};
    const bytes = (await this.state.storage.get("bytes")) ?? 0;
    const size = b64len(body.value);
    const prior = Object.hasOwn(kv, body.key) ? b64len(kv[body.key]) : 0;
    const next = bytes - prior + size;

    if (!Object.hasOwn(kv, body.key) && body.keyCap > 0 &&
        Object.keys(kv).length >= body.keyCap) {
      return Response.json({ error: "room-full" }, { status: 413 });
    }
    if (body.roomCap > 0 && next > body.roomCap) {
      return Response.json({ error: "room-full" }, { status: 413 });
    }
    if (body.appCap > 0 && body.otherBytes + next > body.appCap) {
      return Response.json({ error: "app-full" }, { status: 507 });
    }

    kv[body.key] = body.value;
    const seq = ((await this.state.storage.get("seq")) ?? 0) + 1;
    meta.updatedAt = body.now;
    await this.state.storage.put("kv", kv);
    await this.state.storage.put("bytes", next);
    await this.state.storage.put("seq", seq);
    await this.state.storage.put("meta", meta);
    return Response.json({ seq, bytes: next });
  }

  // An absent key still commits and still consumes a sequence number: a client
  // splicing a live stream onto a snapshot reads a skipped seq as a lost frame,
  // so a silent no-op here would look like data loss downstream.
  async del(body) {
    const meta = await this.state.storage.get("meta");
    if (!meta) {
      return new Response("not found\n", { status: 404 });
    }
    const kv = (await this.state.storage.get("kv")) ?? {};
    let bytes = (await this.state.storage.get("bytes")) ?? 0;
    if (Object.hasOwn(kv, body.key)) {
      bytes -= b64len(kv[body.key]);
      delete kv[body.key];
    }
    const seq = ((await this.state.storage.get("seq")) ?? 0) + 1;
    meta.updatedAt = body.now;
    await this.state.storage.put("kv", kv);
    await this.state.storage.put("bytes", bytes);
    await this.state.storage.put("seq", seq);
    await this.state.storage.put("meta", meta);
    return Response.json({ seq, bytes });
  }

  async webSocketMessage(ws, message) {
    const seen = ((await this.state.storage.get("seen")) ?? 0) + 1;
    await this.state.storage.put("seen", seen);
    ws.send(JSON.stringify({ echo: String(message), seen }));
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
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const op = new URL(request.url).pathname.split("/").pop();
    switch (op) {
      case "put":
        return this.put(await request.json());
      case "get":
        return this.get();
      case "status":
        return this.setStatus(await request.json());
      case "rename":
        return this.rename(await request.json());
      case "roomcreated":
        return this.roomCreated(await request.json());
      case "roomcounts":
        return this.roomCounts(new URL(request.url).searchParams);
      case "roomothers":
        return this.roomOthers(new URL(request.url).searchParams.get("room"));
      case "roomsettle":
        return this.roomSettle(await request.json());
      case "claim":
        return this.claim(await request.json());
      case "unclaim":
        return this.unclaim(await request.json());
      case "remove":
        return this.remove(await request.json());
      case "append":
        return this.append(await request.json());
      case "versions":
        return this.listVersions();
      case "delversion":
        return this.deleteVersion(await request.json());
      case "pin":
        return this.pin(await request.json());
      default:
        return new Response("unknown op\n", { status: 404 });
    }
  }

  async put(body) {
    // The slug namespace is global, and this cell IS the slug, so uniqueness is
    // enforced here or nowhere: an unconditional write would let a second
    // upload silently clobber the first owner's paste.
    if (await this.state.storage.get("row")) {
      return Response.json({ error: "slug-taken" }, { status: 409 });
    }
    const claim = await this.state.storage.get("claim");
    if (claim && claim.owner !== body.row.identity) {
      return Response.json({ error: "slug-taken" }, { status: 409 });
    }
    // v1 is SEEDED into the version list rather than living only on the row.
    // Keeping it off the list made every reader special-case it - the listing
    // omitted it, and the byte total needed a separate baseSize to add it back.
    // One list with every version in it removes both.
    await this.state.storage.put("row", body.row);
    await this.state.storage.put("versions", [{
      ver: 1, kind: body.row.kind, contentSha: body.row.contentSha,
      size: body.row.size, createdAt: body.row.createdAt, deleted: false,
      manifest: body.row.manifest ?? null,
    }]);
    await this.state.storage.put("maxVer", 1);
    if (claim) {
      await this.state.storage.delete("claim");
    }
    return new Response(null, { status: 204 });
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
    ledger.push({ id: body.id, subnet: body.subnet, at: body.at });
    await this.state.storage.put("roomLedger", ledger);
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

  // The app's room bytes EXCLUDING one room, which is what that room needs to
  // decide the app cap inside its own single event.
  async roomOthers(roomID) {
    const bytes = (await this.state.storage.get("roomBytes")) ?? {};
    let total = 0;
    for (const [id, n] of Object.entries(bytes)) {
      if (id !== roomID) {
        total += n;
      }
    }
    return Response.json({ otherBytes: total });
  }

  // ABSOLUTE per room, never a delta: re-running a settle with the same total
  // is a no-op, where "add n" would double-charge a retry. The same reason the
  // identity cell charges pastes absolutely.
  async roomSettle(body) {
    const bytes = (await this.state.storage.get("roomBytes")) ?? {};
    if (bytes[body.room] === body.bytes) {
      return new Response(null, { status: 204 });
    }
    bytes[body.room] = body.bytes;
    await this.state.storage.put("roomBytes", bytes);
    return new Response(null, { status: 204 });
  }

  // A slug held before its content exists, so a multi-file deploy can stage
  // against the slug it will commit under. Same-owner re-claim is idempotent:
  // a retry must not turn into a collision with itself.
  async claim(body) {
    if (await this.state.storage.get("row")) {
      return Response.json({ error: "slug-taken" }, { status: 409 });
    }
    const claim = await this.state.storage.get("claim");
    if (claim && claim.owner !== body.owner) {
      return Response.json({ error: "slug-taken" }, { status: 409 });
    }
    if (!claim) {
      await this.state.storage.put("claim", { owner: body.owner, at: body.now });
    }
    return new Response(null, { status: 204 });
  }

  // Releasing is deliberately narrow: it drops nothing once a row exists, and
  // nothing owned by anyone else, so a repeated or late abandon cannot remove a
  // slug something was actually committed under.
  async unclaim(body) {
    const claim = await this.state.storage.get("claim");
    if (claim && claim.owner === body.owner && !(await this.state.storage.get("row"))) {
      await this.state.storage.delete("claim");
    }
    return new Response(null, { status: 204 });
  }

  async get() {
    const row = await this.state.storage.get("row");
    if (!row) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json(row);
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
    await this.state.storage.put("row", row);
    return Response.json({ changed: true });
  }

  // Appends a version. Version numbers never reuse a retired one, so the
  // counter is stored rather than derived from the list length.
  async append(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ appended: false, reason: "absent" });
    }
    const versions = (await this.state.storage.get("versions")) ?? [];
    const nextVer = ((await this.state.storage.get("maxVer")) ?? 1) + 1;
    versions.push({
      ver: nextVer, kind: body.kind, contentSha: body.contentSha, size: body.size,
      createdAt: body.now, deleted: false, manifest: body.manifest ?? null,
    });
    await this.state.storage.put("versions", versions);
    await this.state.storage.put("maxVer", nextVer);
    // Bumped whether or not the head rolls: a pinned paste that gains a version
    // has still been updated, and the timestamp reports the paste's last change,
    // not the served version's.
    row.updatedAt = body.now ?? row.updatedAt;
    const wasPinned = (row.pinnedVersion ?? 0) !== 0;
    this.rollServed(row, versions);
    await this.state.storage.put("row", row);
    // The CHARGE counts every live version, pinned or not - a pin changes which
    // version is served, never which are retained - so it is deliberately a
    // different number from row.size.
    const charged = versions.reduce((n, v) => (v.deleted ? n : n + (v.size ?? 0)), 0);
    return Response.json({ appended: true, ver: nextVer, wasPinned, totalSize: charged });
  }

  // NEWEST FIRST, which is the order every reader wants and none should have to
  // impose: a listing sorted by insertion leaks the storage order into the UI.
  async listVersions() {
    const versions = (await this.state.storage.get("versions")) ?? [];
    return Response.json([...versions].sort((a, b) => b.ver - a.ver));
  }

  // Cannot be refused, so there is nothing to check first. Returns the paste's
  // new total so the caller can settle the charge from a value this cell
  // computed, rather than by adjusting a number it holds separately.
  async deleteVersion(body) {
    const versions = (await this.state.storage.get("versions")) ?? [];
    const v = versions.find((x) => x.ver === body.ver);
    if (!v) {
      return Response.json({ deleted: false, reason: "absent" });
    }
    if (v.deleted) {
      // Already tombstoned: a no-op, NOT a not-found. Re-deleting must be safe,
      // because a retry cannot tell whether its first attempt landed.
      return Response.json({
        deleted: true,
        totalSize: versions.reduce((n, x) => (x.deleted ? n : n + (x.size ?? 0)), 0),
      });
    }
    // TOMBSTONED, not removed. The row stays in the listing marked deleted so a
    // reader can tell "this version was retired" from "this version never
    // existed", and so its number is visibly never reused.
    v.deleted = true;
    await this.state.storage.put("versions", versions);
    const row = await this.state.storage.get("row");
    const total = versions.reduce((n, x) => (x.deleted ? n : n + (x.size ?? 0)), 0);
    if (row) {
      // Deleting the served version rolls the head onto the next live one.
      if (row.pinnedVersion === body.ver) {
        row.pinnedVersion = 0;
      }
      this.rollServed(row, versions);
      await this.state.storage.put("row", row);
    }
    return Response.json({ deleted: true, totalSize: total });
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
      return;
    }
    const pinned = row.pinnedVersion
      ? live.find((v) => v.ver === row.pinnedVersion)
      : null;
    const v = pinned ?? live.reduce((a, b) => (b.ver > a.ver ? b : a));
    row.contentSha = v.contentSha;
    row.kind = v.kind;
    row.manifest = v.manifest ?? null;
    row.size = v.size;
  }

  // Pin changes which version the public URL SERVES, not what is retained, so
  // it never touches the charge and stays inside this cell. Verified against
  // the identity summary rather than assumed: the summary carries name, status,
  // size and kind, and none of those move when a pin does.
  async pin(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ pinned: false, reason: "absent" });
    }
    row.pinnedVersion = body.ver ?? 0;
    this.rollServed(row, (await this.state.storage.get("versions")) ?? []);
    await this.state.storage.put("row", row);
    return Response.json({ pinned: true, ver: row.pinnedVersion });
  }

  // Guarded the same way a rename is: a slug deleted and re-minted by someone
  // else must not be removable by a stale request holding the old identity.
  async remove(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ removed: false, reason: "absent" });
    }
    if (row.identity !== body.identity || row.createdAt !== body.createdAt) {
      return Response.json({ removed: false, reason: "not-owner" });
    }
    await this.state.storage.delete("row");
    return Response.json({ removed: true });
  }

  // Only a still-PENDING row transitions, so a late finalizer cannot resurrect
  // a paste the reconciler already failed, and a repeat is harmless.
  async setStatus(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ changed: false, reason: "absent" });
    }
    if (row.status !== "pending") {
      return Response.json({ changed: false, reason: "not-pending", status: row.status });
    }
    row.status = body.status;
    await this.state.storage.put("row", row);
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
// The subnet is the rate-limit unit, so it is the cell. Admission is a
// check-and-record that must not interleave - two keys racing the last slot
// would both be admitted if the count and the write were separate steps - and a
// cell is single-threaded, so the whole decision is one event by construction.
// That is the same property the identity cell gives the quota check.
export class Subnet {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.pathname.split("/").pop();
    const body = request.method === "POST" ? await request.json() : {};
    switch (op) {
      case "admit":
        return this.admit(body);
      case "snapshot":
        return this.snapshot(Number(url.searchParams.get("now")), Number(url.searchParams.get("window")));
      default:
        return new Response("unknown op\n", { status: 404 });
    }
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
    // The scope IS the cell. idFromName is deterministic, so every node routes
    // one owner's intents to one cell without coordinating.
    // The scope IS the cell: one owner, one identity cell, holding intents,
    // quota and index together.
    return env.IDENTITY.get(env.IDENTITY.idFromName(scope)).fetch(request);
  },
};
