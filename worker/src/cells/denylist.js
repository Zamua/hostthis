import { cellRoute } from "./image.js";
import { dispatch, mutation, read } from "./ops.js";

// The DENYLIST cell: the operator's banned keys and networks
// (docs/SPEC.md "Operator denylist"). One cell holds the whole list; hostthisd
// replicas poll `list` and check SSH sessions against their copy.

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

const DENYLIST_OPS = {
  list: read((self) => self.list()),
  add: mutation((self, body) => self.add(body)),
  remove: mutation((self, body) => self.remove(body)),
};

export class Denylist {
  constructor(state) {
    this.state = state;
    this.now = () => Date.now();
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, DENYLIST_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  async list() {
    const now = this.now();
    const rows = await this.state.storage.list({ prefix: ENTRY });
    const entries = [...rows].filter(([, row]) => !expired(row, now))
      .map(([key, row]) => ({ entry: key.slice(ENTRY.length), ...row }));
    entries.sort((a, b) => a.addedAt - b.addedAt);
    return Response.json({ entries });
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
    return Response.json({ entry, removed });
  }
}
