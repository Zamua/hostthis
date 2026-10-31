import { MAX_PUT_ENTRIES } from "./image.js";

// The owner identity scopes intents, quota reservations, and the paste index.
// Paste rows use slug-scoped cells, so cross-cell creates recover through the
// owner's durable intent log.

// A create intent younger than this is an in-flight create, not a crash.
// Recovery before the Go side has written the Paste row would fence and
// release a create that is about to succeed.
export const CREATE_INTENT_GRACE_MS = 30_000;

export const INTENT_PREFIX = "intent:";
const CREATE_ABORT_PREFIX = "create-abort:";
const ARTIFACT_ACCOUNT_PREFIX = "artifact-account:";
export const ARTIFACT_PENDING = "artifactPending";
export const ARTIFACT_RECEIPT_PREFIX = "artifact-receipt:";
const VERSION_PREFIX = "ver:";
const MANIFEST_PREFIX = "manifest:";
export const LEGACY_VERSIONS = "versions";
const RETRY_BASE_MS = 1000;
const RETRY_MAX_MS = 5 * 60_000;

export const ROOM_CHARGE_PREFIX = "roomCharge:";

// An app's room bytes as its owner's Identity cell holds them.
export function roomChargeKey(slug) {
  return ROOM_CHARGE_PREFIX + slug;
}

export function artifactAccountKey(slug, generation) {
  return `${ARTIFACT_ACCOUNT_PREFIX}${slug}:${generation}`;
}

export function chargedSize(entry) {
  return entry.chargedSize ?? entry.size ?? 0;
}

export function intentKey(id) {
  return INTENT_PREFIX + id;
}

export function createAbortKey(generation) {
  return CREATE_ABORT_PREFIX + generation;
}

export function versionKey(ver) {
  return VERSION_PREFIX + ver;
}

export function manifestKey(ver) {
  return MANIFEST_PREFIX + ver;
}

export function retryDelay(attempts) {
  return Math.min(RETRY_BASE_MS * 2 ** (attempts - 1), RETRY_MAX_MS);
}

// SQLite refuses an oversized value identically on every retry.
export function isTooBig(error) {
  return String(error?.message ?? error).includes("too big");
}

// Cell storage caps one value near 2 MiB, so a history is one key per version
// with each manifest apart. Reads accept a legacy single-value history until a
// version write splits it.
export async function loadVersions(storage) {
  const legacy = await storage.get(LEGACY_VERSIONS);
  if (legacy) {
    return legacy.map(({ manifest: _manifest, ...version }) => version);
  }
  const maxVer = (await storage.get("maxVer")) ?? 0;
  const found = await Promise.all(
    Array.from({ length: maxVer }, (_, i) => storage.get(versionKey(i + 1))));
  return found.filter(Boolean);
}

export function chargeOf(version) {
  return version.charge ?? version.size ?? 0;
}

export function latestLive(versions) {
  return versions.reduce((latest, v) => !v.deleted && (!latest || v.ver > latest.ver) ? v : latest, null);
}

export function uploadPrefix(uploadId) {
  return `uploads/${uploadId}/`;
}

// The upload prefix an object key lives under, or "" for a key outside one.
export function prefixOf(key) {
  const match = /^uploads\/[^/]+\//.exec(key);
  return match ? match[0] : "";
}

export async function loadManifest(storage, ver) {
  const legacy = await storage.get(LEGACY_VERSIONS);
  if (legacy) {
    return legacy.find((version) => version.ver === ver)?.manifest ?? null;
  }
  return (await storage.get(manifestKey(ver))) ?? null;
}

// Cloudflare refuses a put or delete of more than MAX_PUT_ENTRIES keys at once;
// inside a transaction the batches still land as one.
export async function putInBatches(tx, entries) {
  const list = [...entries];
  for (let i = 0; i < list.length; i += MAX_PUT_ENTRIES) {
    await tx.put(Object.fromEntries(list.slice(i, i + MAX_PUT_ENTRIES)));
  }
}

export async function deleteInBatches(tx, keys) {
  for (let i = 0; i < keys.length; i += MAX_PUT_ENTRIES) {
    await tx.delete(keys.slice(i, i + MAX_PUT_ENTRIES));
  }
}

export async function splitLegacyVersions(tx) {
  const legacy = await tx.get(LEGACY_VERSIONS);
  if (!legacy) {
    return;
  }
  const updates = new Map();
  for (const { manifest, ...version } of legacy) {
    updates.set(versionKey(version.ver), version);
    if (manifest) {
      updates.set(manifestKey(version.ver), manifest);
    }
  }
  updates.set("maxVer", Math.max((await tx.get("maxVer")) ?? 0, ...legacy.map((v) => v.ver)));
  await putInBatches(tx, updates);
  await tx.delete(LEGACY_VERSIONS);
}

// A caller that predates upload ids sends none, and its versions record none.
export function isUploadId(value) {
  return value === undefined || value === null || typeof value === "string";
}

export function withUploadId(version, uploadId) {
  return uploadId ? { ...version, uploadId } : version;
}

// The pin when it is live, otherwise the newest live version.
export function servedOf(row, live) {
  return (row.pinnedVersion ? live.find((v) => v.ver === row.pinnedVersion) : null) ??
    live.reduce((a, b) => (b.ver > a.ver ? b : a));
}

export function setOwn(object, key, value) {
  Object.defineProperty(object, key, {
    value,
    enumerable: true,
    configurable: true,
    writable: true,
  });
}

// The version fence shared by the owner and app allocation ledgers. A decision
// below the current version is stale, at it a replay that must repeat the
// recorded target, above it by more than one a gap. Only the next version
// decides; the caller persists record and nextTotal when they are present.
export function fence(current, total, version, target, cap) {
  const refuse = (error) => ({
    status: 409,
    body: { error, version: current.version, allocated: current.allocated, total },
  });
  if (version < current.version) {
    return refuse("stale-version");
  }
  if (version === current.version) {
    if (target !== current.target) {
      return refuse("version-target-mismatch");
    }
    const granted = current.target === current.allocated;
    return {
      status: granted ? 200 : 507,
      body: { granted, version: current.version, allocated: current.allocated, total },
    };
  }
  if (version !== current.version + 1) {
    return refuse("version-gap");
  }
  const nextTotal = total - current.allocated + target;
  const granted = target <= current.allocated || cap <= 0 || nextTotal <= cap;
  const record = { version, allocated: granted ? target : current.allocated, target };
  return {
    status: granted ? 200 : 507,
    body: { granted, version, allocated: record.allocated, total: granted ? nextTotal : total },
    record,
    nextTotal,
  };
}

// A stored intent is a private shape, so the Go port's type can change without
// a stored-format migration. Written from the intent hostthisd sends on reserve.
export function toRow(body) {
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
