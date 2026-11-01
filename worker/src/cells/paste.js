import { b64url } from "../solid/jose.js";
import { ECDSA_P256, VAPID_KEY, VAPID_TTL_MS, httpsOrigin, rawPoint, signVapid } from "../push.js";
import { manifestObjects, resolveRead } from "../read.js";
import { cellRoute } from "./image.js";
import { USER_QUOTA_BYTES } from "../rooms/policy.js";
import { DECIDE, DROP, PROJECT, advance, decisionEvent, nextPending, samePending, startPending } from "./artifact-saga.js";
import { cellCall, dispatch, mutation, read, requireShape } from "./ops.js";
import { ARTIFACT_PENDING, ARTIFACT_RECEIPT_PREFIX, LEGACY_VERSIONS, chargeOf, createAbortKey, deleteInBatches, fence, isTooBig, isUploadId, latestLive, loadManifest, loadVersions, manifestKey, prefixOf, retryDelay, servedOf, splitLegacyVersions, uploadPrefix, versionKey, withUploadId } from "./records.js";

// The PASTE cell: one row, addressed by slug. Separate from the identity cell
// because a reader arrives holding a slug and does not know the owner, while
// the index reader holds an identity and does not know the slugs. No single
// cell can serve both, which is exactly why the create spans two and needs an
// intent to survive a crash between them.
const PASTE_OPS = {
  get: read((self) => self.get()),
  versions: read((self) => self.listVersions()),
  resolve: read((self, _body, url) => self.resolve(url)),
  exists: read((self) => self.exists()),
  operatorrow: read((self) => self.operatorRow()),
  quarantine: mutation((self, body) => self.quarantine(body)),
  release: mutation((self) => self.release()),
  put: mutation((self, body) => self.put(body)),
  abortcreate: mutation((self, body) => self.abortCreate(body)),
  status: mutation((self, body) => self.setStatus(body)),
  rename: mutation((self, body) => self.rename(body)),
  roomcreated: mutation((self, body) => self.roomCreated(body)),
  roomcounts: mutation((self, _body, url) => self.roomCounts(url), { body: false }),
  roompreflight: mutation((self, body) => self.roomPreflight(body)),
  roomdecide: mutation((self, body) => self.roomDecide(body)),
  roomchargesync: mutation((self) => self.roomChargeSync()),
  remove: mutation((self, body) => self.remove(body)),
  append: mutation((self, body) => self.append(body)),
  delversion: mutation((self, body) => self.deleteVersion(body)),
  head: read((self) => self.head()),
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
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, PASTE_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  async put(body) {
    if (!requireShape(body, { generation: "string" })) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (!requireShape(body, { fingerprint: "string" }) || !body.row) {
      return Response.json({ error: "missing-create-fingerprint" }, { status: 400 });
    }
    if (!isUploadId(body.row.uploadId)) {
      return Response.json({ error: "invalid-upload-id" }, { status: 400 });
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
      // Not part of the row; a caller may still send it.
      delete body.row.contentSha;
      body.row.generation = body.generation;
      body.row.accountingVersion = 0;
      // v1 is SEEDED as a version rather than living only on the row, so no
      // reader special-cases it. An earlier incarnation's history is cleared.
      const stale = [LEGACY_VERSIONS];
      const staleMaxVer = (await tx.get("maxVer")) ?? 0;
      for (let ver = 1; ver <= staleMaxVer; ver++) {
        stale.push(versionKey(ver), manifestKey(ver));
      }
      await deleteInBatches(tx, stale);
      const updates = new Map([
        ["row", body.row],
        ["createFingerprint", body.fingerprint],
        [versionKey(1), withUploadId({
          ver: 1, kind: body.row.kind, size: body.row.size,
          createdAt: body.row.createdAt, deleted: false,
        }, body.row.uploadId)],
        ["maxVer", 1],
      ]);
      if (body.row.manifest) {
        updates.set(manifestKey(1), body.row.manifest);
      }
      await tx.put(Object.fromEntries(updates));
      return new Response(null, { status: 204 });
    });
  }

  async abortCreate(body) {
    if (!requireShape(body, { generation: "string" })) {
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
      const writes = { roomLedger: ledger };
      // Only a room being created for the first time adds to the lifetime
      // count; a create replayed onto an existing room does not.
      if (body.fresh) {
        writes.roomTotal = (await this.roomTotal()) + 1;
      }
      await this.state.storage.put(writes);
    }
    return new Response(null, { status: 204 });
  }

  // Rooms this app has ever created; rooms are never deleted. An app older
  // than the counter starts from its rooms that hold bytes, which leaves out
  // only rooms that never stored a value.
  async roomTotal() {
    const total = await this.state.storage.get("roomTotal");
    if (total !== undefined) {
      return total;
    }
    return (await this.state.storage.list({ prefix: "roomAllocation:" })).size;
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
        publicKey: b64url(await rawPoint(pair.publicKey)),
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
    if (body.roomLimit > 0 && (await this.roomTotal()) >= body.roomLimit) {
      return Response.json({ error: "app-full", total }, { status: 507 });
    }
    return Response.json({ total });
  }

  // A room's byte decision against the app's ceiling and, when it raises the
  // app's total, its owner's quota (docs/SPEC.md "Room storage"). One decision
  // at a time: the owner charge is a round trip that must not interleave.
  async roomDecide(body) {
    if (!requireShape(body, { version: "posint", targetBytes: "uint" })) {
      return Response.json({ error: "invalid-budget-decision" }, { status: 400 });
    }
    return this.state.blockConcurrencyWhile(async () => {
      const storage = this.state.storage;
      const key = `roomAllocation:${body.room}`;
      const current = (await storage.get(key)) ?? { version: 0, allocated: 0, target: 0 };
      const total = (await storage.get("roomAllocated")) ?? 0;
      const decision = fence(current, total, body.version, body.targetBytes, body.appCap);
      if (!decision.record) {
        return Response.json(decision.body, { status: decision.status });
      }
      // Growth is charged only while room ceilings are enforced (appCap 0 is
      // none); a shrink, which rooms send without a ceiling, releases whatever
      // charge the owner holds.
      const changed = decision.body.granted && decision.nextTotal !== total;
      const moves = changed && (decision.nextTotal > total
        ? body.appCap > 0
        : (await storage.get("roomOwnerCharge")) !== undefined);
      const owner = moves ? await this.settleRoomOwner(body.app) : { identity: null };
      if (owner.unavailable) {
        return Response.json({ error: "owner-charge-unavailable" }, { status: 502 });
      }
      const refuse = async (error) => {
        await storage.put(key, { version: body.version, allocated: current.allocated, target: body.targetBytes });
        return Response.json({ granted: false, version: body.version, allocated: current.allocated, total, error }, { status: 507 });
      };
      const grows = moves && decision.nextTotal > total;
      if (grows && owner.identity) {
        const charged = await this.chargeRoomOwner(owner, decision.nextTotal);
        if (charged === "unavailable") {
          return Response.json({ error: "owner-charge-unavailable" }, { status: 502 });
        }
        if (charged === "refused") {
          return refuse("owner-over-quota");
        }
      }
      const updates = new Map([[key, decision.record]]);
      if (decision.body.granted && decision.nextTotal !== total) {
        updates.set("roomAllocated", decision.nextTotal);
      }
      await storage.put(Object.fromEntries(updates));
      if (decision.body.granted && decision.nextTotal < total && owner.identity) {
        // A failed release leaves the owner over-charged until the next decision
        // settles it, which the accounting allows.
        await this.chargeRoomOwner(owner, decision.nextTotal);
      }
      return Response.json(decision.body, { status: decision.status });
    });
  }

  // Seeds or refreshes the owner charge with no cap: bytes that predate the
  // owner quota, and an operator's repair.
  async roomChargeSync() {
    return this.state.blockConcurrencyWhile(async () => {
      const owner = await this.settleRoomOwner();
      if (owner.unavailable) {
        return Response.json({ error: "owner-charge-unavailable" }, { status: 502 });
      }
      return Response.json({
        owner: owner.identity !== null, roomAllocated: (await this.state.storage.get("roomAllocated")) ?? 0,
        charge: (await this.state.storage.get("roomOwnerCharge")) ?? null,
      });
    });
  }

  // Brings `roomOwnerCharge` in line with the row's owner and the committed
  // app total: replays an unsettled charge, seeds a first one, moves it on a
  // re-mint, and lowers an over-charge. Answers { identity, slug, charge } or
  // { unavailable } when the owner's cell could not be reached.
  async settleRoomOwner(appSlug) {
    const storage = this.state.storage;
    const row = await storage.get("row");
    const slug = row?.slug ?? appSlug ?? this.state.id?.name;
    let charge = (await storage.get("roomOwnerCharge")) ?? null;
    // A deleted app's rooms stay charged to its last owner; a slug that never
    // had one is bounded by the app ceiling alone.
    const identity = row?.identity ?? charge?.owner ?? null;
    const total = (await storage.get("roomAllocated")) ?? 0;
    if (!identity || !slug) {
      return { identity: null, slug, charge };
    }
    try {
      if (charge && charge.owner !== identity) {
        if (!await this.seedRoomOwner(charge.owner, slug, 0)) {
          return { unavailable: true };
        }
        charge = null;
      }
      if (charge && !charge.settled) {
        const res = await this.identityCall(identity, "roomcharge", {
          slug, version: charge.version, target: charge.target, userCap: USER_QUOTA_BYTES,
        });
        const answer = await res.json();
        if (res.status === 200 || res.status === 507) {
          charge = { ...charge, target: answer.allocated, settled: true };
          await storage.put("roomOwnerCharge", charge);
        } else if (res.status === 409) {
          charge = null;
        } else {
          return { unavailable: true };
        }
      }
      if (!charge) {
        const version = await this.seedRoomOwner(identity, slug, total);
        if (!version) {
          return { unavailable: true };
        }
        charge = { owner: identity, version, target: total, settled: true };
        await storage.put("roomOwnerCharge", charge);
      }
    } catch {
      return { unavailable: true };
    }
    const settled = { identity, slug, charge };
    if (charge.target > total && await this.chargeRoomOwner(settled, total) === "unavailable") {
      return { unavailable: true };
    }
    return settled;
  }

  // The new version of a seeded charge, or 0 when the owner's cell failed.
  async seedRoomOwner(identity, slug, target) {
    const res = await this.identityCall(identity, "roomcharge", { slug, seed: true, target });
    if (res.status !== 200) {
      await res.body?.cancel();
      return 0;
    }
    return (await res.json()).version;
  }

  // Moves the owner charge to target at the next version, recording it
  // unsettled first so a lost answer is replayed: "granted", "refused" or
  // "unavailable".
  async chargeRoomOwner(owner, target) {
    const storage = this.state.storage;
    const next = { owner: owner.identity, version: owner.charge.version + 1, target, settled: false };
    await storage.put("roomOwnerCharge", next);
    owner.charge = next;
    let res;
    let answer;
    try {
      res = await this.identityCall(owner.identity, "roomcharge", {
        slug: owner.slug, version: next.version, target, userCap: USER_QUOTA_BYTES,
      });
      answer = await res.json();
    } catch {
      return "unavailable";
    }
    if (res.status !== 200 && res.status !== 507) {
      return "unavailable";
    }
    owner.charge = { ...next, target: answer.allocated, settled: true };
    await storage.put("roomOwnerCharge", owner.charge);
    return answer.granted ? "granted" : "refused";
  }

  async get() {
    const row = await this.state.storage.get("row");
    if (!row) {
      return new Response("not found\n", { status: 404 });
    }
    // Not 404: a caller taking the slug for absent would heal its owner's
    // listing entry away while the row still exists.
    if (await this.state.storage.get("quarantine")) {
      return Response.json({ error: "quarantined", identity: row.identity }, { status: 451 });
    }
    return Response.json(row);
  }

  // The row and any quarantine record, for operator tools: the takedown of a
  // quarantined slug needs the row its get withholds.
  async operatorRow() {
    const [row, quarantine] = await Promise.all(["row", "quarantine"].map((k) => this.state.storage.get(k)));
    if (!row) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ row, quarantine: quarantine ?? null });
  }

  // A quarantined slug keeps its row, versions and bytes; only reads stop.
  // The first record stands, so a later quarantine cannot rewrite its reason.
  async quarantine(body) {
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ error: "no-such-paste" }, { status: 404 });
    }
    const prior = await this.state.storage.get("quarantine");
    if (!prior) {
      await this.state.storage.put("quarantine", { reason: body.reason, actor: body.actor, at: body.at });
    }
    return Response.json({ identity: row.identity, already: Boolean(prior) });
  }

  async release() {
    const released = (await this.state.storage.get("quarantine")) !== undefined;
    if (released) {
      await this.state.storage.delete("quarantine");
    }
    return Response.json({ released });
  }

  // The rules run here, so neither a large manifest nor any other row field
  // leaves the cell: only the answer does (read.js).
  async resolve(url) {
    const path = url.searchParams.get("path");
    if (!path?.startsWith("/")) {
      return Response.json({ error: "invalid-path" }, { status: 400 });
    }
    const [row, quarantine] = await Promise.all(["row", "quarantine"].map((k) => this.state.storage.get(k)));
    return Response.json(resolveRead(quarantine ? undefined : row, {
      path,
      raw: url.searchParams.get("raw") === "1",
      files: url.searchParams.get("files") === "1",
    }));
  }

  // Presence, for routes that need no manifest.
  async exists() {
    const [row, quarantine] = await Promise.all(["row", "quarantine"].map((k) => this.state.storage.get(k)));
    return Response.json({ exists: row !== undefined && !quarantine });
  }

  requireGeneration(row, body) {
    if (row.generation !== body.generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
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

  identityCall(owner, op, body) {
    return cellCall(this.env.IDENTITY, owner, `/identity/${op}`, "scope", body);
  }

  receiptKey(generation, opId) {
    return `${ARTIFACT_RECEIPT_PREFIX}${generation}:${opId}`;
  }

  receiptResponse(receipt) {
    return Response.json(receipt.body, { status: receipt.status });
  }

  // A version written before reuse has no charge and shares nothing, so its
  // size is what it occupies.
  liveCharge(versions) {
    return versions.reduce((sum, version) => version.deleted ? sum : sum + chargeOf(version), 0);
  }

  // Backoff bounds the commits a stuck operation costs.
  async retryArtifactPending(result = { outcome: "unavailable" }) {
    await this.state.storage.transaction(async (tx) => {
      const pending = await tx.get(ARTIFACT_PENDING);
      if (!pending) {
        return;
      }
      const attempts = (pending.attempts ?? 0) + 1;
      await tx.put(ARTIFACT_PENDING, { ...pending, attempts });
      await tx.setAlarm(Date.now() + retryDelay(attempts));
    });
    return result;
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

  // Runs the pending operation to its end or its next retry: asks the cell its
  // stage calls, turns the answer into an event, and performs the effect
  // nextPending names (artifact-saga.js).
  async resumeArtifactPending(rearm = false) {
    let pending = await this.state.storage.get(ARTIFACT_PENDING);
    if (!pending) {
      return { outcome: "complete" };
    }
    // Re-armed at the next backoff deadline, not now: a new deadline resets the
    // runtime's own retry state, so a handler killed mid-call would otherwise
    // fire again at once.
    if (rearm) {
      await this.state.storage.setAlarm(Date.now() + retryDelay((pending.attempts ?? 0) + 1));
    }
    for (;;) {
      const event = await this.artifactStageEvent(pending);
      const step = nextPending(pending, event);
      switch (step.effect) {
        case "retry":
          return this.retryArtifactPending({ outcome: step.outcome });
        case "settleAbsent":
          return (await this.settleAbsentArtifact(pending)) ? { outcome: "complete" } : this.retryArtifactPending();
        case "refuse":
          return this.refuseArtifact(pending);
        case "publish":
          try {
            await this.publishAppend(pending);
          } catch (error) {
            if (!isTooBig(error)) {
              throw error;
            }
            await this.abandonAppend(pending);
            return this.resumeArtifactPending();
          }
          break;
        case "advance":
          await this.state.storage.transaction(async (tx) => {
            const row = await tx.get("row");
            if (row) {
              row.accountingVersion = pending.version;
              await tx.put("row", row);
            }
            await tx.put(ARTIFACT_PENDING, advance(pending));
          });
          break;
        case "clear":
          await this.clearArtifactPending();
          return { outcome: step.outcome };
        case "finishDrop":
          await this.finishArtifactDrop(pending);
          return (await this.state.storage.get(ARTIFACT_PENDING))
            ? this.retryArtifactPending()
            : { outcome: step.outcome };
        default:
          throw new Error(`unknown artifact effect ${step.effect}`);
      }
      pending = await this.state.storage.get(ARTIFACT_PENDING);
      if (!pending) {
        return { outcome: "complete" };
      }
    }
  }

  // The event the pending operation's stage call produces.
  async artifactStageEvent(pending) {
    try {
      switch (pending.stage) {
        case DECIDE: {
          const response = await this.identityCall(pending.owner, "artifactdecide", {
            slug: pending.slug,
            generation: pending.generation,
            version: pending.version,
            target: pending.target,
            userCap: pending.userCap,
          });
          return decisionEvent(pending, response.status, await response.json());
        }
        case PROJECT: {
          const row = await this.state.storage.get("row");
          if (!row) {
            return "noRow";
          }
          const response = await this.identityCall(pending.owner, "artifactproject",
            this.artifactProjection(row, pending));
          return response.ok ? "projected" : "conflict";
        }
        case DROP: {
          const response = await this.identityCall(pending.owner, "artifactdrop", {
            slug: pending.slug,
            generation: pending.generation,
            version: pending.version,
          });
          return response.ok ? "dropped" : "conflict";
        }
        default:
          return "invalid";
      }
    } catch {
      return "unreachable";
    }
  }

  // Identity no longer lists the artifact, so a remove or fail finishes
  // without a drop. False when the cell moved on meanwhile.
  async settleAbsentArtifact(pending) {
    return this.state.storage.transaction(async (tx) => {
      if (!samePending(await tx.get(ARTIFACT_PENDING), pending, DECIDE)) {
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
  }

  async refuseArtifact(pending) {
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

  async finishArtifactDrop(pending) {
    await this.state.storage.transaction(async (tx) => {
      if (!samePending(await tx.get(ARTIFACT_PENDING), pending, DROP)) {
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
  }

  async publishAppend(pending) {
    await this.state.storage.transaction(async (tx) => {
      const current = await tx.get(ARTIFACT_PENDING);
      if (!samePending(current, pending, DECIDE)) {
        return;
      }
      await splitLegacyVersions(tx);
      const row = await tx.get("row");
      const { manifest, ...version } = pending.mutation;
      const versions = await loadVersions(tx);
      versions.push(version);
      row.updatedAt = version.createdAt ?? row.updatedAt;
      row.accountingVersion = pending.version;
      await this.rollServed(tx, row, versions, pending.mutation);
      const receipt = {
        status: 200,
        body: {
          appended: true,
          ver: version.ver,
          wasPinned: pending.wasPinned,
          totalSize: pending.target,
        },
      };
      const updates = new Map([
        ["row", row],
        [versionKey(version.ver), version],
        ["maxVer", version.ver],
        [ARTIFACT_PENDING, advance(current)],
        [this.receiptKey(pending.generation, pending.opId), receipt],
      ]);
      if (manifest) {
        updates.set(manifestKey(version.ver), manifest);
      } else {
        await tx.delete(manifestKey(version.ver));
      }
      await tx.put(Object.fromEntries(updates));
    });
  }

  // The grant already stands at pending.version, so the release is the next
  // version at the charge without this append.
  async abandonAppend(pending) {
    const latestVersion = (await this.state.storage.get("maxVer")) ?? 1;
    await this.state.storage.transaction(async (tx) => {
      const current = await tx.get(ARTIFACT_PENDING);
      if (!samePending(current, pending, DECIDE)) {
        return;
      }
      const { mutation, ...rest } = current;
      await tx.put(Object.fromEntries([
        [ARTIFACT_PENDING, {
          ...rest,
          kind: "abandon",
          version: current.version + 1,
          target: current.target - chargeOf(mutation),
          latestVersion,
          attempts: 0,
        }],
        [this.receiptKey(current.generation, current.opId),
          { status: 413, body: { error: "version-too-large" } }],
      ]));
      await tx.setAlarm(Date.now());
    });
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
    const pending = startPending("projection", {
      opId: "",
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: row.accountingVersion ?? 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    });
    await this.state.storage.transaction(async (tx) => {
      await tx.put(Object.fromEntries([["row", row], [ARTIFACT_PENDING, pending]]));
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
    // No row lacks a generation, so an empty one names nothing.
    if (!generation) {
      return Response.json({ error: "generation-mismatch" }, { status: 409 });
    }
    const receipt = await this.state.storage.get(this.receiptKey(generation, opId));
    if (receipt) {
      return this.receiptResponse(receipt);
    }
    const pending = await this.state.storage.get(ARTIFACT_PENDING);
    if (pending) {
      // Another operation holds the cell: this request persisted nothing and
      // never will, which 423 tells apart from its own unresolved work (502).
      if (pending.generation !== generation || pending.opId !== opId) {
        return Response.json({ error: "artifact-operation-pending" }, { status: 423 });
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
    if (!requireShape(body, { size: "uint" })) {
      return Response.json({ error: "invalid-size" }, { status: 400 });
    }
    if (!isUploadId(body.uploadId)) {
      return Response.json({ error: "invalid-upload-id" }, { status: 400 });
    }
    const row = await this.state.storage.get("row");
    if (!row) {
      return Response.json({ appended: false, reason: "absent" });
    }
    const refused = this.requireGeneration(row, body);
    if (refused) {
      return refused;
    }
    const versions = await loadVersions(this.state.storage);
    const nextVer = ((await this.state.storage.get("maxVer")) ?? 1) + 1;
    let charge = body.size;
    if (body.base !== undefined) {
      const reuse = await this.reuseCharge(versions, body);
      if (reuse.error) {
        return Response.json({ error: reuse.error }, { status: 409 });
      }
      charge = reuse.charge;
    }
    const target = this.liveCharge(versions) + charge;
    const pending = startPending("append", {
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: body.userCap ?? 0,
      latestVersion: nextVer,
      wasPinned: (row.pinnedVersion ?? 0) !== 0,
      mutation: withUploadId({
        ver: nextVer,
        kind: body.kind,
        size: body.size,
        ...(body.base !== undefined ? { charge } : {}),
        createdAt: body.now,
        deleted: false,
        manifest: body.manifest ?? null,
      }, body.uploadId),
    });
    try {
      await this.state.storage.transaction(async (tx) => {
        await tx.put(ARTIFACT_PENDING, pending);
        await tx.setAlarm(Date.now());
      });
    } catch (error) {
      if (!isTooBig(error)) {
        throw error;
      }
      return Response.json({ error: "version-too-large" }, { status: 413 });
    }
    const result = await this.resumeArtifactPending();
    const receipt = await this.state.storage.get(this.receiptKey(body.generation, body.opId));
    if (receipt) {
      return this.receiptResponse(receipt);
    }
    if (result.outcome === "conflict") {
      return Response.json({ error: "artifact-accounting-conflict" }, { status: 409 });
    }
    return Response.json({ error: "artifact-accounting-unavailable" }, { status: 502 });
  }

  // An append computed against base may point at base's standalone objects
  // and charges only the objects base does not hold (docs/SPEC.md "Redeploy
  // reuse"). The pending operation this append writes blocks every other
  // mutation until it settles, so base is still the head when it publishes.
  async reuseCharge(versions, body) {
    const head = latestLive(versions);
    if (!Number.isSafeInteger(body.base) || !head || head.ver !== body.base) {
      return { error: "base-moved" };
    }
    const base = manifestObjects(await loadManifest(this.state.storage, head.ver));
    const own = `uploads/${body.uploadId}/`;
    let charge = 0;
    for (const [key, object] of manifestObjects(body.manifest)) {
      if (key.startsWith(own)) {
        charge += object.size;
        continue;
      }
      const shared = base.get(key);
      if (!shared || shared.packed || object.packed) {
        return { error: "bad-reuse" };
      }
    }
    return { charge };
  }

  // The latest live version: the head a redeploy is computed against.
  async head() {
    const head = latestLive(await loadVersions(this.state.storage));
    if (!head) {
      return Response.json({ found: false });
    }
    return Response.json({
      found: true, ver: head.ver, manifest: (await loadManifest(this.state.storage, head.ver)) ?? null,
    });
  }

  // NEWEST FIRST, which is the order every reader wants and none should have to
  // impose: a listing sorted by insertion leaks the storage order into the UI.
  // Metadata only: a manifest per version would make the answer, and the cell's
  // memory while building it, grow with history times site size.
  async listVersions() {
    const versions = await loadVersions(this.state.storage);
    return Response.json(versions
      .map((version) => ({ ...version, uploadId: version.uploadId ?? "" }))
      .sort((a, b) => b.ver - a.ver));
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
    const refused = this.requireGeneration(row, body);
    if (refused) {
      return refused;
    }
    const versions = await loadVersions(this.state.storage);
    const version = versions.find((candidate) => candidate.ver === body.ver);
    if (!version) {
      return Response.json({ deleted: false, reason: "absent" });
    }
    // Its bytes were named by the delete that tombstoned it; naming them again
    // could delete objects a later version has since come to share.
    if (version.deleted) {
      return Response.json({
        deleted: true, totalSize: this.liveCharge(versions), upload: "", prefixes: [], keys: [], freed: 0,
      });
    }
    const live = versions.filter((candidate) => !candidate.deleted);
    const served = row.pinnedVersion ||
      live.reduce((latest, candidate) => Math.max(latest, candidate.ver), 0);
    if (body.ver === served) {
      return Response.json({ error: "version-served" }, { status: 409 });
    }

    const before = this.liveCharge(versions);
    const freeing = await this.freedBy(versions, version);
    version.deleted = true;
    if (row.pinnedVersion === body.ver) {
      row.pinnedVersion = 0;
    }
    await this.rollServed(this.state.storage, row, versions);
    const target = this.liveCharge(versions);
    const receipt = {
      status: 200,
      body: {
        deleted: true,
        totalSize: target,
        // A whole-prefix delete of this version's upload, for a caller that
        // reads only this field.
        upload: freeing.prefixes.includes(uploadPrefix(version.uploadId)) ? version.uploadId : "",
        prefixes: freeing.prefixes,
        keys: freeing.keys,
        freed: before - target,
      },
    };
    const pending = startPending("deleteVersion", {
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    });
    await this.state.storage.transaction(async (tx) => {
      await splitLegacyVersions(tx);
      await tx.put(Object.fromEntries([
        [versionKey(version.ver), version],
        ...(freeing.next ? [[versionKey(freeing.next.ver), freeing.next]] : []),
        ["row", row],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  // What deleting version frees, from its live neighbours alone: the live
  // versions sharing an object are one unbroken run (docs/SPEC.md "Redeploy
  // reuse"). The next live version's charge is recomputed against its new
  // predecessor, in versions, and returned to be stored.
  async freedBy(versions, version) {
    const live = versions.filter((v) => !v.deleted).sort((a, b) => a.ver - b.ver);
    const prev = live.filter((v) => v.ver < version.ver).at(-1);
    const next = live.find((v) => v.ver > version.ver);
    const objects = async (v) => v ? manifestObjects(await loadManifest(this.state.storage, v.ver)) : new Map();
    const [mine, before, after] = [await objects(version), await objects(prev), await objects(next)];
    const deleted = new Set(versions.filter((v) => v.deleted || v.ver === version.ver)
      .map((v) => v.uploadId).filter(Boolean));
    const held = (prefix) => [...before.keys(), ...after.keys()].some((key) => key.startsWith(prefix));
    const freedKeys = [...mine].filter(([key]) => !before.has(key) && !after.has(key));
    const candidates = new Set(freedKeys.map(([key]) => prefixOf(key)).filter(Boolean));
    if (version.uploadId) {
      candidates.add(uploadPrefix(version.uploadId));
    }
    const prefixes = [...candidates]
      .filter((prefix) => deleted.has(prefix.slice("uploads/".length, -1)) && !held(prefix))
      .sort();
    const keys = freedKeys.map(([key]) => key)
      .filter((key) => !prefixes.includes(prefixOf(key)))
      .sort();
    let changed = null;
    if (next && (next.charge !== undefined || freedKeys.length < mine.size)) {
      let charge = 0;
      for (const [key, object] of after) {
        if (!before.has(key)) {
          charge += object.size;
        }
      }
      next.charge = charge;
      changed = next;
    }
    return { prefixes, keys, next: changed };
  }

  // Roll the row onto the version the public URL serves: the pin when set,
  // otherwise the newest live one. The row's kind, upload id, manifest and size
  // are a VIEW of that version, so every mutation that can change which version
  // is served has to pass through here or the view goes stale.
  //
  // row.size is the SERVED version's size, NOT the sum of live versions. The
  // quota total is a different number, kept in the identity cell, and conflating
  // them makes a multi-version paste report its whole history as its size.
  async rollServed(storage, row, versions, fresh = null) {
    const live = versions.filter((v) => !v.deleted);
    // A stored contentSha would describe a version that may no longer be served.
    delete row.contentSha;
    if (!live.length) {
      row.uploadId = "";
      row.manifest = null;
      row.size = 0;
      row.pinnedVersion = 0;
      return;
    }
    const v = servedOf(row, live);
    if (row.pinnedVersion && v.ver !== row.pinnedVersion) {
      row.pinnedVersion = 0;
    }
    row.uploadId = v.uploadId ?? "";
    row.kind = v.kind;
    row.manifest = v.ver === fresh?.ver
      ? fresh.manifest ?? null
      : await loadManifest(storage, v.ver);
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
    const refused = this.requireGeneration(row, body);
    if (refused) {
      return refused;
    }
    const versions = await loadVersions(this.state.storage);
    const wanted = body.ver ?? 0;
    if (wanted !== 0 && !versions.some((version) => version.ver === wanted && !version.deleted)) {
      return Response.json({ pinned: false, reason: "absent" });
    }
    row.pinnedVersion = wanted;
    await this.rollServed(this.state.storage, row, versions);
    const target = this.liveCharge(versions);
    const receipt = { status: 200, body: { pinned: true, ver: wanted } };
    const pending = startPending("pin", {
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    });
    await this.state.storage.transaction(async (tx) => {
      await tx.put(Object.fromEntries([
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
    const refused = this.requireGeneration(row, body);
    if (refused) {
      return refused;
    }
    const version = (row.accountingVersion ?? 0) + 1;
    const pending = startPending("remove", {
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version,
      target: 0,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    });
    // The ids are taken inside the removal: once the row is gone the slug can be
    // re-created, and a later read would name the new incarnation's uploads.
    const receipt = await this.state.storage.transaction(async (tx) => {
      const uploads = (await loadVersions(tx))
        .sort((a, b) => a.ver - b.ver)
        .map((v) => v.uploadId)
        .filter(Boolean);
      const removed = { status: 200, body: { removed: true, uploads } };
      // A re-minted slug must not inherit the old incarnation's quarantine.
      await tx.delete(["row", "quarantine"]);
      await tx.put(Object.fromEntries([
        ["artifactTombstone", {
          slug: row.slug,
          identity: row.identity,
          createdAt: row.createdAt,
          generation: row.generation,
          accountingVersion: version,
        }],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), removed],
      ]));
      await tx.setAlarm(Date.now());
      return removed;
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
    const refused = this.requireGeneration(row, body);
    if (refused) {
      return refused;
    }
    if (!["pending", "failed"].includes(row.status)) {
      return Response.json({ changed: false, reason: "not-pending", status: row.status });
    }
    row.status = "failed";
    const receipt = { status: 200, body: { changed: true, status: "failed" } };
    const pending = startPending("fail", {
      opId: body.opId,
      slug: row.slug,
      owner: row.identity,
      generation: row.generation,
      version: (row.accountingVersion ?? 0) + 1,
      target: 0,
      userCap: 0,
      latestVersion: (await this.state.storage.get("maxVer")) ?? 1,
    });
    await this.state.storage.transaction(async (tx) => {
      await tx.put(Object.fromEntries([
        ["row", row],
        [ARTIFACT_PENDING, pending],
        [this.receiptKey(body.generation, body.opId), receipt],
      ]));
      await tx.setAlarm(Date.now());
    });
    await this.resumeArtifactPending();
    return this.receiptResponse(receipt);
  }

  // A thrown alarm would fall to the runtime's own retry, which neither backs
  // off far enough nor keeps the operation's state.
  async alarm() {
    await this.state.blockConcurrencyWhile(async () => {
      try {
        await this.resumeArtifactPending(true);
      } catch (error) {
        console.error(`paste alarm: ${error}`);
        await this.retryArtifactPending();
      }
    });
  }

  // Only a still-PENDING row transitions, so a late finalizer cannot resurrect
  // a paste the reconciler already failed, and a repeat is harmless.
  async setStatus(body) {
    if (!requireShape(body, { generation: "string" })) {
      return Response.json({ error: "missing-generation" }, { status: 400 });
    }
    if (body.status === "failed") {
      return this.fail(body);
    }
    // A pending row moves only to ready or failed (docs/SPEC.md "Paste lifecycle status").
    if (body.status !== "ready") {
      return Response.json({ error: "invalid-status" }, { status: 400 });
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
