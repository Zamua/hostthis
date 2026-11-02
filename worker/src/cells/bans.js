import { purgeSlugs } from "../abuse-routes.js";
import { deleteUploadPrefix } from "../blobs.js";

// Owner bans (docs/SPEC.md "Owner ban"): the Abuse cell's records and the
// alarm work that quarantines, sweeps, deletes or releases an owner's slugs.
// Work is done in small batches per alarm run so no invocation fans out past
// the platform's subrequest limits, however many slugs an owner has.

export const BAN = "b:";
const BAN_BY_OWNER = "bo:";
const DAY_MS = 86_400_000;
const FIRST_SWEEP_MS = 60_000;
const SWEEP_EVERY_MS = 3_600_000;
const REPURGE_MS = 15_000;
const QUARANTINE_BATCH = 10;
const DELETE_BATCH = 5;
const RELEASE_BATCH = 10;
export const RETENTION_DAYS = { default: 7, min: 1, max: 90 };

const CONTINUE_MS = 1000;

// Slug states within a ban: `pending` awaits quarantine, `banned` was
// quarantined by this ban, `own` was already quarantined on its own, `gone`
// had no row, `deleted` and `failed` record the deletion pass, `released` the
// unban.
const DELETABLE = new Set(["pending", "banned", "own", "failed"]);

function paste(env, slug) {
  return env.PASTES.get(env.PASTES.idFromName(slug));
}

async function pasteCall(env, slug, op, body) {
  const q = `slug=${encodeURIComponent(slug)}`;
  const init = body === undefined ? {} : { method: "POST", body: JSON.stringify(body) };
  return paste(env, slug).fetch(new Request(`https://cell/paste/${op}?${q}`, init));
}

export async function ownerSlugs(env, identity) {
  const res = await env.IDENTITY.get(env.IDENTITY.idFromName(identity)).fetch(new Request(
    `https://cell/identity/list?scope=${encodeURIComponent(identity)}`));
  if (!res.ok) {
    throw new Error(`identity list answered ${res.status}`);
  }
  return (await res.json()).map((e) => e.slug);
}

export async function openBanId(storage, ownerHash) {
  return storage.get(BAN_BY_OWNER + ownerHash);
}

export async function clearOpenBan(storage, ownerHash) {
  await storage.delete(BAN_BY_OWNER + ownerHash);
}

export async function createBan(storage, { banId, ownerHash, identity, reason, now, retentionDays, slugs }) {
  const record = {
    banId, owner: ownerHash, identity, reason, at: now,
    deleteAfter: now + retentionDays * DAY_MS,
    state: "active",
    slugs: Object.fromEntries(slugs.map((s) => [s, "pending"])),
    nextSweep: now + FIRST_SWEEP_MS,
    repurge: [],
  };
  await storage.put({ [BAN + banId]: record, [BAN_BY_OWNER + ownerHash]: banId });
  return record;
}

export function summary(record) {
  const counts = {};
  for (const state of Object.values(record.slugs)) {
    counts[state] = (counts[state] ?? 0) + 1;
  }
  return {
    banId: record.banId, owner: record.owner, reason: record.reason, at: record.at,
    deleteAfter: record.deleteAfter, state: record.state,
    slugs: Object.keys(record.slugs).length,
    quarantined: (counts.banned ?? 0) + (counts.own ?? 0),
    deleted: counts.deleted ?? 0,
    failed: Object.entries(record.slugs).filter(([, s]) => s === "failed").map(([slug]) => slug),
  };
}

// Quarantines up to a batch of pending slugs; returns the slugs to purge.
async function quarantinePending(cell, record, now) {
  const purge = [];
  const pending = Object.keys(record.slugs).filter((s) => record.slugs[s] === "pending").slice(0, QUARANTINE_BATCH);
  for (const slug of pending) {
    const row = await pasteCall(cell.env, slug, "operatorrow");
    if (row.status === 404) {
      record.slugs[slug] = "gone";
      continue;
    }
    if (!row.ok) {
      continue; // retried next pass
    }
    const { quarantine } = await row.json();
    if (quarantine) {
      record.slugs[slug] = "own";
      continue;
    }
    const reason = `owner banned: ${record.reason}`.slice(0, 200);
    const res = await pasteCall(cell.env, slug, "quarantine", { reason, actor: `ban:${record.banId}`, at: now });
    if (!res.ok) {
      continue;
    }
    record.slugs[slug] = "banned";
    await cell.indexQuarantine(slug, { owner: record.identity, reason, actor: `ban:${record.banId}`, at: now });
    purge.push(slug);
  }
  return purge;
}

async function sweep(cell, record, now) {
  for (const slug of await ownerSlugs(cell.env, record.identity)) {
    if (!(slug in record.slugs)) {
      record.slugs[slug] = "pending";
    }
  }
  record.nextSweep = now + SWEEP_EVERY_MS;
}

// Deletes as the owner's own delete would: the row, every version's upload
// prefix, then the cached host.
async function deleteSlug(env, slug, banId) {
  const got = await pasteCall(env, slug, "operatorrow");
  if (got.status === 404) {
    return "deleted";
  }
  if (!got.ok) {
    return "failed";
  }
  const { row } = await got.json();
  const res = await pasteCall(env, slug, "remove", {
    createdAt: row.createdAt, generation: row.generation, identity: row.identity, opId: `ban:${banId}:${slug}`,
  });
  const answer = await res.json().catch(() => ({}));
  if (!res.ok || !answer.removed) {
    return "failed";
  }
  for (const upload of answer.uploads ?? []) {
    await deleteUploadPrefix(env, `uploads/${upload}/`);
  }
  return "deleted";
}

async function deleteBatch(cell, record) {
  const purge = [];
  const due = Object.keys(record.slugs).filter((s) => DELETABLE.has(record.slugs[s])).slice(0, DELETE_BATCH);
  for (const slug of due) {
    let outcome;
    try {
      outcome = await deleteSlug(cell.env, slug, record.banId);
    } catch (error) {
      console.warn(`warn: ban ${record.banId} delete ${slug}: ${error?.message ?? error}`);
      outcome = "failed";
    }
    record.slugs[slug] = outcome;
    if (outcome === "deleted") {
      await cell.unindexQuarantine(slug);
      purge.push(slug);
    }
  }
  return { purge, more: Object.values(record.slugs).some((s) => DELETABLE.has(s)) };
}

async function releaseBatch(cell, record) {
  const purge = [];
  const due = Object.keys(record.slugs).filter((s) => record.slugs[s] === "banned" || record.slugs[s] === "pending")
    .slice(0, RELEASE_BATCH);
  for (const slug of due) {
    if (record.slugs[slug] === "banned") {
      const res = await pasteCall(cell.env, slug, "release", {});
      if (!res.ok) {
        continue;
      }
      await cell.unindexQuarantine(slug);
      purge.push(slug);
    }
    record.slugs[slug] = "released";
  }
  return { purge, more: Object.values(record.slugs).some((s) => s === "banned" || s === "pending") };
}

// Advances one ban; returns when it next needs the alarm (null: never).
export async function advance(cell, record, now) {
  const purge = [...record.repurge];
  record.repurge = [];
  let fresh = [];
  let next = null;
  if (record.state === "active" && now >= record.deleteAfter) {
    record.state = "deleting";
  }
  if (record.state === "active") {
    if (now >= record.nextSweep) {
      await sweep(cell, record, now);
    }
    fresh = await quarantinePending(cell, record, now);
    const pendingLeft = Object.values(record.slugs).includes("pending");
    next = Math.min(pendingLeft ? now + CONTINUE_MS : record.nextSweep, record.deleteAfter);
  } else if (record.state === "deleting") {
    const { purge: gone, more } = await deleteBatch(cell, record);
    purge.push(...gone);
    if (more) {
      next = now + CONTINUE_MS;
    } else {
      record.state = "deleted";
      delete record.identity;
      await clearOpenBan(cell.state.storage, record.owner);
    }
  } else if (record.state === "lifting") {
    const { purge: released, more } = await releaseBatch(cell, record);
    fresh = released;
    if (more) {
      next = now + CONTINUE_MS;
    } else {
      record.state = "lifted";
    }
  }
  purge.push(...fresh);
  if (purge.length > 0) {
    await purgeSlugs(cell.env, purge);
  }
  // A read that resolved before the change can refill the edge cache for a
  // few seconds, so freshly changed hosts are purged once more.
  if (fresh.length > 0) {
    record.repurge = fresh;
    next = Math.min(next ?? Infinity, now + REPURGE_MS);
  }
  return next;
}
