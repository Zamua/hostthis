// Host names as the router compares them. Pure functions, no I/O.

// Names the public host when the internal router forwards a public request.
export const FORWARDED_HOST = "X-Hostthis-Forwarded-Host";
// The client address of a forwarded request.
export const FORWARDED_CLIENT_IP = "X-Hostthis-Client-IP";

// Lowercase, without a port or one trailing dot. A bracketed IPv6 literal
// keeps its brackets: its colons sit inside them, so only a trailing ":<port>"
// can match.
export function normalizeHost(value) {
  const host = String(value).trim().toLowerCase().replace(/:\d*$/, "");
  return host.endsWith(".") ? host.slice(0, -1) : host;
}

// Both arguments normalized.
export function withinApex(host, apex) {
  return host === apex || host.endsWith(`.${apex}`);
}

// Dot-separated labels of letters, digits and hyphens.
export function isHostname(host) {
  return /^[a-z0-9-]+(\.[a-z0-9-]+)*$/.test(host);
}

// domain.ParseSlug.
export function parseSlug(text) {
  return /^[abcdefghijkmnpqrstuvwxyz23456789]{8}$/.test(text) ? text : null;
}

// The slug of "<slug>.<apex>" whose one label parses as a slug, else null.
// Both arguments normalized.
export function slugOfHost(host, apex) {
  if (!apex || !host.endsWith(`.${apex}`)) {
    return null;
  }
  return parseSlug(host.slice(0, -apex.length - 1));
}
