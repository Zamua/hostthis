// The owner identity scopes intents, quota reservations, and the paste index.
// Paste rows use slug-scoped cells, so cross-cell creates recover through the
// owner's durable intent log.

// A create intent younger than this is an in-flight create, not a crash.
// Recovery before the Go side has written the Paste row would fence and
// release a create that is about to succeed.
const CREATE_INTENT_GRACE_MS = 30_000;

const INTENT_PREFIX = "intent:";
const CREATE_ABORT_PREFIX = "create-abort:";
const ARTIFACT_ACCOUNT_PREFIX = "artifact-account:";
const ARTIFACT_PENDING = "artifactPending";
const ARTIFACT_RECEIPT_PREFIX = "artifact-receipt:";

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

// Each cell class routes through an op table: op name to { run, mutating,
// body, room }. A mutating op runs under blockConcurrencyWhile so its
// read-check-write sequence cannot interleave; body parses the request JSON;
// room loads the room document and answers 404 without one. run receives
// (self, body, url, doc).
const read = (run, flags = {}) => ({ run, ...flags });
const mutation = (run, flags = {}) => ({ mutating: true, body: true, run, ...flags });

function runOp(self, entry, body, url) {
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
// own default.
async function dispatch(self, table, request) {
  const url = new URL(request.url);
  const entry = table[url.pathname.split("/").pop()];
  if (!entry) {
    return null;
  }
  return runOp(self, entry, entry.body ? await request.json() : undefined, url);
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

const IDENTITY_OPS = {
  bytes: read((self) => self.bytes()),
  list: read((self) => self.list()),
  firstSeen: read((self) => self.firstSeen()),
  reserve: mutation((self, body) => self.reserve(body)),
  release: mutation((self, body) => self.release(body)),
  confirm: mutation((self, body) => self.confirm(body)),
  drop: mutation((self, body) => self.drop(body)),
  artifactseed: mutation((self, body) => self.artifactSeed(body)),
  artifactdecide: mutation((self, body) => self.artifactDecide(body)),
  artifactproject: mutation((self, body) => self.artifactProject(body)),
  artifactdrop: mutation((self, body) => self.artifactDrop(body)),
  notesubnet: mutation((self, body) => self.noteSubnet(body)),
  subnets: mutation((self, _body, url) => self.subnets(url), { body: false }),
};

// The Identity cell owns the state that must agree for one owner: outstanding
// intents, quota reservations, and the paste index.
export class Identity {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    return (await dispatch(this, IDENTITY_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
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
      // The cell clock, not the caller's: recovery is scheduled against it.
      const now = Date.now();
      updates.set(intentKey(body.intent.id), {
        ...toRow({ ...body.intent, generation, status: body.status ?? "pending" }),
        reservedAt: now,
      });
      await tx.put(updates);
      await tx.setAlarm(now + CREATE_INTENT_GRACE_MS);
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
    return this.env.PASTES.get(this.env.PASTES.idFromName(slug));
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
      const now = Date.now();
      let retry = false;
      let nextDue = Infinity;
      for (const [key, intent] of intents) {
        if (intent.kind !== "create_paste") {
          retry = true;
          continue;
        }
        const due = (intent.reservedAt ?? 0) + CREATE_INTENT_GRACE_MS;
        if (due > now) {
          nextDue = Math.min(nextDue, due);
          continue;
        }
        const id = key.slice(INTENT_PREFIX.length);
        if (!await this.resolveCreateIntent(id, intent)) {
          retry = true;
        }
      }
      if (retry) {
        await this.state.storage.setAlarm(now + 1000);
      } else if (nextDue !== Infinity) {
        await this.state.storage.setAlarm(nextDue);
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

  async subnets(url) {
    const now = Number(url.searchParams.get("now"));
    const windowMs = Number(url.searchParams.get("window"));
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
}


// A Room cell owns one room's KV document, dense mutation sequence, budget
// recovery state, and hibernatable WebSockets. Its app's Paste cell serializes
// cross-room byte allocations.
// Web Push: RFC 8030 delivery, RFC 8291 aes128gcm encryption, RFC 8292 VAPID.
// The runtime imports no HKDF and no raw EC point, so HKDF is HMAC by hand and
// a subscriber's point is wrapped in the P-256 SPKI prefix before import.

const PUSH_KEY = "push";
const PUSH_MAX_SUBSCRIPTIONS = 16;
const PUSH_MAX_ITEMS = 16;
const PUSH_MAX_ENDPOINT = 1024;
const PUSH_MAX_BODY = 1024;
const PUSH_MAX_PAYLOAD = 2048;
const PUSH_MAX_ROOM_KEY = 256;
const PUSH_DAILY_CAP = 8;
const PUSH_STALE_MS = 15 * 60 * 1000;
const PUSH_TEST_INTERVAL_MS = 60 * 1000;
const PUSH_SEND_TIMEOUT_MS = 10 * 1000;
const PUSH_RECORD_SIZE = 4096;
// Push-service hosts, matched exactly or as a dot-suffix; ciphertext and VAPID
// tokens go nowhere else.
const PUSH_SERVICE_HOSTS = [
  "push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com",
];
const VAPID_TTL_MS = 12 * 60 * 60 * 1000;
const VAPID_KEY = "pushVapid";
// A push service can answer 404 for a registration it has not finished
// propagating; a gone answer inside this window is a failed send, not a prune.
const PUSH_PRUNE_GRACE_MS = 60_000;
const ECDH_P256 = { name: "ECDH", namedCurve: "P-256" };
const ECDSA_P256 = { name: "ECDSA", namedCurve: "P-256" };
const P256_SPKI_PREFIX = new Uint8Array([
  48, 89, 48, 19, 6, 7, 42, 134, 72, 206, 61, 2, 1, 6, 8, 42, 134, 72, 206, 61, 3, 1, 7, 3, 66, 0,
]);
const PUSH_ID_PATTERN = /^[A-Za-z0-9_-]{1,32}$/;
const PUSH_AT_PATTERN = /^([01][0-9]|2[0-3]):[0-5][0-9]$/;
const RFC3339_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

function utf8(text) {
  return new TextEncoder().encode(text);
}

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function b64u(bytes) {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// Accepts base64url or base64, padded or not; null for anything else.
function unb64u(text) {
  if (typeof text !== "string" || !/^[A-Za-z0-9_=+/-]*$/.test(text)) {
    return null;
  }
  const normalized = text.replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
  if (normalized.length % 4 === 1) {
    return null;
  }
  try {
    const binary = atob(normalized + "=".repeat((4 - (normalized.length % 4)) % 4));
    return Uint8Array.from(binary, (c) => c.charCodeAt(0));
  } catch {
    return null;
  }
}

async function hmacSha256(key, data) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, data));
}

// HKDF-SHA256 extract + expand, one block: every length the protocol asks for
// is at most 32 bytes.
async function hkdf(salt, ikm, info, length) {
  const prk = await hmacSha256(salt, ikm);
  const block = await hmacSha256(prk, concat(info, new Uint8Array([1])));
  return block.slice(0, length);
}

function importP256Point(point) {
  return crypto.subtle.importKey("spki", concat(P256_SPKI_PREFIX, point), ECDH_P256, false, []);
}

// exportKey("raw") returns the bare point on some runtimes and SPKI on others;
// the point is always the last 65 bytes.
async function rawPoint(publicKey) {
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", publicKey));
  return raw.slice(raw.length - 65);
}

// RFC 8291 aes128gcm: one record, rs 4096, header || AES-GCM(plaintext || 0x02).
// serverKeys and salt are injectable so a test can pin the RFC vector.
export async function encryptPush(subscription, plaintext, { serverKeys, salt } = {}) {
  serverKeys ??= await crypto.subtle.generateKey(ECDH_P256, true, ["deriveBits"]);
  salt ??= crypto.getRandomValues(new Uint8Array(16));
  const uaPoint = unb64u(subscription.p256dh);
  const auth = unb64u(subscription.auth);
  const uaKey = await importP256Point(uaPoint);
  const secret = new Uint8Array(
    await crypto.subtle.deriveBits({ name: "ECDH", public: uaKey }, serverKeys.privateKey, 256),
  );
  const asPoint = await rawPoint(serverKeys.publicKey);
  const ikm = await hkdf(auth, secret, concat(utf8("WebPush: info\0"), uaPoint, asPoint), 32);
  const cek = await hkdf(salt, ikm, utf8("Content-Encoding: aes128gcm\0"), 16);
  const nonce = await hkdf(salt, ikm, utf8("Content-Encoding: nonce\0"), 12);
  const key = await crypto.subtle.importKey("raw", cek, "AES-GCM", false, ["encrypt"]);
  const ciphertext = new Uint8Array(
    await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, key, concat(plaintext, new Uint8Array([2]))),
  );
  const recordSize = new Uint8Array(4);
  new DataView(recordSize.buffer).setUint32(0, PUSH_RECORD_SIZE);
  return concat(salt, recordSize, new Uint8Array([asPoint.length]), asPoint, ciphertext);
}

// ES256 JWS compact form; WebCrypto's ECDSA output is already r || s.
export async function signVapid(privateKey, claims) {
  const part = (value) => b64u(utf8(JSON.stringify(value)));
  const input = `${part({ typ: "JWT", alg: "ES256" })}.${part(claims)}`;
  const signature = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, utf8(input));
  return `${input}.${b64u(new Uint8Array(signature))}`;
}

function httpsOrigin(value) {
  if (typeof value !== "string") {
    return null;
  }
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.origin === value ? url.origin : null;
  } catch {
    return null;
  }
}

const localFormats = new Map();

function localFormat(tz) {
  let format = localFormats.get(tz);
  if (!format) {
    format = new Intl.DateTimeFormat("en-US", {
      timeZone: tz, hourCycle: "h23",
      year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", second: "2-digit",
    });
    localFormats.set(tz, format);
  }
  return format;
}

function knownTimeZone(tz) {
  if (typeof tz !== "string" || !tz) {
    return false;
  }
  try {
    localFormat(tz);
    return true;
  } catch {
    return false;
  }
}

function localParts(ms, tz) {
  const parts = {};
  for (const { type, value } of localFormat(tz).formatToParts(new Date(ms))) {
    parts[type] = value;
  }
  return {
    year: Number(parts.year), month: Number(parts.month), day: Number(parts.day),
    hour: Number(parts.hour) % 24, minute: Number(parts.minute), second: Number(parts.second),
  };
}

function pad2(n) {
  return String(n).padStart(2, "0");
}

export function localDate(ms, tz) {
  const p = localParts(ms, tz);
  return `${p.year}-${pad2(p.month)}-${pad2(p.day)}`;
}

function offsetAt(ms, tz) {
  const p = localParts(ms, tz);
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - Math.floor(ms / 1000) * 1000;
}

// The instant of a wall-clock time in tz. A skipped clock (spring forward)
// resolves to the shifted time; a repeated clock to its first occurrence.
export function zonedToUTC(year, month, day, hour, minute, tz) {
  const guess = Date.UTC(year, month - 1, day, hour, minute);
  const first = guess - offsetAt(guess, tz);
  const second = guess - offsetAt(first, tz);
  for (const candidate of [first, second]) {
    const p = localParts(candidate, tz);
    if (p.day === day && p.hour === hour && p.minute === minute) {
      return candidate;
    }
  }
  return Math.max(first, second);
}

// Next instant strictly after now at which the item fires, in tz.
export function nextDue(item, tz, now) {
  if (item.when !== undefined) {
    return Date.parse(item.when);
  }
  const [hour, minute] = item.at.split(":").map(Number);
  const today = localParts(now, tz);
  for (let k = 0; k <= 7; k++) {
    const day = new Date(Date.UTC(today.year, today.month - 1, today.day + k));
    if (!item.days.includes(day.getUTCDay())) {
      continue;
    }
    const at = zonedToUTC(day.getUTCFullYear(), day.getUTCMonth() + 1, day.getUTCDate(), hour, minute, tz);
    if (at > now) {
      return at;
    }
  }
  return null;
}

function invalid(error, status = 400) {
  return { error: Response.json({ error }, { status }) };
}

function pushServiceHost(host) {
  return PUSH_SERVICE_HOSTS.some((known) => host === known || host.endsWith(`.${known}`));
}

function validateSubscription(body) {
  if (typeof body?.endpoint !== "string" || utf8(body.endpoint).length > PUSH_MAX_ENDPOINT) {
    return invalid("invalid-endpoint");
  }
  try {
    const url = new URL(body.endpoint);
    if (url.protocol !== "https:" || !pushServiceHost(url.hostname)) {
      return invalid("invalid-endpoint");
    }
  } catch {
    return invalid("invalid-endpoint");
  }
  const p256dh = unb64u(body.keys?.p256dh);
  if (!p256dh || p256dh.length !== 65 || p256dh[0] !== 4) {
    return invalid("invalid-p256dh");
  }
  const auth = unb64u(body.keys?.auth);
  if (!auth || auth.length !== 16) {
    return invalid("invalid-auth");
  }
  return { subscription: { endpoint: body.endpoint, p256dh: b64u(p256dh), auth: b64u(auth) } };
}

function validRoomKey(key) {
  if (typeof key !== "string" || !key || utf8(key).length > PUSH_MAX_ROOM_KEY) {
    return false;
  }
  return key !== "ws" && key !== "push" && !key.startsWith("push/");
}

function optionalText(value, max) {
  return value === undefined || (typeof value === "string" && utf8(value).length <= max);
}

function validateItem(item, now) {
  if (item === null || typeof item !== "object" || Array.isArray(item)) {
    return "invalid-item";
  }
  if (typeof item.id !== "string" || !PUSH_ID_PATTERN.test(item.id)) {
    return "invalid-id";
  }
  const recurring = item.at !== undefined || item.days !== undefined;
  if (recurring === (item.when !== undefined)) {
    return "invalid-time";
  }
  if (recurring) {
    if (typeof item.at !== "string" || !PUSH_AT_PATTERN.test(item.at)) {
      return "invalid-at";
    }
    if (!Array.isArray(item.days) || item.days.length === 0 || item.days.length > 7 ||
        !item.days.every((d) => Number.isInteger(d) && d >= 0 && d <= 6) ||
        new Set(item.days).size !== item.days.length) {
      return "invalid-days";
    }
  } else if (typeof item.when !== "string" || !RFC3339_PATTERN.test(item.when) ||
             !(Date.parse(item.when) > now)) {
    return "invalid-when";
  }
  if (typeof item.title !== "string" || !item.title || utf8(item.title).length > 64) {
    return "invalid-title";
  }
  if (!optionalText(item.url, 512)) {
    return "invalid-url";
  }
  if (!optionalText(item.tag, 64)) {
    return "invalid-tag";
  }
  if ((item.body !== undefined) === (item.bodyKey !== undefined)) {
    return "invalid-body";
  }
  if (item.body !== undefined && (typeof item.body !== "string" || utf8(item.body).length > PUSH_MAX_BODY)) {
    return "invalid-body";
  }
  if (item.bodyKey !== undefined && !validRoomKey(item.bodyKey)) {
    return "invalid-body-key";
  }
  return null;
}

function validateSchedule(body, now) {
  if (!knownTimeZone(body?.tz)) {
    return invalid("invalid-tz");
  }
  if (!Array.isArray(body.items)) {
    return invalid("invalid-items");
  }
  if (body.items.length > PUSH_MAX_ITEMS) {
    return invalid("too-many-items", 413);
  }
  const ids = new Set();
  const items = [];
  for (const item of body.items) {
    const error = validateItem(item, now);
    if (error) {
      return invalid(error);
    }
    if (ids.has(item.id)) {
      return invalid("duplicate-id");
    }
    ids.add(item.id);
    const copy = { id: item.id };
    for (const field of ["at", "days", "when", "title", "body", "bodyKey", "url", "tag"]) {
      if (item[field] !== undefined) {
        copy[field] = field === "days" ? [...item.days] : item[field];
      }
    }
    items.push(copy);
  }
  return { schedule: { tz: body.tz, items } };
}

function publicItem(item) {
  const { due, ...rest } = item;
  return rest;
}

const ROOM_OPS = {
  count: read((self) => Response.json({ sockets: self.state.getWebSockets().length })),
  meta: read((self, _body, _url, doc) => Response.json(doc.meta), { room: true }),
  get: read((self, _body, url, doc) => self.getValue(doc, url.searchParams.get("key")), { room: true }),
  scan: read((self, _body, _url, doc) => Response.json({ values: doc.kv, seq: doc.seq }), { room: true }),
  create: mutation((self, body) => self.createBlocked(body)),
  put: mutation((self, body, _url, doc) => self.putBlocked(doc, body), { room: true }),
  del: mutation((self, body, _url, doc) => self.delBlocked(doc, body), { room: true }),
  pushsublist: read((self) => self.pushSubList(), { room: true }),
  pushscheduleget: read((self) => self.pushScheduleGet(), { room: true }),
  pushsubput: mutation((self, body) => self.pushSubPut(body), { room: true }),
  pushsubdel: mutation((self, body) => self.pushSubDel(body), { room: true }),
  pushscheduleput: mutation((self, body) => self.pushSchedulePut(body), { room: true }),
  pushtest: mutation((self, body, _url, doc) => self.pushTest(doc, body), { room: true }),
};

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
    const handled = await dispatch(this, ROOM_OPS, request);
    if (handled) {
      return handled;
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

  // Direct entry points, same gating as the op table.
  create(body) {
    return runOp(this, ROOM_OPS.create, body);
  }

  put(body) {
    return runOp(this, ROOM_OPS.put, body);
  }

  del(body) {
    return runOp(this, ROOM_OPS.del, body);
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

  getValue(doc, key) {
    if (!Object.hasOwn(doc.kv, key)) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ value: doc.kv[key] });
  }

  async putBlocked(doc, body) {
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

  async delBlocked(doc, body) {
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
      await this.rearm(Date.now(), tx);
    });
  }

  async clearPending(doc) {
    delete doc.pending;
    await this.state.storage.transaction(async (tx) => {
      await tx.put("state", doc);
      await this.rearm(null, tx);
    });
  }

  async retryPendingLater() {
    await this.rearm(Date.now() + 1000);
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
      await this.rearm(Date.now());
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

  broadcastPut(doc, key) {
    this.broadcast({
      type: "put", seq: doc.seq, key,
      value: Object.hasOwn(doc.wire, key) ? JSON.parse(doc.wire[key]) : null,
    });
  }

  // info.now is a test hook; the runtime passes retry metadata only.
  async alarm(info = {}) {
    await this.state.blockConcurrencyWhile(async () => {
      await this.resumePending(undefined, true);
      await this.firePush(info.now ?? Date.now());
    });
  }

  // One alarm serves both the budget retry and the push timer: a re-arm takes
  // the earlier deadline, and neither side can disarm the other's.
  async rearm(budgetAt, tx = this.state.storage) {
    const pushAt = (await tx.get(PUSH_KEY))?.nextFireAt ?? null;
    const at = budgetAt === null ? pushAt : pushAt === null ? budgetAt : Math.min(budgetAt, pushAt);
    if (at === null) {
      await tx.deleteAlarm();
    } else {
      await tx.setAlarm(at);
    }
  }

  async loadPush() {
    return (await this.state.storage.get(PUSH_KEY)) ??
      { subscriptions: [], schedule: null, nextFireAt: null, lastTestAt: 0 };
  }

  // A pending budget already holds the alarm at its retry instant, which the
  // push save must keep; without one the push deadline is the whole alarm.
  async savePush(push) {
    const dues = (push.schedule?.items ?? []).map((item) => item.due);
    push.nextFireAt = dues.length ? Math.min(...dues) : null;
    await this.state.storage.transaction(async (tx) => {
      await tx.put(PUSH_KEY, push);
      const doc = await tx.get("state");
      const budgetAt = doc?.pending ? (await tx.getAlarm()) ?? Date.now() : null;
      await this.rearm(budgetAt, tx);
    });
  }

  async pushSubPut(body) {
    const checked = validateSubscription(body);
    if (checked.error) {
      return checked.error;
    }
    const push = await this.loadPush();
    const existing = push.subscriptions.find((s) => s.endpoint === checked.subscription.endpoint);
    if (existing) {
      Object.assign(existing, checked.subscription);
    } else {
      if (push.subscriptions.length >= PUSH_MAX_SUBSCRIPTIONS) {
        return Response.json({ error: "too-many-subscriptions" }, { status: 413 });
      }
      push.subscriptions.push({ ...checked.subscription, added: body.now ?? Date.now(), sent: {} });
    }
    if (typeof body.subject === "string") {
      push.subject = body.subject;
    }
    await this.savePush(push);
    return new Response(null, { status: 204 });
  }

  async pushSubDel(body) {
    if (typeof body?.endpoint !== "string") {
      return Response.json({ error: "invalid-endpoint" }, { status: 400 });
    }
    const push = await this.loadPush();
    const kept = push.subscriptions.filter((s) => s.endpoint !== body.endpoint);
    if (kept.length !== push.subscriptions.length) {
      push.subscriptions = kept;
      await this.savePush(push);
    }
    return new Response(null, { status: 204 });
  }

  async pushSubList() {
    const push = await this.loadPush();
    return Response.json({
      subscriptions: push.subscriptions.map((s) => ({
        endpoint: s.endpoint, added: new Date(s.added).toISOString(),
      })),
    });
  }

  async pushSchedulePut(body) {
    const now = body.now ?? Date.now();
    const checked = validateSchedule(body, now);
    if (checked.error) {
      return checked.error;
    }
    const { schedule } = checked;
    for (const item of schedule.items) {
      item.due = nextDue(item, schedule.tz, now);
    }
    const push = await this.loadPush();
    push.schedule = schedule;
    if (typeof body.subject === "string") {
      push.subject = body.subject;
    }
    await this.savePush(push);
    return new Response(null, { status: 204 });
  }

  async pushScheduleGet() {
    const push = await this.loadPush();
    return Response.json({
      tz: push.schedule?.tz ?? "",
      items: (push.schedule?.items ?? []).map(publicItem),
    });
  }

  async pushTest(doc, body) {
    const now = body?.now ?? Date.now();
    const push = await this.loadPush();
    const elapsed = now - (push.lastTestAt ?? 0);
    if (elapsed < PUSH_TEST_INTERVAL_MS) {
      return Response.json({
        error: "rate-limited",
        retryAfter: Math.ceil((PUSH_TEST_INTERVAL_MS - elapsed) / 1000),
      }, { status: 429 });
    }
    push.lastTestAt = now;
    await this.savePush(push);
    const payload = utf8(JSON.stringify({
      title: "Test notification", body: "Push notifications are working.", tag: "hostthis-test",
    }));
    const date = localDate(now, push.schedule?.tz ?? "UTC");
    const outcome = await this.deliver(push, this.dispatch(push, doc, payload, date, new Map(), now));
    await this.savePush(push);
    return Response.json(outcome);
  }

  // Fires every item whose due instant has passed. The schedule advances and
  // persists before any send, so a crash mid-fire loses sends rather than
  // repeating them.
  async firePush(now) {
    const push = await this.loadPush();
    if (!push.schedule?.items.length || push.nextFireAt === null) {
      return;
    }
    if (push.nextFireAt > now) {
      // The runtime consumed the alarm; the deadline must be set again.
      await this.savePush(push);
      return;
    }
    const { tz } = push.schedule;
    const fires = [];
    const kept = [];
    for (const item of push.schedule.items) {
      if (item.due > now) {
        kept.push(item);
        continue;
      }
      if (now - item.due <= PUSH_STALE_MS) {
        fires.push({ item, date: localDate(item.due, tz) });
      }
      if (item.when === undefined) {
        item.due = nextDue(item, tz, now);
        kept.push(item);
      }
    }
    push.schedule.items = kept;
    await this.savePush(push);
    const doc = fires.length ? await this.loadRoom() : null;
    if (!doc) {
      return;
    }
    const signed = new Map();
    const sends = [];
    for (const { item, date } of fires) {
      const payload = this.resolvePayload(item, doc, date);
      if (payload) {
        sends.push(...this.dispatch(push, doc, payload, date, signed, now));
      }
    }
    await this.deliver(push, sends);
    await this.savePush(push);
  }

  // date is the item's local due date in tz.
  resolvePayload(item, doc, date) {
    let body = item.body;
    if (body === undefined) {
      const key = item.bodyKey.replaceAll("{date}", date);
      if (!Object.hasOwn(doc.kv, key)) {
        return null;
      }
      const bytes = unb64u(doc.kv[key]);
      if (!bytes) {
        return null;
      }
      body = new TextDecoder().decode(bytes);
    }
    const payload = utf8(JSON.stringify({ title: item.title, body, url: item.url, tag: item.tag }));
    return payload.length > PUSH_MAX_PAYLOAD ? null : payload;
  }

  // Charges the daily counter of every subscription under the cap and starts
  // one send per charged subscription. The cap is decided here, before any
  // send runs, so concurrent sends never race on it.
  dispatch(push, doc, payload, date, signed, now) {
    const sends = [];
    for (const sub of push.subscriptions) {
      const count = sub.sent?.[date] ?? 0;
      if (count >= PUSH_DAILY_CAP) {
        continue;
      }
      sub.sent = { [date]: count + 1 };
      sends.push(this.sendPush(sub, payload, doc.meta.appSlug, push.subject, signed, now).then((result) => ({ sub, result })));
    }
    return sends;
  }

  // Awaits the sends of one fire together and prunes gone subscriptions.
  // Mutates push; the caller persists.
  async deliver(push, sends) {
    const outcome = { sent: 0, pruned: 0 };
    const gone = new Set();
    for (const settled of await Promise.allSettled(sends)) {
      if (settled.status !== "fulfilled") {
        continue;
      }
      if (settled.value.result === "gone") {
        gone.add(settled.value.sub);
      } else if (settled.value.result === "sent") {
        outcome.sent++;
      }
    }
    outcome.pruned = gone.size;
    push.subscriptions = push.subscriptions.filter((sub) => !gone.has(sub));
    return outcome;
  }

  // signed caches the in-flight signing per origin so concurrent sends to one
  // push service share a token.
  vapidHeader(origin, appSlug, subject, signed) {
    if (!signed.has(origin)) {
      const claims = { aud: origin };
      if (subject) {
        claims.sub = subject;
      }
      signed.set(origin, this.appCall(appSlug, "pushsign", claims).then(async (res) => {
        if (!res.ok) {
          throw new Error("vapid signing unavailable");
        }
        const { token, key } = await res.json();
        return `vapid t=${token}, k=${key}`;
      }));
    }
    return signed.get(origin);
  }

  async sendPush(sub, payload, appSlug, subject, signed, now) {
    try {
      const authorization = await this.vapidHeader(new URL(sub.endpoint).origin, appSlug, subject, signed);
      const body = await encryptPush(sub, payload);
      const res = await (this.env.PUSH_FETCH ?? fetch)(sub.endpoint, {
        method: "POST",
        headers: {
          "Content-Encoding": "aes128gcm",
          "Content-Type": "application/octet-stream",
          TTL: "86400",
          Urgency: "normal",
          Authorization: authorization,
        },
        body,
        redirect: "manual",
        signal: AbortSignal.timeout(this.env.PUSH_SEND_TIMEOUT_MS ?? PUSH_SEND_TIMEOUT_MS),
      });
      await res.body?.cancel();
      if ((res.status === 404 || res.status === 410) && now - sub.added >= PUSH_PRUNE_GRACE_MS) {
        return "gone";
      }
      return res.ok ? "sent" : "failed";
    } catch {
      return "failed";
    }
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
const PASTE_OPS = {
  get: read((self) => self.get()),
  versions: read((self) => self.listVersions()),
  put: mutation((self, body) => self.put(body)),
  abortcreate: mutation((self, body) => self.abortCreate(body)),
  status: mutation((self, body) => self.setStatus(body)),
  rename: mutation((self, body) => self.rename(body)),
  roomcreated: mutation((self, body) => self.roomCreated(body)),
  roomcounts: mutation((self, _body, url) => self.roomCounts(url), { body: false }),
  roompreflight: mutation((self, body) => self.roomPreflight(body)),
  roomdecide: mutation((self, body) => self.roomDecide(body)),
  remove: mutation((self, body) => self.remove(body)),
  append: mutation((self, body) => self.append(body)),
  delversion: mutation((self, body) => self.deleteVersion(body)),
  pin: mutation((self, body) => self.pin(body)),
  pushkey: mutation((self) => self.pushKey(), { body: false }),
  pushsign: mutation((self, body) => self.pushSign(body)),
};

export class Paste {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    return (await dispatch(this, PASTE_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
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
  async roomCounts(url) {
    const now = Number(url.searchParams.get("now"));
    const windowMs = Number(url.searchParams.get("window"));
    const subnet = url.searchParams.get("subnet");
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

  async vapidKeys() {
    let keys = await this.state.storage.get(VAPID_KEY);
    if (!keys) {
      const pair = await crypto.subtle.generateKey(ECDSA_P256, true, ["sign", "verify"]);
      keys = {
        privateJwk: await crypto.subtle.exportKey("jwk", pair.privateKey),
        publicKey: b64u(await rawPoint(pair.publicKey)),
      };
      await this.state.storage.put(VAPID_KEY, keys);
    }
    return keys;
  }

  async pushKey() {
    return Response.json({ key: (await this.vapidKeys()).publicKey });
  }

  // Signs for one push-service origin; the private key never leaves the cell.
  async pushSign(body) {
    const aud = httpsOrigin(body?.aud);
    if (!aud) {
      return Response.json({ error: "invalid-aud" }, { status: 400 });
    }
    if (body.sub !== undefined && typeof body.sub !== "string") {
      return Response.json({ error: "invalid-sub" }, { status: 400 });
    }
    const keys = await this.vapidKeys();
    const privateKey = await crypto.subtle.importKey("jwk", keys.privateJwk, ECDSA_P256, false, ["sign"]);
    const now = body.now ?? Date.now();
    const claims = { aud, exp: Math.floor((now + VAPID_TTL_MS) / 1000) };
    if (body.sub) {
      claims.sub = body.sub;
    }
    return Response.json({ token: await signVapid(privateKey, claims), key: keys.publicKey });
  }

  async roomPreflight(body) {
    const total = (await this.state.storage.get("roomAllocated")) ?? 0;
    if (body.appCap > 0 && total >= body.appCap) {
      return Response.json({ error: "app-full", total }, { status: 507 });
    }
    return Response.json({ total });
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

  // A legacy row (no generation) adopts on first mutation: assign a fresh
  // generation, then idempotently seed the Identity account from live versions
  // before any accounting decision runs against it, or a decide would add the
  // legacy entry's charge a second time. The generation persists before the
  // seed so a response-lost seed retries under the SAME incarnation. A caller
  // holding the legacy row addresses it with an empty generation; an empty
  // generation against an adopted row stays a conflict, because real callers
  // re-read the row between attempts.
  async requireAdoptedGeneration(row, body) {
    if (row.generation) {
      if (row.generation !== body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
    } else {
      if (body.generation) {
        return Response.json({ error: "generation-mismatch" }, { status: 409 });
      }
      row.generation = crypto.randomUUID();
      row.accountingVersion = 0;
      await this.state.storage.transaction(async (tx) => {
        await tx.put(new Map([["row", row], ["legacyAdoptionPending", true]]));
      });
    }
    if (await this.state.storage.get("legacyAdoptionPending")) {
      const versions = (await this.state.storage.get("versions")) ?? [];
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
          latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
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
        return Response.json({ error: "adoption-seed-conflict", detail: seed }, { status: response.status });
      }
      row.accountingVersion = seed.version;
      await this.state.storage.transaction(async (tx) => {
        await tx.put("row", row);
        await tx.delete("legacyAdoptionPending");
      });
    }
    body.generation = row.generation;
    return null;
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
    if (typeof generation !== "string") {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (typeof opId !== "string" || !opId) {
      return Response.json({ error: "missing-operation-id" }, { status: 400 });
    }
    // An empty generation addresses a legacy row: no receipt scope exists
    // until adoption assigns one, and the adoption gate decides its fate.
    if (!generation) {
      return null;
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
    const refused = await this.requireAdoptedGeneration(row, body);
    if (refused) {
      return refused;
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
    const refused = await this.requireAdoptedGeneration(row, body);
    if (refused) {
      return refused;
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
    const refused = await this.requireAdoptedGeneration(row, body);
    if (refused) {
      return refused;
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
    const refused = await this.requireAdoptedGeneration(row, body);
    if (refused) {
      return refused;
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
    const refused = await this.requireAdoptedGeneration(row, body);
    if (refused) {
      return refused;
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
const SUBNET_OPS = {
  admit: mutation((self, body) => self.admit(body)),
  snapshot: mutation((self, _body, url) => self.snapshot(url), { body: false }),
};

export class Subnet {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    return (await dispatch(this, SUBNET_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
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

  async snapshot(url) {
    const rows = await this.live(Number(url.searchParams.get("now")),
      Number(url.searchParams.get("window")));
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
    if (!url.pathname.startsWith("/identity/")) {
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
