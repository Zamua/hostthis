import { bucketGet } from "../bucket.js";
import { normalizeHost } from "../public/host.js";
import { cellRoute } from "./image.js";
import { dispatch, mutation } from "./ops.js";

// The PURGE cell: the one queue every edge-cache purge of a deployment goes
// through (docs/SPEC.md "Purge queue"). The purge API admits a few calls a
// minute for the whole account, so a purge is queued durably, sent in calls
// of many hosts paced under a budget, and kept until a call carrying it
// succeeds. A purge is never dropped.

export const PURGE_CELL = "purge";
// Hosts in one call, the API's limit on every plan.
export const PURGE_HOSTS_MAX = 100;
// A read that resolved before a change can still fill the edge cache for up
// to 10 seconds (EDGE_FILL_WINDOW_MS), so every host is purged a second time
// this long after its first purge went out.
export const SECOND_PURGE_MS = 15_000;
// How long a second purge may wait past its time for others to share its
// call. Second purges come in the same bursts the first ones did.
export const SECOND_SLACK_MS = 3_000;
// The least time between two calls, so a burst of changes shares calls.
export const CALL_GAP_MS = 1_000;
// How long to wait after a refusal that is not the rate limit and names no
// wait (a server error, a rejected credential, no answer).
export const FAILURE_WAIT_MS = 15_000;
export const CALL_TIMEOUT_MS = 10_000;
// Slugs the queue holds at once; an enqueue past it is refused, and the
// caller keeps it.
export const QUEUE_MAX = 5_000;
// The status snapshot is rebuilt from the cell once it is this old.
export const STATUS_MAX_AGE_MS = 5 * 60 * 1000;

const SLUG = /^[a-z0-9]{1,63}$/;
const FIRST = "p:";
const SECOND = "r:";
const STATE = "state";
const WRITE_BATCH = 128;

// The budget, as two token buckets: `ordinary` pays for any call, `urgent`
// only for a call carrying an urgent purge, so ordinary purges can never
// spend what a takedown needs. HOSTTHIS_PURGE_BUDGET is
// "<per minute>/<burst>,<per minute>/<burst>", ordinary then urgent. The
// default leaves room under the smallest account limit (5 a minute, burst 25).
export const DEFAULT_BUDGET = Object.freeze({
  ordinary: Object.freeze({ perMinute: 3, burst: 15 }),
  urgent: Object.freeze({ perMinute: 1, burst: 5 }),
});

function bucketSpec(text) {
  const match = /^(\d+(?:\.\d+)?)\/(\d+)$/.exec(text.trim());
  if (!match || Number(match[1]) <= 0 || Number(match[2]) < 1) {
    return null;
  }
  return { perMinute: Number(match[1]), burst: Number(match[2]) };
}

// The deployment's purge settings, or null when it names no credential, zone
// or apex: such a deployment queues nothing.
export function purgeConfig(env) {
  const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
  if (!env.HOSTTHIS_CF_PURGE_TOKEN || !env.HOSTTHIS_CF_ZONE_ID || !apex) {
    return null;
  }
  let budget = DEFAULT_BUDGET;
  const raw = env.HOSTTHIS_PURGE_BUDGET;
  if (typeof raw === "string" && raw.trim() !== "") {
    const [ordinary, urgent] = raw.split(",").map(bucketSpec);
    if (ordinary && urgent && raw.split(",").length === 2) {
      budget = { ordinary, urgent };
    } else {
      console.warn(`warn: HOSTTHIS_PURGE_BUDGET ${JSON.stringify(raw)} is not "<n>/<burst>,<n>/<burst>"; using the default`);
    }
  }
  return { apex, zone: env.HOSTTHIS_CF_ZONE_ID, token: env.HOSTTHIS_CF_PURGE_TOKEN, budget };
}

// Tokens a bucket holds at now, given what it held at `at`.
function level(spec, held, now) {
  return Math.min(spec.burst, held.tokens + Math.max(0, now - held.at) * spec.perMinute / 60_000);
}

// When a bucket next holds a whole token.
function readyAt(spec, held, now) {
  const have = level(spec, held, now);
  return have >= 1 ? now : now + Math.ceil((1 - have) * 60_000 / spec.perMinute);
}

function statusKey(env) {
  return `internal/purge/${env.PURGE.idFromName(PURGE_CELL)}.json`;
}

function purgeCell(env) {
  return env.PURGE.get(env.PURGE.idFromName(PURGE_CELL));
}

// Queues slugs for purging. Answers { configured, queued, sent }: sent is
// whether every slug's first purge has already gone out. A deployment with no
// purge credential queues nothing. Throws when the cell refuses or cannot be
// reached, so the caller keeps the slugs.
export async function enqueuePurge(env, slugs, { urgent = false } = {}) {
  if (!purgeConfig(env) || !env.PURGE) {
    return { configured: false, queued: 0, sent: false };
  }
  const res = await purgeCell(env).fetch(new Request("https://cell/purge/enqueue", {
    method: "POST",
    body: JSON.stringify({ slugs, urgent }),
  }));
  if (!res.ok) {
    throw new Error(`purge enqueue: status ${res.status}: ${(await res.text()).slice(0, 200)}`);
  }
  return res.json();
}

// The queue's status for a monitor, from the snapshot the cell writes at
// every change, else from the cell. A monitor's poll does not call the cell.
export async function purgeStatus(env, now = Date.now()) {
  if (!purgeConfig(env) || !env.PURGE) {
    return Response.json({ configured: false, pending: 0, second: 0, oldestAt: null });
  }
  try {
    const object = await bucketGet(env.PAYLOADS, statusKey(env));
    const snap = object && JSON.parse(await new Response(object.body).text());
    if (snap && Number.isSafeInteger(snap.writtenAt) && Number.isSafeInteger(snap.pending)
      && now - snap.writtenAt >= 0 && now - snap.writtenAt < STATUS_MAX_AGE_MS) {
      return Response.json(snap);
    }
  } catch (error) {
    console.warn(`warn: purge status read: ${error.message}`);
  }
  return purgeCell(env).fetch(new Request("https://cell/purge/status", { method: "POST" }));
}

// The internal router's /purge/ paths: POST /purge/enqueue {slugs, urgent}
// and GET /purge/status.
export async function purgeRoute(request, env, url) {
  if (url.pathname === "/purge/status" && request.method === "GET") {
    return purgeStatus(env);
  }
  if (url.pathname === "/purge/enqueue" && request.method === "POST") {
    if (!purgeConfig(env) || !env.PURGE) {
      return Response.json({ configured: false, queued: 0, sent: false });
    }
    return purgeCell(env).fetch(new Request("https://cell/purge/enqueue", { method: "POST", body: await request.text() }));
  }
  return new Response("not found\n", { status: 404 });
}

export const PURGE_OPS = {
  enqueue: mutation((self, body) => self.enqueue(body)),
  // The status, and its snapshot rewritten.
  status: mutation((self) => self.status(), { body: false }),
};

export class Purge {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.now = () => Date.now();
    this.fetcher = (...args) => fetch(...args);
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, PURGE_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  // The queue is bounded (QUEUE_MAX), so one list reads all of a kind.
  async rows(prefix) {
    const page = await this.state.storage.list({ prefix });
    return [...page].map(([key, row]) => ({ slug: key.slice(prefix.length), ...row }));
  }

  async held(cfg, now) {
    const stored = await this.state.storage.get(STATE);
    const full = (spec) => ({ tokens: spec.burst, at: now });
    return {
      ordinary: stored?.ordinary ?? full(cfg.budget.ordinary),
      urgent: stored?.urgent ?? full(cfg.budget.urgent),
      lastCall: stored?.lastCall ?? 0,
      holdUntil: stored?.holdUntil ?? 0,
    };
  }

  async enqueue(body) {
    const cfg = purgeConfig(this.env);
    if (!cfg) {
      return Response.json({ configured: false, queued: 0, sent: false });
    }
    const slugs = body?.slugs;
    if (!Array.isArray(slugs) || slugs.length > PURGE_HOSTS_MAX || !slugs.every((s) => typeof s === "string" && SLUG.test(s))) {
      return Response.json({ error: "invalid-request" }, { status: 400 });
    }
    const urgent = body.urgent === true;
    const now = this.now();
    const wanted = [...new Set(slugs)];
    const present = await this.state.storage.get(wanted.map((slug) => FIRST + slug));
    const fresh = wanted.filter((slug) => !present.has(FIRST + slug));
    if (fresh.length > 0 && (await this.rows(FIRST)).length + fresh.length > QUEUE_MAX) {
      return Response.json({ error: "purge-queue-full" }, { status: 507 });
    }
    const puts = {};
    for (const slug of wanted) {
      const row = present.get(FIRST + slug);
      // A slug already waiting keeps its time, and becomes urgent if asked.
      puts[FIRST + slug] = { at: row?.at ?? now, urgent: urgent || row?.urgent === true };
    }
    if (wanted.length > 0) {
      await this.state.storage.put(puts);
    }
    await this.pump(cfg);
    const left = wanted.length > 0 ? await this.state.storage.get(wanted.map((slug) => FIRST + slug)) : new Map();
    return Response.json({ configured: true, queued: wanted.length, sent: left.size === 0 });
  }

  // Sends what may be sent now, then sets the alarm for what is left.
  async pump(cfg) {
    await this.sendOnce(cfg);
    await this.arm(cfg);
    await this.writeStatus();
  }

  // What a call made at now would carry, urgent first, then oldest first, and
  // when it may be made. Null with nothing waiting.
  async plan(cfg, now) {
    const first = await this.rows(FIRST);
    const second = await this.rows(SECOND);
    if (first.length === 0 && second.length === 0) {
      return null;
    }
    const held = await this.held(cfg, now);
    const dueSecond = second.filter((row) => row.due <= now);
    const order = (a, b) => (Number(b.urgent) - Number(a.urgent)) || (a.at - b.at) || (a.slug < b.slug ? -1 : 1);
    const batch = [
      ...first.map((row) => ({ ...row, kind: FIRST })),
      ...dueSecond.map((row) => ({ ...row, kind: SECOND })),
    ].sort(order).slice(0, PURGE_HOSTS_MAX);
    const urgent = batch.some((row) => row.urgent);
    // A call carrying a first purge goes as soon as the pace allows; second
    // purges alone wait for their time plus the slack.
    let wanted;
    if (first.length > 0) {
      wanted = now;
    } else {
      wanted = Math.min(...second.map((row) => row.due)) + SECOND_SLACK_MS;
    }
    const ordinaryReady = readyAt(cfg.budget.ordinary, held.ordinary, now);
    const urgentReady = urgent ? readyAt(cfg.budget.urgent, held.urgent, now) : Infinity;
    const at = Math.max(wanted, held.lastCall + CALL_GAP_MS, held.holdUntil, Math.min(ordinaryReady, urgentReady));
    return { batch, at, held, pays: ordinaryReady <= urgentReady ? "ordinary" : "urgent" };
  }

  // Makes one call when one is due. Every caller holds the cell's
  // concurrency block (a mutating op, or the alarm), so one call is in flight
  // at a time; CALL_TIMEOUT_MS keeps it well inside the block's limit.
  async sendOnce(cfg) {
    const now = this.now();
    const plan = await this.plan(cfg, now);
    if (!plan || plan.at > now || plan.batch.length === 0) {
      return;
    }
    const { batch, held, pays } = plan;
    const spec = cfg.budget[pays];
    held[pays] = { tokens: level(spec, held[pays], now) - 1, at: now };
    held.lastCall = now;
    // The token is spent before the call: a call that dies still counted.
    await this.state.storage.put(STATE, held);
    const outcome = await this.call(cfg, batch.map((row) => row.slug));
    if (!outcome.ok) {
      const after = this.now();
      if (outcome.limited) {
        // The account's budget is spent, by this queue or another purger.
        held.ordinary = { tokens: 0, at: after };
        held.urgent = { tokens: 0, at: after };
      }
      held.holdUntil = after + (outcome.retryAfterMs ?? (outcome.limited ? 0 : FAILURE_WAIT_MS));
      await this.state.storage.put(STATE, held);
      return;
    }
    const sent = this.now();
    for (let i = 0; i < batch.length; i += WRITE_BATCH) {
      const part = batch.slice(i, i + WRITE_BATCH);
      const seconds = {};
      for (const row of part.filter((r) => r.kind === FIRST)) {
        seconds[SECOND + row.slug] = { at: row.at, urgent: row.urgent, due: sent + SECOND_PURGE_MS };
      }
      await this.state.storage.transaction(async (tx) => {
        if (Object.keys(seconds).length > 0) {
          await tx.put(seconds);
        }
        // A second purge that just went out is done; one whose first purge
        // went out with it was replaced above.
        const done = part.filter((r) => r.kind === SECOND && !(SECOND + r.slug in seconds)).map((r) => SECOND + r.slug);
        await tx.delete([...part.filter((r) => r.kind === FIRST).map((r) => FIRST + r.slug), ...done]);
      });
    }
  }

  // One purge call. ok, or limited (the rate limit) and retryAfterMs when the
  // answer names a wait.
  async call(cfg, slugs) {
    try {
      const res = await this.fetcher(`https://api.cloudflare.com/client/v4/zones/${cfg.zone}/purge_cache`, {
        method: "POST",
        headers: { "Authorization": `Bearer ${cfg.token}`, "Content-Type": "application/json" },
        body: JSON.stringify({ hosts: slugs.map((slug) => `${slug}.${cfg.apex}`) }),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (res.ok) {
        await res.body?.cancel();
        return { ok: true };
      }
      const wait = Number(res.headers.get("Retry-After"));
      console.warn(`warn: purge of ${slugs.length} host(s): status ${res.status}: ${(await res.text()).slice(0, 200)}`);
      return {
        ok: false,
        limited: res.status === 429,
        ...(Number.isFinite(wait) && wait > 0 && { retryAfterMs: Math.min(wait, 300) * 1000 }),
      };
    } catch (error) {
      console.warn(`warn: purge of ${slugs.length} host(s): ${error?.message ?? error}`);
      return { ok: false, limited: false };
    }
  }

  async arm(cfg) {
    const now = this.now();
    const plan = await this.plan(cfg, now);
    if (!plan) {
      await this.state.storage.deleteAlarm();
      return;
    }
    await this.state.storage.setAlarm(Math.max(plan.at, now + 1));
  }

  async alarm() {
    const cfg = purgeConfig(this.env);
    if (!cfg) {
      return;
    }
    await this.state.blockConcurrencyWhile(() => this.pump(cfg));
  }

  // pending: slugs whose first purge has not gone out. second: second purges
  // not yet out. oldestAt: when the longest-waiting purge became due (a first
  // purge when queued, a second at its time), or null.
  async summary() {
    const now = this.now();
    const first = await this.rows(FIRST);
    const second = await this.rows(SECOND);
    const due = [...first.map((row) => row.at), ...second.map((row) => row.due)];
    return {
      configured: true,
      writtenAt: now,
      pending: first.length,
      urgent: first.filter((row) => row.urgent).length,
      second: second.length,
      oldestAt: due.length > 0 ? Math.min(...due) : null,
    };
  }

  // Writes the status for monitors to read. A failed write removes the
  // snapshot, so a poll reaches the cell; failing that too is only logged,
  // and the snapshot's age sends polls here within STATUS_MAX_AGE_MS.
  async writeStatus() {
    const summary = await this.summary();
    const key = statusKey(this.env);
    try {
      await this.env.PAYLOADS.put(key, JSON.stringify(summary));
    } catch (error) {
      console.warn(`warn: purge status write: ${error.message}`);
      await this.env.PAYLOADS.delete(key).catch((e) => console.warn(`warn: purge status remove: ${e.message}`));
    }
    return summary;
  }

  async status() {
    return Response.json(await this.writeStatus());
  }
}
