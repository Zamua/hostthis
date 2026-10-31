import { b64url } from "./solid/jose.js";
// Web Push: RFC 8030 delivery, RFC 8291 aes128gcm encryption, RFC 8292 VAPID.
// The runtime imports no HKDF and no raw EC point, so HKDF is HMAC by hand and
// a subscriber's point is wrapped in the P-256 SPKI prefix before import.

export const PUSH_KEY = "push";
export const PUSH_MAX_SUBSCRIPTIONS = 16;
const PUSH_MAX_ITEMS = 16;
const PUSH_MAX_ENDPOINT = 1024;
const PUSH_MAX_BODY = 1024;
export const PUSH_MAX_PAYLOAD = 2048;
const PUSH_MAX_ROOM_KEY = 256;
export const PUSH_DAILY_CAP = 8;
export const PUSH_STALE_MS = 15 * 60 * 1000;
export const PUSH_TEST_INTERVAL_MS = 60 * 1000;
export const PUSH_SEND_TIMEOUT_MS = 10 * 1000;
const PUSH_RECORD_SIZE = 4096;
// Push-service hosts, matched exactly or as a dot-suffix; ciphertext and VAPID
// tokens go nowhere else.
const PUSH_SERVICE_HOSTS = [
  "push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com",
];
export const VAPID_TTL_MS = 12 * 60 * 60 * 1000;
export const VAPID_KEY = "pushVapid";
// A push service can answer 404 for a registration it has not finished
// propagating; a gone answer inside this window is a failed send, not a prune.
export const PUSH_PRUNE_GRACE_MS = 60_000;
const ECDH_P256 = { name: "ECDH", namedCurve: "P-256" };
export const ECDSA_P256 = { name: "ECDSA", namedCurve: "P-256" };
const P256_SPKI_PREFIX = new Uint8Array([
  48, 89, 48, 19, 6, 7, 42, 134, 72, 206, 61, 2, 1, 6, 8, 42, 134, 72, 206, 61, 3, 1, 7, 3, 66, 0,
]);
const PUSH_ID_PATTERN = /^[A-Za-z0-9_-]{1,32}$/;
const PUSH_AT_PATTERN = /^([01][0-9]|2[0-3]):[0-5][0-9]$/;
const RFC3339_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

export function utf8(text) {
  return new TextEncoder().encode(text);
}

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

// Accepts base64url or base64, padded or not; null for anything else.
export function unb64u(text) {
  if (typeof text !== "string" || !/^[A-Za-z0-9_=+/-]*$/.test(text)) {
    return null;
  }
  const normalized = text.replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
  if (normalized.length % 4 === 1) {
    return null;
  }
  try {
    const binary = atob(normalized + "=".repeat((4 - (normalized.length % 4)) % 4));
    return Uint8Array.from(binary, (c) => c.charCodeAt(0));
  } catch {
    return null;
  }
}

async function hmacSha256(key, data) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, data));
}

// HKDF-SHA256 extract + expand, one block: every length the protocol asks for
// is at most 32 bytes.
async function hkdf(salt, ikm, info, length) {
  const prk = await hmacSha256(salt, ikm);
  const block = await hmacSha256(prk, concat(info, new Uint8Array([1])));
  return block.slice(0, length);
}

function importP256Point(point) {
  return crypto.subtle.importKey("spki", concat(P256_SPKI_PREFIX, point), ECDH_P256, false, []);
}

// exportKey("raw") returns the bare point on some runtimes and SPKI on others;
// the point is always the last 65 bytes.
export async function rawPoint(publicKey) {
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", publicKey));
  return raw.slice(raw.length - 65);
}

// RFC 8291 aes128gcm: one record, rs 4096, header || AES-GCM(plaintext || 0x02).
// serverKeys and salt are injectable so a test can pin the RFC vector.
export async function encryptPush(subscription, plaintext, { serverKeys, salt } = {}) {
  serverKeys ??= await crypto.subtle.generateKey(ECDH_P256, true, ["deriveBits"]);
  salt ??= crypto.getRandomValues(new Uint8Array(16));
  const uaPoint = unb64u(subscription.p256dh);
  const auth = unb64u(subscription.auth);
  const uaKey = await importP256Point(uaPoint);
  const secret = new Uint8Array(
    await crypto.subtle.deriveBits({ name: "ECDH", public: uaKey }, serverKeys.privateKey, 256),
  );
  const asPoint = await rawPoint(serverKeys.publicKey);
  const ikm = await hkdf(auth, secret, concat(utf8("WebPush: info\0"), uaPoint, asPoint), 32);
  const cek = await hkdf(salt, ikm, utf8("Content-Encoding: aes128gcm\0"), 16);
  const nonce = await hkdf(salt, ikm, utf8("Content-Encoding: nonce\0"), 12);
  const key = await crypto.subtle.importKey("raw", cek, "AES-GCM", false, ["encrypt"]);
  const ciphertext = new Uint8Array(
    await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, key, concat(plaintext, new Uint8Array([2]))),
  );
  const recordSize = new Uint8Array(4);
  new DataView(recordSize.buffer).setUint32(0, PUSH_RECORD_SIZE);
  return concat(salt, recordSize, new Uint8Array([asPoint.length]), asPoint, ciphertext);
}

// ES256 JWS compact form; WebCrypto's ECDSA output is already r || s.
export async function signVapid(privateKey, claims) {
  const part = (value) => b64url(utf8(JSON.stringify(value)));
  const input = `${part({ typ: "JWT", alg: "ES256" })}.${part(claims)}`;
  const signature = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, utf8(input));
  return `${input}.${b64url(new Uint8Array(signature))}`;
}

export function httpsOrigin(value) {
  if (typeof value !== "string") {
    return null;
  }
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.origin === value ? url.origin : null;
  } catch {
    return null;
  }
}

const localFormats = new Map();

function localFormat(tz) {
  let format = localFormats.get(tz);
  if (!format) {
    format = new Intl.DateTimeFormat("en-US", {
      timeZone: tz, hourCycle: "h23",
      year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", second: "2-digit",
    });
    localFormats.set(tz, format);
  }
  return format;
}

function knownTimeZone(tz) {
  if (typeof tz !== "string" || !tz) {
    return false;
  }
  try {
    localFormat(tz);
    return true;
  } catch {
    return false;
  }
}

function localParts(ms, tz) {
  const parts = {};
  for (const { type, value } of localFormat(tz).formatToParts(new Date(ms))) {
    parts[type] = value;
  }
  return {
    year: Number(parts.year), month: Number(parts.month), day: Number(parts.day),
    hour: Number(parts.hour) % 24, minute: Number(parts.minute), second: Number(parts.second),
  };
}

function pad2(n) {
  return String(n).padStart(2, "0");
}

export function localDate(ms, tz) {
  const p = localParts(ms, tz);
  return `${p.year}-${pad2(p.month)}-${pad2(p.day)}`;
}

function offsetAt(ms, tz) {
  const p = localParts(ms, tz);
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - Math.floor(ms / 1000) * 1000;
}

// The instant of a wall-clock time in tz. A skipped clock (spring forward)
// resolves to the shifted time; a repeated clock to its first occurrence.
export function zonedToUTC(year, month, day, hour, minute, tz) {
  const guess = Date.UTC(year, month - 1, day, hour, minute);
  const first = guess - offsetAt(guess, tz);
  const second = guess - offsetAt(first, tz);
  for (const candidate of [first, second]) {
    const p = localParts(candidate, tz);
    if (p.day === day && p.hour === hour && p.minute === minute) {
      return candidate;
    }
  }
  return Math.max(first, second);
}

// Next instant strictly after now at which the item fires, in tz.
export function nextDue(item, tz, now) {
  if (item.when !== undefined) {
    return Date.parse(item.when);
  }
  const [hour, minute] = item.at.split(":").map(Number);
  const today = localParts(now, tz);
  for (let k = 0; k <= 7; k++) {
    const day = new Date(Date.UTC(today.year, today.month - 1, today.day + k));
    if (!item.days.includes(day.getUTCDay())) {
      continue;
    }
    const at = zonedToUTC(day.getUTCFullYear(), day.getUTCMonth() + 1, day.getUTCDate(), hour, minute, tz);
    if (at > now) {
      return at;
    }
  }
  return null;
}

function invalid(error, status = 400) {
  return { error: Response.json({ error }, { status }) };
}

function pushServiceHost(host) {
  return PUSH_SERVICE_HOSTS.some((known) => host === known || host.endsWith(`.${known}`));
}

export function validateSubscription(body) {
  if (typeof body?.endpoint !== "string" || utf8(body.endpoint).length > PUSH_MAX_ENDPOINT) {
    return invalid("invalid-endpoint");
  }
  try {
    const url = new URL(body.endpoint);
    if (url.protocol !== "https:" || !pushServiceHost(url.hostname)) {
      return invalid("invalid-endpoint");
    }
  } catch {
    return invalid("invalid-endpoint");
  }
  const p256dh = unb64u(body.keys?.p256dh);
  if (!p256dh || p256dh.length !== 65 || p256dh[0] !== 4) {
    return invalid("invalid-p256dh");
  }
  const auth = unb64u(body.keys?.auth);
  if (!auth || auth.length !== 16) {
    return invalid("invalid-auth");
  }
  return { subscription: { endpoint: body.endpoint, p256dh: b64url(p256dh), auth: b64url(auth) } };
}

function validRoomKey(key) {
  if (typeof key !== "string" || !key || utf8(key).length > PUSH_MAX_ROOM_KEY) {
    return false;
  }
  return key !== "ws" && key !== "push" && !key.startsWith("push/");
}

function optionalText(value, max) {
  return value === undefined || (typeof value === "string" && utf8(value).length <= max);
}

function validateItem(item, now) {
  if (item === null || typeof item !== "object" || Array.isArray(item)) {
    return "invalid-item";
  }
  if (typeof item.id !== "string" || !PUSH_ID_PATTERN.test(item.id)) {
    return "invalid-id";
  }
  const recurring = item.at !== undefined || item.days !== undefined;
  if (recurring === (item.when !== undefined)) {
    return "invalid-time";
  }
  if (recurring) {
    if (typeof item.at !== "string" || !PUSH_AT_PATTERN.test(item.at)) {
      return "invalid-at";
    }
    if (!Array.isArray(item.days) || item.days.length === 0 || item.days.length > 7 ||
        !item.days.every((d) => Number.isInteger(d) && d >= 0 && d <= 6) ||
        new Set(item.days).size !== item.days.length) {
      return "invalid-days";
    }
  } else if (typeof item.when !== "string" || !RFC3339_PATTERN.test(item.when) ||
             !(Date.parse(item.when) > now)) {
    return "invalid-when";
  }
  if (typeof item.title !== "string" || !item.title || utf8(item.title).length > 64) {
    return "invalid-title";
  }
  if (!optionalText(item.url, 512)) {
    return "invalid-url";
  }
  if (!optionalText(item.tag, 64)) {
    return "invalid-tag";
  }
  if ((item.body !== undefined) === (item.bodyKey !== undefined)) {
    return "invalid-body";
  }
  if (item.body !== undefined && (typeof item.body !== "string" || utf8(item.body).length > PUSH_MAX_BODY)) {
    return "invalid-body";
  }
  if (item.bodyKey !== undefined && !validRoomKey(item.bodyKey)) {
    return "invalid-body-key";
  }
  return null;
}

export function validateSchedule(body, now) {
  if (!knownTimeZone(body?.tz)) {
    return invalid("invalid-tz");
  }
  if (!Array.isArray(body.items)) {
    return invalid("invalid-items");
  }
  if (body.items.length > PUSH_MAX_ITEMS) {
    return invalid("too-many-items", 413);
  }
  const ids = new Set();
  const items = [];
  for (const item of body.items) {
    const error = validateItem(item, now);
    if (error) {
      return invalid(error);
    }
    if (ids.has(item.id)) {
      return invalid("duplicate-id");
    }
    ids.add(item.id);
    const copy = { id: item.id };
    for (const field of ["at", "days", "when", "title", "body", "bodyKey", "url", "tag"]) {
      if (item[field] !== undefined) {
        copy[field] = field === "days" ? [...item.days] : item[field];
      }
    }
    items.push(copy);
  }
  return { schedule: { tz: body.tz, items } };
}

export function publicItem(item) {
  const { due, ...rest } = item;
  return rest;
}
