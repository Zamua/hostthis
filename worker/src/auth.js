// Who may call the internal routes (docs/SPEC.md "Internal credentials"): the
// full token, which reaches everything and may be bound to a list of client
// addresses, and two narrower tokens for operator tools.
import { clientAddress, isIPv4Mapped, parseIP } from "./rooms/policy.js";

const BEARER = /^bearer +/i;

async function digest(text) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
}

// Both sides are hashed first, so the compare runs over two equal length
// digests and its timing says nothing about the token.
function sameDigest(want, got) {
  let diff = 0;
  for (let i = 0; i < want.length; i++) {
    diff |= want[i] ^ got[i];
  }
  return diff === 0;
}

// The request's bearer token as a digest, or null without one.
async function bearerDigest(request) {
  const header = request.headers.get("Authorization") ?? "";
  const scheme = BEARER.exec(header);
  return scheme ? digest(header.slice(scheme[0].length)) : null;
}

// Whether the request's bearer is token. An unset token matches nothing.
export async function bearerIs(request, token) {
  if (!token) {
    return false;
  }
  const got = await bearerDigest(request);
  return got !== null && sameDigest(await digest(token), got);
}

// The bit prefix of an address or `address/length` entry, IPv4 counted inside
// the IPv4-mapped range so one comparison serves both families. Null when the
// text is neither.
function parseNetwork(text) {
  const [base, length, extra] = text.trim().split("/");
  const groups = parseIP(base ?? "");
  if (!groups || extra !== undefined) {
    return null;
  }
  const v4 = isIPv4Mapped(groups);
  const width = v4 ? 32 : 128;
  const bits = length === undefined ? width : Number(length);
  if (!Number.isInteger(bits) || bits < 0 || bits > width || (length !== undefined && !/^\d+$/.test(length))) {
    return null;
  }
  return { groups, bits: v4 ? 96 + bits : bits };
}

function inNetwork(groups, network) {
  for (let i = 0, left = network.bits; left > 0; i++, left -= 16) {
    const mask = left >= 16 ? 0xffff : (0xffff << (16 - left)) & 0xffff;
    if ((groups[i] & mask) !== (network.groups[i] & mask)) {
      return false;
    }
  }
  return true;
}

// Whether address is in the comma separated list of addresses and CIDR
// prefixes. An empty list, an entry that does not parse, or an address that
// does not parse is false.
export function addressListed(address, list) {
  const networks = String(list ?? "").split(",").map((entry) => entry.trim()).filter(Boolean).map(parseNetwork);
  const groups = parseIP(address ?? "");
  return groups !== null && networks.length > 0 && !networks.includes(null) &&
    networks.some((network) => inNetwork(groups, network));
}

// Whether given is secret, compared in constant time. An unset secret matches
// nothing.
export async function secretIs(given, secret) {
  return Boolean(secret) && sameDigest(await digest(secret), await digest(given ?? ""));
}

// The networks HOSTTHIS_INTERNAL_ALLOW names (comma separated addresses and
// CIDR prefixes), or null when it is unset. An entry that does not parse
// makes the list match nothing, so a typo locks the full token out instead
// of opening it.
function allowedNetworks(env) {
  const raw = String(env.HOSTTHIS_INTERNAL_ALLOW ?? "").trim();
  if (raw === "") {
    return null;
  }
  const networks = raw.split(",").map((entry) => parseNetwork(entry));
  return networks.includes(null) ? [] : networks;
}

// Whether the full token may be used from this request's client address. With
// no list every address may. With one, the address comes from the source the
// deployment names in HOSTTHIS_CLIENT_IP_SOURCE and nowhere else: a header the
// deployment did not name is never read, so on a runtime with no trusted edge
// (where a client sets any header it likes) a forged address proves nothing,
// and an unknown address is refused.
function fullTokenAllowedHere(request, env) {
  const networks = allowedNetworks(env);
  if (networks === null) {
    return true;
  }
  const groups = parseIP(clientAddress(request.headers, env.HOSTTHIS_CLIENT_IP_SOURCE, false) ?? "");
  return groups !== null && networks.some((network) => inNetwork(groups, network));
}

// The scope the request's credential grants: "full", "admin", "read", or null.
// Every configured token is compared, whichever matches, so timing does not
// say which one a guess resembled; a caller learns only whether it got in.
// With no HOSTTHIS_WORKER_TOKEN the internal routes are open, as a deployment
// reachable only inside its cluster runs them.
export async function credentialScope(request, env) {
  if (!env.HOSTTHIS_WORKER_TOKEN) {
    return "full";
  }
  const got = await bearerDigest(request);
  if (got === null) {
    return null;
  }
  const matches = async (token) => Boolean(token) && sameDigest(await digest(token), got);
  const [full, next, admin, read] = await Promise.all([
    matches(env.HOSTTHIS_WORKER_TOKEN), matches(env.HOSTTHIS_WORKER_TOKEN_NEXT),
    matches(env.HOSTTHIS_ADMIN_TOKEN), matches(env.HOSTTHIS_READ_TOKEN),
  ]);
  if (full || next) {
    return fullTokenAllowedHere(request, env) ? "full" : null;
  }
  if (admin) {
    return "admin";
  }
  return read ? "read" : null;
}

// What the read token may call: reads of rows, listings and stored bytes.
const READ_SCOPE = {
  paste: ["get", "versions", "resolve", "exists", "operatorrow", "roomindex", "head"],
  identity: ["bytes", "list", "firstSeen", "subnetlist"],
  denylist: ["list"],
  abuse: ["list", "audit", "bans"],
  room: ["count", "meta", "get", "scan"],
};

// What the admin token may call beyond that: the operator's abuse actions,
// the denylist, and the two deletions a takedown makes.
const ADMIN_SCOPE = {
  paste: ["remove"],
  denylist: ["add", "remove"],
  abuse: ["quarantine", "release", "room-delete", "ban-owner", "unban-owner"],
};

// Exported for the test that holds each name to its cell's op table.
export const SCOPED_OPS = { read: READ_SCOPE, admin: ADMIN_SCOPE };

// Whether a scope may make this internal request. "full" may make any. The
// narrower scopes name their operations by the path alone: a request that
// carries an `op` query (which a cell would also accept as the operation) is
// refused outright, so the path checked here is the operation that runs.
export function scopePermits(scope, method, url) {
  if (scope === "full") {
    return true;
  }
  if (scope !== "admin" && scope !== "read") {
    return false;
  }
  if (url.searchParams.has("op")) {
    return false;
  }
  const segments = url.pathname.split("/");
  const [, family, op] = segments;
  if (family === "blob") {
    if (method === "GET" && segments.length > 2) {
      return true;
    }
    return scope === "admin" && method === "DELETE" && url.pathname === "/blob" && url.searchParams.has("prefix");
  }
  if (segments.length !== 3) {
    return false;
  }
  if (READ_SCOPE[family]?.includes(op)) {
    return true;
  }
  return scope === "admin" && Boolean(ADMIN_SCOPE[family]?.includes(op));
}
