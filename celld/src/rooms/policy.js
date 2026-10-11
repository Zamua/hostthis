// The rooms, push and WebSocket rules hostthisd enforces in internal/http,
// internal/service and internal/domain, for the Worker's public router
// (docs/SPEC.md "Rooms (app persistence)", "Real-time room relay", "Room
// push"). Pure functions, no I/O. Paths and keys are byte strings: one char
// per byte of the percent-decoded path, as a Go string holds them.

import { FORWARDED_CLIENT_IP } from "../public/host.js";
import { byteStringBytes, goText, validUTF8 } from "./wire.js";

export const ROOM_MAX_BYTES = 256 * 1024;
export const ROOM_MAX_KEYS = 256;
export const ROOM_MAX_KEY_BYTES = 256;
export const APP_MAX_ROOM_BYTES = 64 * 1024 * 1024;
export const ROOMS_PER_SUBNET = 60;
export const ROOMS_PER_APP = 300;
export const ROOM_CREATE_WINDOW_MS = 60 * 60 * 1000;
// One byte past the room cap: an oversized body still reaches the cell, whose
// cap refuses it as 413.
export const ROOM_PUT_READ_LIMIT = ROOM_MAX_BYTES + 1;
export const PUSH_BODY_LIMIT = 64 * 1024;

export const ROOM_SOCKET_CAP = 64;
export const ROOM_PING_STALE_MS = 120_000;
// An application close code, so a client tells eviction from a server fault.
export const ROOM_STALE_CLOSE = 4001;
export const ROOM_CLIENT_FRAME_LIMIT = 32 * 1024;
export const PING_FRAME = '{"type":"ping"}';
export const PONG_FRAME = '{"type":"pong"}';

const UUID_V4 = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$/;

// domain.ParseRoomID: lowercased, so two spellings address one room.
export function parseRoomID(text) {
  return UUID_V4.test(text) ? text.toLowerCase() : null;
}

const ROOMS_PREFIX = "/api/rooms";
export const PUSH_KEY_PATH = "/api/push/key";

// The rooms route a decoded app path names, or null outside the surface.
// "/api/roomsX" is a site path, not the API.
export function roomsRoute(path) {
  if (path === PUSH_KEY_PATH) {
    return { kind: "pushkey" };
  }
  let rest;
  if (path === ROOMS_PREFIX) {
    rest = "";
  } else if (path.startsWith(`${ROOMS_PREFIX}/`)) {
    rest = path.slice(ROOMS_PREFIX.length + 1);
  } else {
    return null;
  }
  if (rest === "") {
    return { kind: "collection" };
  }
  const slash = rest.indexOf("/");
  const id = parseRoomID(slash < 0 ? rest : rest.slice(0, slash));
  if (!id) {
    return { kind: "malformed" };
  }
  const key = slash < 0 ? "" : rest.slice(slash + 1);
  if (key === "") {
    return { kind: "scan", id };
  }
  if (key === "ws") {
    return { kind: "ws", id };
  }
  if (key === "push") {
    return { kind: "push", id, sub: "" };
  }
  if (key.startsWith("push/")) {
    return { kind: "push", id, sub: key.slice(4) };
  }
  return { kind: "value", id, key };
}

function isReservedRoomKey(key) {
  return key === "ws" || key === "push" || key.startsWith("push/");
}

// domain.ValidateRoomKey's message, or null for a valid key.
export function roomKeyProblem(key) {
  if (key === "") return "room key is empty";
  if (key.length > ROOM_MAX_KEY_BYTES) return "room key is too long";
  if (!validUTF8(byteStringBytes(key))) return "room key is not valid UTF-8";
  if (isReservedRoomKey(key)) return "room key is reserved";
  return null;
}

// --- client subnet -----------------------------------------------------------

// The address the creation limit counts by, as a string, or null.
export function clientAddress(headers, source, forwarded) {
  if (forwarded) {
    return headers.get(FORWARDED_CLIENT_IP)?.trim() ?? null;
  }
  if (source === "cf-connecting-ip") {
    return headers.get("CF-Connecting-IP")?.trim() ?? null;
  }
  if (source === "xff-rightmost") {
    const xff = headers.get("X-Forwarded-For");
    return xff ? xff.split(",").pop().trim() : null;
  }
  return null;
}

function parseIPv4(text) {
  const parts = text.split(".");
  if (parts.length !== 4 || !parts.every((p) => /^(0|[1-9][0-9]{0,2})$/.test(p) && Number(p) <= 255)) {
    return null;
  }
  return parts.map(Number);
}

function parseIPv6(text) {
  if (text.includes("%")) {
    return null;
  }
  const halves = text.split("::");
  if (halves.length > 2) {
    return null;
  }
  const groups = [];
  const parseHalf = (half, last) => {
    if (half === "") {
      return [];
    }
    const out = [];
    const parts = half.split(":");
    for (let n = 0; n < parts.length; n++) {
      const part = parts[n];
      if (last && n === parts.length - 1 && part.includes(".")) {
        const v4 = parseIPv4(part);
        if (!v4) return null;
        out.push((v4[0] << 8) | v4[1], (v4[2] << 8) | v4[3]);
      } else if (/^[0-9a-fA-F]{1,4}$/.test(part)) {
        out.push(parseInt(part, 16));
      } else {
        return null;
      }
    }
    return out;
  };
  const head = parseHalf(halves[0], halves.length === 1);
  const tail = halves.length === 2 ? parseHalf(halves[1], true) : [];
  if (!head || !tail) {
    return null;
  }
  if (halves.length === 1) {
    if (head.length !== 8) return null;
    groups.push(...head);
  } else {
    if (head.length + tail.length > 7) return null;
    groups.push(...head, ...new Array(8 - head.length - tail.length).fill(0), ...tail);
  }
  return groups;
}

// net.ParseIP's address as eight 16-bit groups, IPv4 as IPv4-mapped; null
// where ParseIP returns nil (zones included).
export function parseIP(text) {
  if (text.includes(":")) {
    return parseIPv6(text);
  }
  const v4 = parseIPv4(text);
  return v4 && [0, 0, 0, 0, 0, 0xffff, (v4[0] << 8) | v4[1], (v4[2] << 8) | v4[3]];
}

// netip's IPv6 text: lowercase, leading zeros dropped, the first longest run
// of two or more zero groups as "::".
function formatIPv6(groups) {
  let zeroStart = -1;
  let zeroEnd = -1;
  for (let i = 0; i < 8; i++) {
    let j = i;
    while (j < 8 && groups[j] === 0) j++;
    if (j - i >= 2 && j - i > zeroEnd - zeroStart) {
      zeroStart = i;
      zeroEnd = j;
    }
  }
  let out = "";
  for (let i = 0; i < 8; i++) {
    if (i === zeroStart) {
      out += "::";
      i = zeroEnd;
      if (i >= 8) break;
    } else if (i > 0) {
      out += ":";
    }
    out += groups[i].toString(16);
  }
  return out;
}

// hostthisd's ipSubnet: the /24 of an IPv4 (or IPv4-mapped) address, the /48
// of an IPv6 one, "unknown" for none.
export function ipSubnet(groups) {
  if (!groups) {
    return "unknown";
  }
  if (groups.slice(0, 5).every((g) => g === 0) && groups[5] === 0xffff) {
    return `${groups[6] >> 8}.${groups[6] & 0xff}.${groups[7] >> 8}.0/24`;
  }
  return `${formatIPv6([...groups.slice(0, 3), 0, 0, 0, 0, 0])}/48`;
}

export function clientSubnet(address) {
  return ipSubnet(address ? parseIP(address) : null);
}

// --- Go formatting -----------------------------------------------------------

const QUOTE_ESCAPES = { 7: "\\a", 8: "\\b", 9: "\\t", 10: "\\n", 11: "\\v", 12: "\\f", 13: "\\r" };

// strconv.Quote, for the messages Go formats with %q.
export function goQuote(text) {
  let out = '"';
  for (const ch of text) {
    const cp = ch.codePointAt(0);
    if (ch === '"' || ch === "\\") {
      out += `\\${ch}`;
    } else if (QUOTE_ESCAPES[cp]) {
      out += QUOTE_ESCAPES[cp];
    } else if (cp < 0x20 || cp === 0x7f) {
      out += `\\x${cp.toString(16).padStart(2, "0")}`;
    } else if ((cp >= 0x80 && cp <= 0x9f) || cp === 0x2028 || cp === 0x2029 || cp === 0xfeff) {
      out += `\\u${cp.toString(16).padStart(4, "0")}`;
    } else {
      out += ch;
    }
  }
  return `${out}"`;
}

function utf8Length(text) {
  return new TextEncoder().encode(text).length;
}

// --- URLs and origins --------------------------------------------------------

function validOptionalPort(port) {
  return port === "" || /^:[0-9]*$/.test(port);
}

// Bytes url.Parse refuses raw in a host: ASCII outside alphanumerics and
// -._~!$&'()*+,;=:[]<>".
const HOST_REFUSED = /[^A-Za-z0-9\-._~!$&'()*+,;=:[\]<>"\x80-￿%]/;

function unescapeHost(host) {
  let out = "";
  for (let i = 0; i < host.length; i++) {
    if (host[i] !== "%") {
      out += host[i];
      continue;
    }
    const hex = host.slice(i + 1, i + 3);
    if (!/^[0-9a-fA-F]{2}$/.test(hex) || (parseInt(hex[0], 16) < 8 && hex !== "25")) {
      return null;
    }
    out += String.fromCharCode(parseInt(hex, 16));
    i += 2;
  }
  return out;
}

// The parts of url.Parse the push and origin rules read; null where url.Parse
// fails.
export function goURL(raw) {
  if (/[\x00-\x1f\x7f]/.test(raw)) {
    return null;
  }
  let rest = raw.split("#")[0];
  let scheme = "";
  for (let i = 0; i < rest.length; i++) {
    const c = rest[i];
    if (/[A-Za-z]/.test(c)) continue;
    if (/[0-9+.-]/.test(c) && i > 0) continue;
    if (c === ":") {
      if (i === 0) return null;
      scheme = rest.slice(0, i).toLowerCase();
      rest = rest.slice(i + 1);
    }
    break;
  }
  rest = rest.split("?")[0];
  const url = { scheme, user: false, host: "" };
  if (!rest.startsWith("/")) {
    if (!scheme && rest.split("/")[0].includes(":")) return null;
    return url;
  }
  if (!rest.startsWith("//") || (!scheme && rest.startsWith("///"))) {
    return url;
  }
  let authority = rest.slice(2).split("/")[0];
  const at = authority.lastIndexOf("@");
  if (at >= 0) {
    url.user = true;
    authority = authority.slice(at + 1);
  }
  if (authority.startsWith("[")) {
    const close = authority.lastIndexOf("]");
    if (close < 0 || !validOptionalPort(authority.slice(close + 1))) return null;
  } else {
    const colon = authority.lastIndexOf(":");
    if (colon >= 0 && !validOptionalPort(authority.slice(colon))) return null;
    if (HOST_REFUSED.test(authority)) return null;
  }
  const host = unescapeHost(authority);
  if (host === null) {
    return null;
  }
  url.host = host;
  return url;
}

// URL.Hostname: the host without its port or IPv6 brackets.
function hostname(host) {
  let name = host;
  const colon = name.lastIndexOf(":");
  if (colon >= 0 && validOptionalPort(name.slice(colon))) {
    name = name.slice(0, colon);
  }
  return name.startsWith("[") && name.endsWith("]") ? name.slice(1, -1) : name;
}

// filepath.Match for the patterns the origin rule builds: "*" is any run of
// characters other than "/", "?" any one of them.
function globMatch(pattern, text) {
  const source = pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, "[^/]*").replace(/\?/g, "[^/]");
  return new RegExp(`^${source}$`).test(text);
}

// coder/websocket's authenticateOrigin with hostthisd's patterns: the refusal
// message, or null when the origin may open the socket. requestHost is the
// Host the client addressed, port included.
export function originRefusal(origin, requestHost, apex) {
  if (!origin) {
    return null;
  }
  const url = goURL(origin);
  if (!url) {
    return `failed to parse Origin header ${goQuote(origin)}`;
  }
  if (requestHost.toLowerCase() === url.host.toLowerCase()) {
    return null;
  }
  for (const pattern of [`*.${apex}`, apex]) {
    if (globMatch(pattern.toLowerCase(), url.host.toLowerCase())) {
      return null;
    }
  }
  if (url.host === "") {
    return `request Origin ${goQuote(origin)} is not a valid URL with a host`;
  }
  return `request Origin ${goQuote(url.host)} is not authorized for Host ${goQuote(requestHost)}`;
}

function headerHasToken(value, token) {
  return (value ?? "").split(",").some((part) => part.trim().toLowerCase() === token);
}

// The handshake checks the Worker can make before passing the upgrade on:
// { status, message, headers } or null. The runtime completes the rest.
export function upgradeRefusal(method, headers) {
  const upgrade = headers.get("Upgrade");
  if (!headerHasToken(upgrade, "websocket")) {
    return {
      status: 426,
      message: `WebSocket protocol violation: Upgrade header ${goQuote(upgrade ?? "")} does not contain websocket`,
      headers: { Connection: "Upgrade", Upgrade: "websocket" },
    };
  }
  if (method !== "GET") {
    return {
      status: 405,
      message: `WebSocket protocol violation: handshake request method is not GET but ${goQuote(method)}`,
      headers: {},
    };
  }
  return null;
}

// --- push bodies ---------------------------------------------------------------

class Mismatch extends Error {}

// encoding/json's field-name folding (appendFoldedName).
function foldName(name) {
  let out = "";
  for (const ch of name) {
    out += ch < "\x80" ? ch.toUpperCase() : ch.toLowerCase().toUpperCase();
  }
  return out;
}

function asString(current, node) {
  if (node.t === "null") return current;
  if (node.t === "string") return node.v;
  throw new Mismatch();
}

const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

// A JSON number decodes into a Go int only as a plain integer literal in range.
function asInt(current, node) {
  if (node.t === "null") return current;
  if (node.t === "number" && /^-?[0-9]+$/.test(node.raw)) {
    const value = BigInt(node.raw);
    if (value >= INT64_MIN && value <= INT64_MAX) return Number(value);
  }
  throw new Mismatch();
}

// null sets a slice to nil; an array decodes element by element into what is
// already there, then truncates.
function sliceOf(element, zero) {
  return (current, node) => {
    if (node.t === "null") return null;
    if (node.t !== "array") throw new Mismatch();
    return node.items.map((item, i) => element(current && i < current.length ? current[i] : zero(), item));
  };
}

function structOf(fields) {
  const folded = Object.keys(fields).map((name) => [foldName(name), name]);
  return (current, node) => {
    if (node.t === "null") return current;
    if (node.t !== "object") throw new Mismatch();
    for (const [key, value] of node.entries) {
      const match = folded.find(([fold]) => fold === foldName(key));
      if (match) {
        current[match[1]] = fields[match[1]](current[match[1]], value);
      }
    }
    return current;
  };
}

const ZERO_KEYS = () => ({ p256dh: "", auth: "" });
const ZERO_SUBSCRIPTION = () => ({ endpoint: "", keys: ZERO_KEYS() });
const ZERO_ENDPOINT = () => ({ endpoint: "" });
const ZERO_ITEM = () => ({
  id: "", at: "", days: null, when: "", title: "", body: "", bodyKey: "", url: "", tag: "",
});
const ZERO_SCHEDULE = () => ({ tz: "", items: null });

const decodeKeys = structOf({ p256dh: asString, auth: asString });
const decodeSubscriptionNode = structOf({ endpoint: asString, keys: (current, node) => decodeKeys(current, node) });
const decodeEndpointNode = structOf({ endpoint: asString });
const decodeItem = structOf({
  id: asString, at: asString, days: sliceOf(asInt, () => 0), when: asString, title: asString,
  body: asString, bodyKey: asString, url: asString, tag: asString,
});
const decodeScheduleNode = structOf({ tz: asString, items: sliceOf(decodeItem, ZERO_ITEM) });

function decodeInto(zero, decode, node) {
  if (node === null) {
    return null;
  }
  try {
    return decode(zero(), node);
  } catch (error) {
    if (error instanceof Mismatch) return null;
    throw error;
  }
}

// Each decodes a parsed body (wire.js decodeFirstJSON) as Go decodes it into
// the matching domain type; null is hostthisd's "malformed JSON body".
export const decodeSubscription = (node) => decodeInto(ZERO_SUBSCRIPTION, decodeSubscriptionNode, node);
export const decodeEndpoint = (node) => decodeInto(ZERO_ENDPOINT, decodeEndpointNode, node);
export const decodeSchedule = (node) => decodeInto(ZERO_SCHEDULE, decodeScheduleNode, node);

const PUSH_SERVICES = ["push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com"];
const PUSH_INVALID = "push: invalid";
const MAX_PUSH_ENDPOINT = 1024;
const MAX_PUSH_ITEMS = 16;

function invalid(detail) {
  return `${PUSH_INVALID}: ${detail}`;
}

// base64.RawURLEncoding after trailing "=" are trimmed, which also skips
// CR and LF; null where it fails.
function rawURLBase64(text) {
  const trimmed = text.replace(/=+$/, "").replace(/[\r\n]/g, "");
  if (trimmed === "" || !/^[A-Za-z0-9_-]*$/.test(trimmed) || trimmed.length % 4 === 1) {
    return null;
  }
  const binary = atob(trimmed.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (trimmed.length % 4)) % 4));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

// domain.ValidatePushSubscription's message, or null.
export function pushSubscriptionProblem(sub) {
  if (sub.endpoint === "") {
    return invalid("endpoint is required");
  }
  if (utf8Length(sub.endpoint) > MAX_PUSH_ENDPOINT) {
    return invalid(`endpoint exceeds ${MAX_PUSH_ENDPOINT} bytes`);
  }
  const url = goURL(sub.endpoint);
  if (!url || url.scheme !== "https" || url.host === "") {
    return invalid("endpoint must be an https URL");
  }
  const host = hostname(url.host).toLowerCase();
  if (url.user || !PUSH_SERVICES.some((base) => host === base || host.endsWith(`.${base}`))) {
    return invalid("endpoint host is not a known push service");
  }
  const p256dh = rawURLBase64(sub.keys.p256dh);
  if (!p256dh || p256dh.length !== 65 || p256dh[0] !== 4) {
    return invalid("keys.p256dh must be a base64url uncompressed P-256 point");
  }
  const auth = rawURLBase64(sub.keys.auth);
  if (!auth || auth.length !== 16) {
    return invalid("keys.auth must be 16 base64url bytes");
  }
  return null;
}

// RoomPush.DeleteSubscription's check.
export function pushEndpointProblem(endpoint) {
  return endpoint === "" || utf8Length(endpoint) > MAX_PUSH_ENDPOINT ? PUSH_INVALID : null;
}

function daysIn(year, month) {
  if (month === 2) {
    return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0 ? 29 : 28;
  }
  return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,](\d+))?(?:Z|([+-])(\d{2}):(\d{2}))$/;

// time.Parse(time.RFC3339, text) in epoch milliseconds, or null.
export function parseRFC3339(text) {
  const m = RFC3339.exec(text);
  if (!m) {
    return null;
  }
  const [year, month, day, hour, minute, second] = m.slice(1, 7).map(Number);
  if (month < 1 || month > 12 || day < 1 || day > daysIn(year, month) || hour > 23 || minute > 59 || second > 59) {
    return null;
  }
  const offsetHours = m[8] ? Number(m[9]) : 0;
  const offsetMinutes = m[8] ? Number(m[10]) : 0;
  if (offsetHours > 24 || offsetMinutes > 60) {
    return null;
  }
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  date.setUTCHours(hour, minute, second, m[7] ? Number(m[7].slice(0, 3).padEnd(3, "0")) : 0);
  const offset = (m[8] === "-" ? -1 : 1) * (offsetHours * 60 + offsetMinutes) * 60_000;
  return date.getTime() - offset;
}

function isPushClock(text) {
  return /^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(text);
}

const PUSH_DATE_SAMPLE = "2026-09-06";

function pushItemProblem(item, now) {
  if (item.id === "" || utf8Length(item.id) > 32) {
    return invalid("id must be 1 to 32 characters");
  }
  if (!/^[A-Za-z0-9_-]+$/.test(item.id)) {
    return invalid(`id ${goQuote(item.id)} has a character outside [A-Za-z0-9_-]`);
  }
  if (item.title === "" || utf8Length(item.title) > 64) {
    return invalid("title must be 1 to 64 bytes");
  }
  if (utf8Length(item.url) > 512) {
    return invalid("url exceeds 512 bytes");
  }
  if (utf8Length(item.tag) > 64) {
    return invalid("tag exceeds 64 bytes");
  }
  if ((item.body !== "") === (item.bodyKey !== "")) {
    return invalid("exactly one of body and bodyKey is required");
  }
  if (item.body !== "") {
    if (utf8Length(item.body) > 1024) {
      return invalid("body exceeds 1024 bytes");
    }
  } else {
    const key = item.bodyKey.replaceAll("{date}", PUSH_DATE_SAMPLE);
    const problem = roomKeyProblem(byteString(key));
    if (problem) {
      return invalid(`bodyKey: ${problem}`);
    }
  }
  if (item.when !== "") {
    if (item.at !== "" || (item.days?.length ?? 0) > 0) {
      return invalid("when excludes at and days");
    }
    const when = parseRFC3339(item.when);
    if (when === null) {
      return invalid("when must be RFC 3339 with an offset");
    }
    return when > now ? null : invalid("when is not in the future");
  }
  if (!isPushClock(item.at)) {
    return invalid("at must be HH:MM");
  }
  if (!item.days?.length) {
    return invalid("days must name at least one weekday");
  }
  const seen = new Set();
  for (const day of item.days) {
    if (day < 0 || day > 6) {
      return invalid(`day ${day} is outside 0..6`);
    }
    if (seen.has(day)) {
      return invalid(`day ${day} repeats`);
    }
    seen.add(day);
  }
  return null;
}

// domain.ValidatePushSchedule's message, or null. now is epoch milliseconds.
// Whether tz names a zone is the Room cell's to judge.
export function pushScheduleProblem(schedule, now) {
  const items = schedule.items ?? [];
  if (items.length > MAX_PUSH_ITEMS) {
    return invalid(`more than ${MAX_PUSH_ITEMS} items`);
  }
  if (schedule.tz === "") {
    return invalid("tz is required");
  }
  const ids = new Set();
  for (let i = 0; i < items.length; i++) {
    const problem = pushItemProblem(items[i], now);
    if (problem) {
      return `${problem} (item ${i})`;
    }
    if (ids.has(items[i].id)) {
      return invalid(`duplicate id ${goQuote(items[i].id)}`);
    }
    ids.add(items[i].id);
  }
  return null;
}

// A push item as Go marshals domain.PushItem: field order kept, empty fields
// omitted.
export function pushItemWire(item) {
  const out = { id: item.id ?? "" };
  if (item.at) out.at = item.at;
  if (item.days?.length) out.days = [...item.days];
  if (item.when) out.when = item.when;
  out.title = item.title ?? "";
  for (const field of ["body", "bodyKey", "url", "tag"]) {
    if (item[field]) out[field] = item[field];
  }
  return out;
}

// A cell timestamp as Go's time.Time marshals it: RFC 3339 in UTC, the
// fraction without trailing zeros.
export function goTime(value) {
  const iso = new Date(value).toISOString();
  return iso.replace(/\.(\d*?)0*Z$/, (_, digits) => (digits ? `.${digits}Z` : "Z"));
}

// Text as the byte string of its UTF-8.
export function byteString(text) {
  let out = "";
  for (const byte of new TextEncoder().encode(text)) {
    out += String.fromCharCode(byte);
  }
  return out;
}

// A byte string as the text a Go string becomes in JSON.
export function byteStringText(bytes) {
  return goText(byteStringBytes(bytes));
}
