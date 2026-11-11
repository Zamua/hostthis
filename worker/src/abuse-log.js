import { clientAddress, formatIPv6, ipv4Subnet, isIPv4Mapped, parseIP, roomsRoute } from "./rooms/policy.js";

// The client address in its canonical text, as hostthisd writes it: an
// IPv4-mapped IPv6 address reads as IPv4. "unknown" when the text is no address.
export function logAddr(address) {
  const groups = typeof address === "string" ? parseIP(address.trim()) : null;
  if (!groups) {
    return "unknown";
  }
  if (isIPv4Mapped(groups)) {
    return [groups[6] >> 8, groups[6] & 0xff, groups[7] >> 8, groups[7] & 0xff].join(".");
  }
  return formatIPv6(groups);
}

// The subnet a log line groups an address under: the IPv4 /24 or IPv6 /40.
// "unknown" when the text is no address.
export function logSubnet(address) {
  const groups = typeof address === "string" ? parseIP(address.trim()) : null;
  if (!groups) {
    return "unknown";
  }
  if (isIPv4Mapped(groups)) {
    return ipv4Subnet(groups);
  }
  return `${formatIPv6([groups[0], groups[1], groups[2] & 0xff00, 0, 0, 0, 0, 0])}/40`;
}

// One line per abuse event, with the client's address and the subnet it
// groups under. Headers and bodies are not parameters, so a caller cannot put
// them in the line.
export function abuseLog(event, { route, status, ip, country, asn }) {
  console.log(JSON.stringify({
    event,
    route,
    status,
    addr: logAddr(ip),
    subnet: logSubnet(ip),
    country: typeof country === "string" && /^[A-Za-z0-9]{2}$/.test(country) ? country.toUpperCase() : null,
    asn: Number.isSafeInteger(asn) && asn > 0 ? asn : null,
  }));
}

export function abuseEventOf(kind, status) {
  switch (status) {
    case 201:
      return kind === "collection" ? "created" : null;
    case 401:
    case 403:
      return "auth-failure";
    case 413:
      return "oversize";
    case 429:
      return "rate-limited";
    case 507:
      return "capacity";
    default:
      return null;
  }
}

// Logs a rooms-surface response when it is an abuse event; never throws, since
// logging must not fail a request.
export function noteRoomResponse(request, env, forwarded, path, status) {
  try {
    const kind = roomsRoute(path)?.kind;
    const event = abuseEventOf(kind, status);
    if (!event) {
      return;
    }
    abuseLog(event, {
      route: kind,
      status,
      ip: clientAddress(request.headers, env.HOSTTHIS_CLIENT_IP_SOURCE, forwarded),
      country: forwarded ? null : request.cf?.country,
      asn: forwarded ? null : request.cf?.asn,
    });
  } catch {
    // Dropped on purpose.
  }
}
