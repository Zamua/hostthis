// Outbound requests to addresses an owner's WebID profile names: the
// profile, its identity provider, the provider's endpoints and the pod.
// Every one is checked before it is fetched, redirects are followed only to
// addresses that pass the same check, and bodies the Worker parses are read
// under a cap, so a profile cannot point the Worker at a private network or
// at hostthis itself, nor exhaust its memory.

import { normalizeHost, slugHostSuffix, slugOfHost } from "../public/host.js";

// Larger than any profile, provider document, token answer or container
// listing a real pod sends.
export const SOLID_READ_LIMIT = 1024 * 1024;
const MAX_REDIRECTS = 5;

export class RefusedAddress extends Error {}

function loopback(host) {
  return host === "localhost" || host.endsWith(".localhost");
}

// An IPv4 or IPv6 literal, in any spelling URL parsing normalizes to.
function ipLiteral(host) {
  return host.startsWith("[") || /^[0-9.]+$/.test(host) || /^0x/i.test(host);
}

// The addresses a profile may name: https on a public host name. A local
// runtime (env HOSTTHIS_SOLID_ALLOW_LOOPBACK) also reaches loopback names, as
// a WebID on localhost may use plain http (domain.ParseWebID).
export function allowedPodUrl(raw, env) {
  let url;
  try {
    url = new URL(raw);
  } catch {
    return false;
  }
  const host = url.hostname.toLowerCase().replace(/\.$/, "");
  if (loopback(host)) {
    return env?.HOSTTHIS_SOLID_ALLOW_LOOPBACK === "1" && (url.protocol === "https:" || url.protocol === "http:");
  }
  if (url.protocol !== "https:" || url.username || url.password || host === "" || ipLiteral(host)) {
    return false;
  }
  if (!host.includes(".") || /\.(local|internal|lan|home\.arpa)$/.test(host)) {
    return false;
  }
  if (host === "workers.dev" || host.endsWith(".workers.dev")) {
    return false;
  }
  const apex = normalizeHost(env?.HOSTTHIS_APEX_DOMAIN ?? "");
  if (apex && (host === apex || host.endsWith(`.${apex}`))) {
    return false;
  }
  return slugOfHost(host, slugHostSuffix(env)) === null;
}

// fetch() for a profile-named address: refuses a disallowed one, and follows
// a GET or HEAD redirect only to an allowed address. Other methods do not
// follow redirects; the caller sees the 3xx as a refusal.
export async function podNetFetch(env, url, init = {}) {
  const method = (init.method ?? "GET").toUpperCase();
  let target = url;
  for (let hop = 0; ; hop++) {
    if (!allowedPodUrl(target, env)) {
      throw new RefusedAddress(`refused to contact ${safeOrigin(target)}: not a public https address`);
    }
    const res = await fetch(target, { ...init, redirect: "manual" });
    const location = res.headers.get("Location");
    if (res.status < 300 || res.status >= 400 || !location || (method !== "GET" && method !== "HEAD") || hop === MAX_REDIRECTS) {
      return res;
    }
    await res.body?.cancel();
    target = new URL(location, target).href;
  }
}

function safeOrigin(raw) {
  try {
    return new URL(raw).origin;
  } catch {
    return "an invalid address";
  }
}

// The body as text, refused past the cap.
export async function readTextCapped(res, limit = SOLID_READ_LIMIT) {
  if (!res.body) {
    return "";
  }
  const reader = res.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    total += value.length;
    if (total > limit) {
      await reader.cancel().catch(() => {});
      throw new Error(`answer larger than ${limit} bytes`);
    }
    chunks.push(value);
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return new TextDecoder().decode(out);
}

export async function readJsonCapped(res, limit = SOLID_READ_LIMIT) {
  return JSON.parse(await readTextCapped(res, limit));
}
