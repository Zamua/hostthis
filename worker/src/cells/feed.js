import { cellRoute } from "./image.js";
import { dispatch, mutation, read, requireShape } from "./ops.js";

// The FEED cell: one cell, named FEED_CELL, listing which apps changed and
// when, for an operator's scan to read what moved since its last run instead
// of every cell (docs/SPEC.md "Change feed and room index"). Best-effort by
// design: a writer never fails because an append did.
export const FEED_CELL = "changes";
export const FEED_RETENTION_MS = 8 * 24 * 3600 * 1000;
const ENTRY = "c:";
const PAGE_MAX = 1000;
const KINDS = new Set(["paste", "rooms"]);
// Retention is enforced on append, a page of the oldest entries at a time.
const PRUNE_BATCH = 128;

// Keys sort by time: a zero-padded millisecond stamp, then the slug.
export function feedKey(at, slug) {
  return `${ENTRY}${String(at).padStart(15, "0")}:${slug}`;
}

const FEED_OPS = {
  append: mutation((self, body) => self.append(body)),
  since: read((self, _body, url) => self.since(url)),
};

export class Feed {
  constructor(state) {
    this.state = state;
    this.now = () => Date.now();
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, FEED_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  async append(body) {
    if (!requireShape(body, { slug: "string", kind: "string" }) || !KINDS.has(body.kind)) {
      return Response.json({ error: "invalid-entry" }, { status: 400 });
    }
    const at = this.now();
    await this.state.storage.put(feedKey(at, body.slug), { slug: body.slug, kind: body.kind, at });
    const old = await this.state.storage.list({ prefix: ENTRY, end: feedKey(at - FEED_RETENTION_MS, ""), limit: PRUNE_BATCH });
    if (old.size > 0) {
      await this.state.storage.delete([...old.keys()]);
    }
    return Response.json({ at });
  }

  // Entries at or after `at` (ms), oldest first; `start` resumes from a
  // previous page's `next`.
  async since(url) {
    const at = Number(url.searchParams.get("at") ?? 0);
    const limit = Math.min(Number(url.searchParams.get("limit") ?? PAGE_MAX), PAGE_MAX);
    if (!Number.isSafeInteger(at) || at < 0 || !Number.isSafeInteger(limit) || limit < 1) {
      return Response.json({ error: "invalid-page" }, { status: 400 });
    }
    const start = url.searchParams.get("start") ?? feedKey(at, "");
    const rows = await this.state.storage.list({ prefix: ENTRY, start, limit: limit + 1 });
    const entries = [];
    let next = null;
    for (const [key, entry] of rows) {
      if (entries.length === limit) {
        next = key;
        break;
      }
      entries.push(entry);
    }
    return Response.json({ entries, next });
  }
}

// Appends one entry to the feed; false when the deployment has no feed or the
// append failed, which callers ignore.
export async function noteChange(env, slug, kind) {
  if (!env?.FEED || !slug) {
    return false;
  }
  try {
    const res = await env.FEED.get(env.FEED.idFromName(FEED_CELL)).fetch(new Request("https://cell/feed/append", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ slug, kind }),
    }));
    await res.body?.cancel();
    return res.ok;
  } catch (error) {
    console.warn(`warn: feed append ${kind} ${slug}: ${error?.message ?? error}`);
    return false;
  }
}
