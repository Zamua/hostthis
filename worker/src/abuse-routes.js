// The quarantine and watcher routes (docs/SPEC.md "Quarantine"). The actor
// comes from the credential and the time from this clock; the body supplies
// only the target and reason.
import { normalizeHost } from "./public/host.js";

const REPURGE_MS = 15_000;

const ROUTES = {
  operator: {
    "/abuse/quarantine": { op: "quarantine", fields: ["slug", "reason"], purge: true },
    "/abuse/release": { op: "release", fields: ["slug"], purge: true },
    "/abuse/list": { op: "list" },
    "/abuse/audit": { op: "audit" },
  },
  watcher: {
    "/watcher/quarantine": { op: "quarantine", fields: ["slug", "reason"], purge: true },
    "/watcher/ban-key": { op: "bankey", fields: ["identity", "reason"] },
  },
};

// Purges a slug's host from the zone's cache; false when the deployment names
// no purge credential.
export async function purgeSlug(env, slug, fetcher = fetch) {
  const apex = normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "");
  if (!env.HOSTTHIS_CF_PURGE_TOKEN || !env.HOSTTHIS_CF_ZONE_ID || !apex) {
    return false;
  }
  try {
    const res = await fetcher(`https://api.cloudflare.com/client/v4/zones/${env.HOSTTHIS_CF_ZONE_ID}/purge_cache`, {
      method: "POST",
      headers: { "Authorization": `Bearer ${env.HOSTTHIS_CF_PURGE_TOKEN}`, "Content-Type": "application/json" },
      body: JSON.stringify({ hosts: [`${slug}.${apex}`] }),
    });
    return res.ok;
  } catch {
    return false;
  }
}

export async function abuseRoute(request, env, ctx, url, actor, { now = Date.now, fetcher = fetch, sleep } = {}) {
  const route = ROUTES[actor][url.pathname];
  if (!route) {
    return new Response("not found\n", { status: 404 });
  }
  const cell = env.ABUSE.get(env.ABUSE.idFromName("abuse"));
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
  if (purged) {
    // A read that resolved before the change can still fill the edge cache
    // for up to 10 seconds; a second purge drops it.
    const wait = sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));
    ctx?.waitUntil(wait(REPURGE_MS).then(() => purgeSlug(env, body.slug, fetcher)));
  }
  return Response.json({ ...answer, purged });
}
