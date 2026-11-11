// The quarantine and watcher routes (docs/SPEC.md "Quarantine"). The actor
// comes from the credential and the time from this clock; the body supplies
// only the target and reason.
import { normalizeHost } from "./public/host.js";

const REPURGE_MS = 15_000;

export const ROUTES = {
  operator: {
    "/abuse/quarantine": { op: "quarantine", fields: ["slug", "reason"], purge: true },
    "/abuse/release": { op: "release", fields: ["slug"], purge: true },
    "/abuse/list": { op: "list" },
    "/abuse/audit": { op: "audit" },
    "/abuse/room-delete": { op: "roomdelete", fields: ["app", "room"] },
    "/abuse/ban-owner": { op: "banowner", fields: ["identity", "slug", "reason", "retentionDays"] },
    "/abuse/unban-owner": { op: "unbanowner", fields: ["banId"] },
    "/abuse/bans": { op: "bans" },
  },
  watcher: {
    "/watcher/quarantine": { op: "quarantine", fields: ["slug", "reason"], purge: true },
    "/watcher/ban-key": { op: "bankey", fields: ["identity", "slug", "reason"] },
    "/watcher/ban-owner": { op: "banowner", fields: ["identity", "slug", "reason"] },
    "/watcher/refused-bans": { op: "refused", refused: true },
    "/watcher/key-subnets": { op: "keysubnets", keySubnets: true },
  },
};

// Purges a slug's host from the zone's cache; false when the deployment names
// no purge credential.
function canPurge(env) {
  return Boolean(env.HOSTTHIS_CF_PURGE_TOKEN && env.HOSTTHIS_CF_ZONE_ID && normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? ""));
}

export async function purgeSlug(env, slug, fetcher = fetch) {
  return purgeSlugs(env, [slug], fetcher);
}

// The purge API takes at most 30 hosts per call.
const PURGE_HOSTS_MAX = 30;

export async function purgeSlugs(env, slugs, fetcher = fetch) {
  if (!canPurge(env)) {
    return false;
  }
  const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN);
  let ok = true;
  for (let i = 0; i < slugs.length; i += PURGE_HOSTS_MAX) {
    const batch = slugs.slice(i, i + PURGE_HOSTS_MAX);
    try {
      const res = await fetcher(`https://api.cloudflare.com/client/v4/zones/${env.HOSTTHIS_CF_ZONE_ID}/purge_cache`, {
        method: "POST",
        headers: { "Authorization": `Bearer ${env.HOSTTHIS_CF_PURGE_TOKEN}`, "Content-Type": "application/json" },
        body: JSON.stringify({ hosts: batch.map((slug) => `${slug}.${apex}`) }),
      });
      if (!res.ok) {
        console.warn(`warn: purge ${batch.join(",")}: status ${res.status}: ${(await res.text()).slice(0, 200)}`);
        ok = false;
      }
    } catch (error) {
      console.warn(`warn: purge ${batch.join(",")}: ${error?.message ?? error}`);
      ok = false;
    }
  }
  return ok;
}

const DAY_MS = 86_400_000;
const REFUSED_LIMIT_MAX = 200;

// GET /watcher/refused-bans?since=<ISO>&limit=<n>: the bans the hourly cap
// refused, as ISO times. Read-only.
async function refusedBans(request, cell, url, nowMs) {
  if (request.method !== "GET") {
    return new Response("method not allowed\n", { status: 405 });
  }
  const sinceParam = url.searchParams.get("since");
  const limitParam = url.searchParams.get("limit");
  const since = sinceParam === null ? nowMs - DAY_MS : Date.parse(sinceParam);
  const limit = limitParam === null ? 50 : Number(limitParam);
  if (!Number.isFinite(since) || !Number.isInteger(limit) || limit < 1 || limit > REFUSED_LIMIT_MAX) {
    return Response.json({ error: "invalid-request" }, { status: 400 });
  }
  const res = await cell.fetch(new Request(`https://cell/abuse/refused?since=${since}&limit=${limit}`));
  if (!res.ok) {
    return res;
  }
  const { entries } = await res.json();
  return Response.json({
    now: new Date(nowMs).toISOString(),
    entries: entries.map((e) => ({ ...e, at: new Date(e.at).toISOString() })),
  });
}

// GET /watcher/key-subnets?window_min=&min_keys=&limit=: subnets where keys
// were first seen close together. Read-only.
async function keySubnets(request, cell, url, nowMs) {
  if (request.method !== "GET") {
    return new Response("method not allowed\n", { status: 405 });
  }
  const num = (name, fallback) => (url.searchParams.get(name) === null ? fallback : Number(url.searchParams.get(name)));
  const windowMin = num("window_min", 60);
  const minKeys = num("min_keys", 3);
  const limit = num("limit", 50);
  const within = (n, max) => Number.isInteger(n) && n >= 1 && n <= max;
  if (!within(windowMin, 1440) || !within(minKeys, 1000) || !within(limit, REFUSED_LIMIT_MAX)) {
    return Response.json({ error: "invalid-request" }, { status: 400 });
  }
  const res = await cell.fetch(new Request(
    `https://cell/abuse/keysubnets?now=${nowMs}&window_min=${windowMin}&min_keys=${minKeys}&limit=${limit}`));
  if (!res.ok) {
    return res;
  }
  const iso = (ms) => new Date(ms).toISOString();
  const { groups } = await res.json();
  return Response.json({
    now: iso(nowMs),
    groups: groups.map((g) => ({ ...g, first: iso(g.first), last: iso(g.last), keys: g.keys.map((k) => ({ ...k, at: iso(k.at) })) })),
  });
}

export async function abuseRoute(request, env, ctx, url, actor, { now = Date.now, fetcher = fetch, sleep } = {}) {
  const route = ROUTES[actor][url.pathname];
  if (!route) {
    return new Response("not found\n", { status: 404 });
  }
  const cell = env.ABUSE.get(env.ABUSE.idFromName("abuse"));
  if (route.keySubnets) {
    return keySubnets(request, cell, url, now());
  }
  if (route.refused) {
    return refusedBans(request, cell, url, now());
  }
  if (!route.fields) {
    if (request.method !== "GET") {
      return new Response("method not allowed\n", { status: 405 });
    }
    return cell.fetch(new Request(`https://cell/abuse/${route.op}`));
  }
  if (request.method !== "POST") {
    return new Response("method not allowed\n", { status: 405 });
  }
  let input;
  try {
    input = await request.json();
  } catch {
    return Response.json({ error: "invalid-request" }, { status: 400 });
  }
  const body = { actor, now: now() };
  for (const field of route.fields) {
    body[field] = input?.[field] ?? (field === "reason" ? "" : undefined);
  }
  const res = await cell.fetch(new Request(`https://cell/abuse/${route.op}`, { method: "POST", body: JSON.stringify(body) }));
  if (!route.purge || !res.ok) {
    return res;
  }
  const answer = await res.json();
  const purged = await purgeSlug(env, body.slug, fetcher);
  if (canPurge(env)) {
    // A read that resolved before the change can still fill the edge cache
    // for up to 10 seconds, and a rate-limited first purge needs a retry: the
    // second purge covers both.
    const wait = sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));
    ctx?.waitUntil(wait(REPURGE_MS).then(() => purgeSlug(env, body.slug, fetcher)));
  }
  return Response.json({ ...answer, purged });
}
