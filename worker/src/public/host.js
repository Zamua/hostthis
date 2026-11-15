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

// What follows the slug in an app's host: ".<apex>", or the flat suffix
// HOSTTHIS_SLUG_HOST_SUFFIX names ("-staging.example.com" serves
// "<slug>-staging.example.com"). Empty without either.
export function slugHostSuffix(env) {
  const flat = normalizeHost(env?.HOSTTHIS_SLUG_HOST_SUFFIX ?? "");
  if (flat) {
    return flat;
  }
  const apex = normalizeHost(env?.HOSTTHIS_APEX_DOMAIN ?? "");
  return apex ? `.${apex}` : "";
}

// The slug of "<slug><suffix>" whose prefix parses as a slug, else null.
// host normalized.
export function slugOfHost(host, suffix) {
  if (!suffix || !host.endsWith(suffix)) {
    return null;
  }
  return parseSlug(host.slice(0, -suffix.length));
}

// A host the public router serves: the apex, a name under it, or an app's host.
export function publicHost(host, apex, suffix) {
  return withinApex(host, apex) || slugOfHost(host, suffix) !== null;
}
