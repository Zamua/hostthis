import { bucketGet } from "../bucket.js";
import { cellRoute } from "./image.js";
import { dispatch, mutation, read } from "./ops.js";

// The DENYLIST cell: the operator's banned keys and networks
// (docs/SPEC.md "Operator denylist"). One cell holds the whole list; hostthisd
// replicas poll `list` and check SSH sessions against their copy.
//
// The poll is answered from a snapshot of the list in the payload bucket,
// which the cell rewrites inside every add and remove, so a poll reaches the
// cell only when the snapshot is missing, unreadable or old.

export const DENYLIST_CELL = "denylist";
// The snapshot's bucket key, named by the cell's id so that two lists sharing
// a bucket never read each other's.
export function snapshotKey(env) {
  return `internal/denylist/${env.DENYLIST.idFromName(DENYLIST_CELL)}.json`;
}
// A snapshot older than this is rebuilt from the cell. It bounds how long a
// snapshot the cell failed to rewrite can stand in for the list.
export const SNAPSHOT_MAX_AGE_MS = 5 * 60 * 1000;

export const DENYLIST_CAP = 1000;
const REASON_MAX = 200;
const ENTRY = "e:";
const KEY = /^SHA256:[0-9a-f]{64}$/;

function expired(row, now) {
  return typeof row.expiresAt === "number" && row.expiresAt <= now;
}

function invalid() {
  return Response.json({ error: "invalid-entry" }, { status: 400 });
}

function parseV4(text) {
  const parts = text.split(".");
  if (parts.length !== 4 || !parts.every((p) => /^(0|[1-9][0-9]{0,2})$/.test(p) && Number(p) < 256)) {
    return null;
  }
  return parts.map(Number);
}

// Eight 16-bit groups, or null.
function parseV6(text) {
  const halves = text.toLowerCase().split("::");
  if (halves.length > 2) {
    return null;
  }
  const groups = halves.map((h) => (h === "" ? [] : h.split(":")));
  if (!groups.flat().every((g) => /^[0-9a-f]{1,4}$/.test(g))) {
    return null;
  }
  const [head, tail = []] = groups;
  const missing = 8 - head.length - tail.length;
  if (halves.length === 1 ? missing !== 0 : missing < 1) {
    return null;
  }
  return [...head, ...Array(halves.length === 1 ? 0 : missing).fill("0"), ...tail].map((g) => parseInt(g, 16));
}

function mask(words, bitsPerWord, prefix) {
  return words.map((w, i) => {
    const keep = Math.min(bitsPerWord, Math.max(0, prefix - i * bitsPerWord));
    return keep === 0 ? 0 : w & ~((1 << (bitsPerWord - keep)) - 1) & ((1 << bitsPerWord) - 1);
  });
}

// RFC 5952: lowercase, the longest run of two or more zero groups as "::".
function formatV6(groups) {
  let best = [-1, 0];
  for (let i = 0; i < 8;) {
    let j = i;
    while (j < 8 && groups[j] === 0) j++;
    if (j - i > best[1] && j - i >= 2) best = [i, j - i];
    i = j === i ? i + 1 : j;
  }
  const hex = groups.map((g) => g.toString(16));
  if (best[0] < 0) {
    return hex.join(":");
  }
  return `${hex.slice(0, best[0]).join(":")}::${hex.slice(best[0] + best[1]).join(":")}`;
}

// The stored form of an entry, or null when it is neither a key fingerprint
// nor a CIDR prefix inside the allowed widths.
export function canonicalEntry(text) {
  if (typeof text !== "string") {
    return null;
  }
  if (KEY.test(text)) {
    return text;
  }
  const m = /^([^/\s]+)\/(0|[1-9][0-9]{0,2})$/.exec(text);
  if (!m) {
    return null;
  }
  const prefix = Number(m[2]);
  const v4 = m[1].includes(":") ? null : parseV4(m[1]);
  if (v4) {
    return prefix >= 8 && prefix <= 32 ? `${mask(v4, 8, prefix).join(".")}/${prefix}` : null;
  }
  const v6 = m[1].includes(":") ? parseV6(m[1]) : null;
  if (v6 && prefix >= 16 && prefix <= 128) {
    return `${formatV6(mask(v6, 16, prefix))}/${prefix}`;
  }
  return null;
}

export const DENYLIST_OPS = {
  list: read((self) => self.list()),
  // list, and the snapshot rewritten from it. Mutating, so it cannot write an
  // older list over the snapshot of an add or remove that ran beside it.
  snapshot: mutation((self) => self.snapshot(), { body: false }),
  add: mutation((self, body) => self.add(body)),
  remove: mutation((self, body) => self.remove(body)),
};

// The list a poll is answered with, from the snapshot when it is usable, else
// from the cell, which rewrites the snapshot. Anything wrong with the
// snapshot (a failed read, a missing or malformed object, one past its age)
// goes to the cell: a poll is never answered with a list the cell did not
// write.
export async function denylistPoll(env, now = Date.now()) {
  try {
    const object = await bucketGet(env.PAYLOADS, snapshotKey(env));
    const snap = object && JSON.parse(await new Response(object.body).text());
    if (snap && Array.isArray(snap.entries) && Number.isSafeInteger(snap.writtenAt)
      && now - snap.writtenAt >= 0 && now - snap.writtenAt < SNAPSHOT_MAX_AGE_MS) {
      return Response.json({ entries: snap.entries.filter((row) => !expired(row, now)) });
    }
  } catch (error) {
    console.warn(`warn: denylist snapshot read: ${error.message}`);
  }
  return env.DENYLIST.get(env.DENYLIST.idFromName(DENYLIST_CELL))
    .fetch(new Request("https://cell/denylist/snapshot", { method: "POST" }));
}

export class Denylist {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.now = () => Date.now();
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, DENYLIST_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  async entries(now) {
    const rows = await this.state.storage.list({ prefix: ENTRY });
    const entries = [...rows].filter(([, row]) => !expired(row, now))
      .map(([key, row]) => ({ entry: key.slice(ENTRY.length), ...row }));
    entries.sort((a, b) => a.addedAt - b.addedAt);
    return entries;
  }

  async list() {
    return Response.json({ entries: await this.entries(this.now()) });
  }

  // Writes the list to the bucket for polls to read. A failed write removes
  // the snapshot, so polls reach the cell; if that fails too the error stands
  // and the caller's operation reports it.
  async writeSnapshot() {
    const now = this.now();
    const entries = await this.entries(now);
    const key = snapshotKey(this.env);
    try {
      await this.env.PAYLOADS.put(key, JSON.stringify({ writtenAt: now, entries }));
    } catch (error) {
      console.warn(`warn: denylist snapshot write: ${error.message}`);
      await this.env.PAYLOADS.delete(key);
    }
    return entries;
  }

  // A poll that reached the cell gets the list even when the snapshot cannot
  // be written; the next poll comes here again.
  async snapshot() {
    try {
      return Response.json({ entries: await this.writeSnapshot() });
    } catch (error) {
      console.warn(`warn: denylist snapshot remove: ${error.message}`);
      return this.list();
    }
  }

  async add(body) {
    const entry = canonicalEntry(body?.entry);
    const reason = body?.reason;
    const expiresAt = body?.expiresAt ?? null;
    const now = this.now();
    if (!entry || typeof reason !== "string" || new TextEncoder().encode(reason).length > REASON_MAX) {
      return invalid();
    }
    if (expiresAt !== null && !(Number.isSafeInteger(expiresAt) && expiresAt > now)) {
      return invalid();
    }
    const key = ENTRY + entry;
    const prior = await this.state.storage.get(key);
    if (!prior && await this.liveCount(now) >= DENYLIST_CAP) {
      return Response.json({ error: "denylist-full" }, { status: 507 });
    }
    const row = { reason, addedAt: prior?.addedAt ?? now };
    if (expiresAt !== null) {
      row.expiresAt = expiresAt;
    }
    await this.state.storage.put(key, row);
    await this.writeSnapshot();
    return Response.json({ entry, ...row });
  }

  // Counts unexpired entries, deleting expired ones on the way.
  async liveCount(now) {
    const rows = await this.state.storage.list({ prefix: ENTRY });
    const gone = [...rows].filter(([, row]) => expired(row, now)).map(([key]) => key);
    for (let i = 0; i < gone.length; i += 128) {
      await this.state.storage.delete(gone.slice(i, i + 128));
    }
    return rows.size - gone.length;
  }

  async remove(body) {
    const entry = canonicalEntry(body?.entry);
    if (!entry) {
      return invalid();
    }
    const key = ENTRY + entry;
    const removed = (await this.state.storage.get(key)) !== undefined;
    if (removed) {
      await this.state.storage.delete(key);
    }
    // Also when nothing was removed: a retry after a failed snapshot write
    // must still leave a current snapshot.
    await this.writeSnapshot();
    return Response.json({ entry, removed });
  }
}
