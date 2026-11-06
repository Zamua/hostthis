import { cellRoute } from "./image.js";
import { dispatch, mutation, requireShape } from "./ops.js";

// The TURN cell: one cell, named TURN_CELL, that issues Cloudflare Realtime
// TURN credentials to room participants under a monthly egress budget
// (docs/SPEC.md "TURN relay credentials"). Every way TURN is unavailable
// answers STUN only, so issuing never fails a request.

export const TURN_CELL = "turn";
export const STUN_SERVER = Object.freeze({ urls: ["stun:stun.cloudflare.com:3478"] });
export const TURN_TTL_S = 3600;
export const READ_EVERY_MS = 30 * 1000;
export const FRESH_FOR_MS = 5 * 60 * 1000;
export const TURN_CAPS = Object.freeze({ room: 64, app: 512, total: 4096 });
export const DEFAULT_BUDGET_GB = 500;
export const CALL_TIMEOUT_MS = 10 * 1000;

const HOUR_MS = 3600 * 1000;
const RTC_API = "https://rtc.live.cloudflare.com/v1/turn/keys";
const GRAPHQL_API = "https://api.cloudflare.com/client/v4/graphql";
const READING = "reading";
const WINDOW = "window";
// i:<expiresAt, 15 digits>:<username>, so a prefix list walks oldest first.
const ISSUED = "i:";
const REVOKE_BATCH = 32;
const DELETE_BATCH = 128;

// The operator's settings, or null when any of the four is missing.
export function turnConfig(env) {
  const keyId = env.HOSTTHIS_TURN_KEY_ID;
  const keyToken = env.HOSTTHIS_TURN_KEY_TOKEN;
  const accountId = env.HOSTTHIS_TURN_ACCOUNT_ID;
  const analyticsToken = env.HOSTTHIS_TURN_ANALYTICS_TOKEN;
  if (![keyId, keyToken, accountId, analyticsToken].every((v) => typeof v === "string" && v !== "")) {
    return null;
  }
  // A string or a number (wrangler vars may be either); unset, blank or not a
  // non-negative number is the default. Number("") is 0, so a blank setting
  // must not read as a zero budget.
  const raw = env.HOSTTHIS_TURN_BUDGET_GB;
  const gb = typeof raw === "number" || (typeof raw === "string" && raw.trim() !== "") ? Number(raw) : NaN;
  const budgetGB = Number.isFinite(gb) && gb >= 0 ? gb : DEFAULT_BUDGET_GB;
  return { keyId, keyToken, accountId, analyticsToken, budgetBytes: budgetGB * 1e9 };
}

// The UTC calendar month an instant falls in, as "YYYY-MM".
export function monthOf(now) {
  return new Date(now).toISOString().slice(0, 7);
}

// The GraphQL request for the key's month-to-date egress.
export function usageRequest(cfg, now) {
  const today = new Date(now).toISOString().slice(0, 10);
  return {
    query: `query TurnUsage($accountTag: string!, $keyId: string!, $from: Date!, $to: Date!) {
  viewer {
    accounts(filter: { accountTag: $accountTag }) {
      callsTurnUsageAdaptiveGroups(limit: 10000, filter: { keyId: $keyId, date_geq: $from, date_leq: $to }) {
        dimensions { keyId }
        sum { egressBytes }
      }
    }
  }
}`,
    variables: { accountTag: cfg.accountId, keyId: cfg.keyId, from: `${monthOf(now)}-01`, to: today },
  };
}

// The summed egress of a GraphQL answer, or null when it is not one.
export function egressOf(answer) {
  if (answer?.errors?.length) {
    return null;
  }
  const groups = answer?.data?.viewer?.accounts?.[0]?.callsTurnUsageAdaptiveGroups;
  if (!Array.isArray(groups)) {
    return null;
  }
  let total = 0;
  for (const group of groups) {
    const bytes = group?.sum?.egressBytes;
    if (!Number.isFinite(bytes) || bytes < 0) {
      return null;
    }
    total += bytes;
  }
  return total;
}

// The TURN entry of a generate-ice-servers answer, its port 53 URLs dropped,
// or null when it holds none.
export function turnEntry(answer) {
  for (const server of answer?.iceServers ?? []) {
    const { urls, username, credential } = server ?? {};
    if (typeof username !== "string" || username === "" || typeof credential !== "string" || !Array.isArray(urls)) {
      continue;
    }
    const kept = urls.filter((u) => typeof u === "string" && /^turns?:/.test(u) && !/:53(\?|$)/.test(u));
    if (kept.length > 0) {
      return { urls: kept, username, credential };
    }
  }
  return null;
}

export function issuedKey(expiresAt, username) {
  return `${ISSUED}${String(expiresAt).padStart(15, "0")}:${username}`;
}

function issuedOf(key) {
  const rest = key.slice(ISSUED.length);
  const colon = rest.indexOf(":");
  return { expiresAt: Number(rest.slice(0, colon)), username: rest.slice(colon + 1) };
}

const answer = (servers, ttl) => new Response(JSON.stringify(ttl ? { iceServers: servers, ttl } : { iceServers: servers }), {
  headers: { "Content-Type": "application/json" },
});
const stunOnly = () => answer([STUN_SERVER]);

const TURN_OPS = {
  issue: mutation((self, body) => self.issue(body)),
};

export class Turn {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.now = () => Date.now();
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, TURN_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  call(url, init) {
    return (this.env.TURN_FETCH ?? fetch)(url, { ...init, redirect: "manual", signal: AbortSignal.timeout(CALL_TIMEOUT_MS) });
  }

  // One credential for a participant of room `room` under app `app`, or STUN
  // only. The front end has already checked that both exist.
  async issue(body) {
    if (!requireShape(body, { app: "string", room: "string", now: "uint" })) {
      return Response.json({ error: "invalid-request" }, { status: 400 });
    }
    const cfg = turnConfig(this.env);
    if (!cfg) {
      return stunOnly();
    }
    const { now } = body;
    const reading = await this.currentReading(cfg, now);
    if (!reading) {
      return stunOnly();
    }
    if (reading.egressBytes >= cfg.budgetBytes) {
      await this.revokeAll(cfg, now);
      return stunOnly();
    }

    const hour = Math.floor(now / HOUR_MS);
    let window = await this.state.storage.get(WINDOW);
    if (window?.hour !== hour) {
      window = { hour, total: 0, apps: {}, rooms: {} };
    }
    const roomKey = `${body.app}|${body.room}`;
    if ((window.rooms[roomKey] ?? 0) >= TURN_CAPS.room || (window.apps[body.app] ?? 0) >= TURN_CAPS.app
      || window.total >= TURN_CAPS.total) {
      return stunOnly();
    }

    const entry = await this.mint(cfg);
    if (!entry) {
      return stunOnly();
    }
    window.total++;
    window.apps[body.app] = (window.apps[body.app] ?? 0) + 1;
    window.rooms[roomKey] = (window.rooms[roomKey] ?? 0) + 1;
    const expiresAt = now + TURN_TTL_S * 1000;
    await this.state.storage.put({ [WINDOW]: window, [issuedKey(expiresAt, entry.username)]: true });
    const alarm = await this.state.storage.getAlarm();
    if (alarm === null || alarm === undefined) {
      await this.state.storage.setAlarm(now + READ_EVERY_MS);
    }
    return answer([STUN_SERVER, entry], TURN_TTL_S);
  }

  // A fresh reading of this month's egress, reading again when it is due, or
  // null when there is none.
  async currentReading(cfg, now) {
    let reading = await this.state.storage.get(READING);
    if (!reading || reading.month !== monthOf(now) || now - reading.at >= READ_EVERY_MS) {
      reading = (await this.read(cfg, now)) ?? reading;
    }
    if (!reading || reading.month !== monthOf(now) || now - reading.at >= FRESH_FOR_MS) {
      return null;
    }
    return reading;
  }

  // Reads the key's month-to-date egress and stores it; null on any failure.
  async read(cfg, now) {
    try {
      const res = await this.call(GRAPHQL_API, {
        method: "POST",
        headers: { "Authorization": `Bearer ${cfg.analyticsToken}`, "Content-Type": "application/json" },
        body: JSON.stringify(usageRequest(cfg, now)),
      });
      const egressBytes = res.status === 200 ? egressOf(await res.json()) : null;
      if (res.status !== 200) {
        await res.body?.cancel();
      }
      if (egressBytes === null) {
        console.warn(`turn: usage read failed: status ${res.status}`);
        return null;
      }
      const reading = { month: monthOf(now), egressBytes, at: now };
      await this.state.storage.put(READING, reading);
      return reading;
    } catch (error) {
      console.warn(`turn: usage read failed: ${error.message}`);
      return null;
    }
  }

  async mint(cfg) {
    try {
      const res = await this.call(`${RTC_API}/${encodeURIComponent(cfg.keyId)}/credentials/generate-ice-servers`, {
        method: "POST",
        headers: { "Authorization": `Bearer ${cfg.keyToken}`, "Content-Type": "application/json" },
        body: JSON.stringify({ ttl: TURN_TTL_S }),
      });
      if (res.status !== 200 && res.status !== 201) {
        await res.body?.cancel();
        console.warn(`turn: credential request failed: status ${res.status}`);
        return null;
      }
      return turnEntry(await res.json());
    } catch (error) {
      console.warn(`turn: credential request failed: ${error.message}`);
      return null;
    }
  }

  async revoke(cfg, username) {
    try {
      const res = await this.call(
        `${RTC_API}/${encodeURIComponent(cfg.keyId)}/credentials/${encodeURIComponent(username)}/revoke`,
        { method: "POST", headers: { "Authorization": `Bearer ${cfg.keyToken}` } },
      );
      await res.body?.cancel();
      return res.status >= 200 && res.status < 300;
    } catch {
      return false;
    }
  }

  // Revokes every unexpired credential this cell issued. A credential whose
  // revocation failed stays listed for the next attempt; expired ones go.
  async revokeAll(cfg, now) {
    const keys = [...(await this.state.storage.list({ prefix: ISSUED })).keys()];
    const done = [];
    const live = [];
    for (const key of keys) {
      (issuedOf(key).expiresAt <= now ? done : live).push(key);
    }
    for (let i = 0; i < live.length; i += REVOKE_BATCH) {
      const batch = live.slice(i, i + REVOKE_BATCH);
      const results = await Promise.all(batch.map((key) => this.revoke(cfg, issuedOf(key).username)));
      batch.forEach((key, j) => results[j] && done.push(key));
    }
    await this.drop(done);
  }

  async drop(keys) {
    for (let i = 0; i < keys.length; i += DELETE_BATCH) {
      await this.state.storage.delete(keys.slice(i, i + DELETE_BATCH));
    }
  }

  // Every 30 seconds while a credential it issued is unexpired: read the
  // budget, revoke on overrun, and forget expired credentials.
  async alarm(info = {}) {
    const now = info.now ?? this.now();
    const cfg = turnConfig(this.env);
    const issued = [...(await this.state.storage.list({ prefix: ISSUED })).keys()];
    if (!cfg) {
      await this.drop(issued);
      return;
    }
    const reading = await this.currentReading(cfg, now);
    if (reading && reading.egressBytes >= cfg.budgetBytes) {
      await this.revokeAll(cfg, now);
    } else {
      await this.drop(issued.filter((key) => issuedOf(key).expiresAt <= now));
    }
    const left = await this.state.storage.list({ prefix: ISSUED, limit: 1 });
    if (left.size > 0) {
      await this.state.storage.setAlarm(now + READ_EVERY_MS);
    }
  }
}
