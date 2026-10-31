import { solidEnabled } from "../solid/identity.js";
import { cellRoute } from "./image.js";
import { cellCall, dispatch, mutation, read, requireShape } from "./ops.js";
import { CREATE_INTENT_GRACE_MS, INTENT_PREFIX, artifactAccountKey, chargedSize, fence, intentKey, toRow } from "./records.js";

// Sign-in attempts one pending pod link keeps (docs/SPEC.md "Pod storage").
const LINK_LOGIN_ATTEMPTS = 8;

const IDENTITY_OPS = {
  bytes: read((self) => self.bytes()),
  list: read((self) => self.list()),
  firstSeen: read((self) => self.firstSeen()),
  reserve: mutation((self, body) => self.reserve(body)),
  release: mutation((self, body) => self.release(body)),
  confirm: mutation((self, body) => self.confirm(body)),
  drop: mutation((self, body) => self.drop(body)),
  artifactdecide: mutation((self, body) => self.artifactDecide(body)),
  artifactproject: mutation((self, body) => self.artifactProject(body)),
  artifactdrop: mutation((self, body) => self.artifactDrop(body)),
  notesubnet: mutation((self, body) => self.noteSubnet(body)),
  // The owner's pod link, and the link cells (`podlink:<id>`) that record each
  // link (docs/SPEC.md "Pod storage").
  podget: read((self) => self.podGet()),
  podset: mutation((self, body) => self.podSet(body)),
  podclear: mutation((self) => self.podClear()),
  linkstart: mutation((self, body) => self.linkStart(body)),
  linkget: read((self) => self.linkGet()),
  linkbegin: mutation((self, body) => self.linkBegin(body)),
  linkcomplete: mutation((self, body) => self.linkComplete(body)),
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
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, IDENTITY_OPS, request))
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
      const active = Object.values(entries).reduce((n, e) => n + chargedSize(e), 0);
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
        return Response.json({ active, replayed: true });
      }
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
        createFingerprint: body.intent.fingerprint,
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
      await tx.put(Object.fromEntries(updates));
      await tx.setAlarm(now + CREATE_INTENT_GRACE_MS);
      return Response.json({ active: active + body.size });
    });
  }

  // Step 3: the row landed, so the entry is real and the intent is discharged.
  async confirm(body) {
    if (!requireShape(body, { generation: "string" })) {
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
    if (!requireShape(body, { generation: "string" })) {
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

  async resolveCreateIntent(id, intent) {
    if (
      !intent.subject || !intent.generation
      || typeof intent.fingerprint !== "string" || !intent.fingerprint
    ) {
      return false;
    }
    let response;
    try {
      response = await cellCall(this.env.PASTES, intent.subject, "/paste/abortcreate", "slug",
        { generation: intent.generation });
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

  async artifactDecide(body) {
    if (!requireShape(body, { slug: "string", generation: "string", version: "posint", target: "uint" })) {
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
      const decision = fence(current, total, body.version, body.target, body.userCap);
      if (!decision.record) {
        return Response.json(decision.body, { status: decision.status });
      }
      if (!Number.isSafeInteger(decision.nextTotal) || decision.nextTotal < 0) {
        return Response.json({ error: "invalid-artifact-total" }, { status: 500 });
      }
      entry.accountingVersion = body.version;
      if (decision.body.granted) {
        entry.size = body.target;
        entry.chargedSize = body.target;
      }
      await tx.put(Object.fromEntries([[key, decision.record], ["entries", entries]]));
      return Response.json(decision.body, { status: decision.status });
    });
  }

  async artifactProject(body) {
    if (!requireShape(body, { slug: "string", generation: "string", version: "uint", servedSize: "uint" })) {
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
      for (const key of ["status", "kind", "name", "latestVersion", "pinnedVersion", "updatedAt"]) {
        if (body[key] !== undefined && body[key] !== null) {
          entry[key] = body[key];
        }
      }
      // A stored contentSha would describe a version that may no longer be served.
      delete entry.contentSha;
      await tx.put("entries", entries);
      return Response.json({ updated: true });
    });
  }

  async artifactDrop(body) {
    if (!requireShape(body, { slug: "string", generation: "string", version: "posint" })) {
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

  async podGet() {
    return Response.json({ link: (await this.state.storage.get("pod")) ?? null });
  }

  async podSet(body) {
    if (!requireShape(body, { id: "string", webid: "string", storage: "string", linked: "uint" })) {
      return Response.json({ error: "malformed-link" }, { status: 400 });
    }
    const link = { id: body.id, webid: body.webid, storage: body.storage, linked: body.linked };
    await this.state.storage.put("pod", link);
    return Response.json({ link });
  }

  async podClear() {
    const had = (await this.state.storage.get("pod")) !== undefined;
    await this.state.storage.delete("pod");
    return Response.json({ cleared: had });
  }

  // A pending link: whose key it binds, the WebID it names, and its secret's
  // hash. Started once; a second start of the same id is a conflict.
  async linkStart(body) {
    if (!solidEnabled(this.env)) {
      return Response.json({ error: "pods-unavailable" }, { status: 409 });
    }
    if (!requireShape(body, { owner: "string", webid: "string", secretHash: "string", expires: "uint" })) {
      return Response.json({ error: "malformed-link" }, { status: 400 });
    }
    if ((await this.state.storage.get("link")) !== undefined) {
      return Response.json({ error: "link-exists" }, { status: 409 });
    }
    await this.state.storage.put("link", {
      state: "pending",
      owner: body.owner,
      webid: body.webid,
      secretHash: body.secretHash,
      expires: body.expires,
    });
    return new Response(null, { status: 204 });
  }

  async linkGet() {
    const link = await this.state.storage.get("link");
    return link ? Response.json(link) : new Response("not found\n", { status: 404 });
  }

  // A login under way: the state, PKCE verifier and DPoP key its callback
  // needs. Every attempt stays valid until one completes, so pressing
  // Continue again never strands the first sign-in.
  async linkBegin(body) {
    const link = await this.state.storage.get("link");
    if (!link || link.state !== "pending") {
      return Response.json({ error: "link-not-pending" }, { status: 409 });
    }
    const logins = [...(link.logins ?? []), body.login].slice(-LINK_LOGIN_ATTEMPTS);
    await this.state.storage.put("link", { ...link, logins });
    return new Response(null, { status: 204 });
  }

  // Pending to linked, once: the login's secrets are dropped and the record
  // never changes again.
  async linkComplete(body) {
    const link = await this.state.storage.get("link");
    if (!link || link.state !== "pending") {
      return Response.json({ error: "link-not-pending" }, { status: 409 });
    }
    if (typeof (body.storage ?? link.storage) !== "string") {
      return Response.json({ error: "malformed-link" }, { status: 400 });
    }
    const done = {
      state: "linked",
      owner: link.owner,
      webid: link.webid,
      storage: body.storage ?? link.storage,
      linked: body.linked,
    };
    await this.state.storage.put("link", done);
    return Response.json(done);
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
