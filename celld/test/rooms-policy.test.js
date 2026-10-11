import assert from "node:assert/strict";
import test from "node:test";

import { cleanPath, cleanRedirect, decodePath, escapePath } from "../src/public/path.js";
import { parseSlug, slugOfHost } from "../src/public/host.js";
import {
  byteString,
  clientAddress,
  clientSubnet,
  decodeEndpoint,
  decodeSchedule,
  decodeSubscription,
  goQuote,
  goTime,
  goURL,
  originRefusal,
  parseRFC3339,
  parseRoomID,
  pushEndpointProblem,
  pushItemWire,
  pushScheduleProblem,
  pushSubscriptionProblem,
  roomKeyProblem,
  roomsRoute,
  upgradeRefusal,
} from "../src/rooms/policy.js";
import { decodeFirstJSON } from "../src/rooms/wire.js";

const UUID = "11111111-1111-4111-8111-111111111111";

test("room ids are strict UUIDv4s, lowercased", () => {
  assert.equal(parseRoomID(UUID), UUID);
  assert.equal(parseRoomID("ABCDEF01-2345-4678-9ABC-DEF012345678"), "abcdef01-2345-4678-9abc-def012345678");
  for (const bad of [
    "", "not-a-uuid", `${UUID}0`, UUID.slice(1),
    "11111111-1111-3111-8111-111111111111", // version 3
    "11111111-1111-4111-c111-111111111111", // variant 110x
    "11111111x1111-4111-8111-111111111111",
    "1111111g-1111-4111-8111-111111111111",
  ]) {
    assert.equal(parseRoomID(bad), null, bad);
  }
});

test("slugs come from one label of the apex in Go's alphabet", () => {
  assert.equal(parseSlug("abcdefgh"), "abcdefgh");
  for (const bad of ["abcdefg", "abcdefghi", "abc12345", "ABCDEFGH", "abcdefgl", "abcdefgo"]) {
    assert.equal(parseSlug(bad), null, bad);
  }
  assert.equal(slugOfHost("abcdefgh.example.com", "example.com"), "abcdefgh");
  assert.equal(slugOfHost("x.abcdefgh.example.com", "example.com"), null);
  assert.equal(slugOfHost("example.com", "example.com"), null);
  assert.equal(slugOfHost("abcdefgh.example.org", "example.com"), null);
});

test("rooms routes split as hostthisd's handler does", () => {
  const cases = [
    ["/api/rooms", { kind: "collection" }],
    ["/api/rooms/", { kind: "collection" }],
    ["/api/push/key", { kind: "pushkey" }],
    [`/api/rooms/${UUID}`, { kind: "scan", id: UUID }],
    [`/api/rooms/${UUID}/`, { kind: "scan", id: UUID }],
    [`/api/rooms/${UUID.toUpperCase()}/k`, { kind: "value", id: UUID, key: "k" }],
    [`/api/rooms/${UUID}/a/b/c`, { kind: "value", id: UUID, key: "a/b/c" }],
    [`/api/rooms/${UUID}/ws`, { kind: "ws", id: UUID }],
    [`/api/rooms/${UUID}/ws/x`, { kind: "value", id: UUID, key: "ws/x" }],
    [`/api/rooms/${UUID}/WS`, { kind: "value", id: UUID, key: "WS" }],
    [`/api/rooms/${UUID}/push`, { kind: "push", id: UUID, sub: "" }],
    [`/api/rooms/${UUID}/push/test`, { kind: "push", id: UUID, sub: "/test" }],
    [`/api/rooms/${UUID}/push/`, { kind: "push", id: UUID, sub: "/" }],
    [`/api/rooms/${UUID}/push:2026-09-06`, { kind: "value", id: UUID, key: "push:2026-09-06" }],
    [`/api/rooms/${UUID}/pushy`, { kind: "value", id: UUID, key: "pushy" }],
    ["/api/rooms/nope", { kind: "malformed" }],
    ["/api/rooms/nope/key", { kind: "malformed" }],
    ["/api/roomsX", null],
    ["/api/room", null],
    ["/api/push/key/", null],
    ["/", null],
  ];
  for (const [path, want] of cases) {
    assert.deepEqual(roomsRoute(path), want, path);
  }
});

test("keys are 1 to 256 bytes and never a reserved segment", () => {
  assert.equal(roomKeyProblem("k"), null);
  assert.equal(roomKeyProblem("x".repeat(256)), null);
  assert.equal(roomKeyProblem("x".repeat(257)), "room key is too long");
  // Bytes, not characters: 86 three-byte characters are 258 bytes.
  assert.equal(roomKeyProblem(byteString("€".repeat(86))), "room key is too long");
  assert.equal(roomKeyProblem(""), "room key is empty");
  for (const key of ["ws", "push", "push/x"]) {
    assert.equal(roomKeyProblem(key), "room key is reserved", key);
  }
  for (const key of ["push:x", "pushy", "wss", "x/push"]) {
    assert.equal(roomKeyProblem(key), null, key);
  }
});

// Expected values are hostthisd's ipSubnet(net.ParseIP(address)).
test("client subnets match hostthisd's", () => {
  const cases = [
    ["203.0.113.7", "203.0.113.0/24"],
    ["203.0.113.255", "203.0.113.0/24"],
    ["0.0.0.0", "0.0.0.0/24"],
    ["255.255.255.255", "255.255.255.0/24"],
    ["01.2.3.4", "unknown"],
    ["1.2.3", "unknown"],
    ["1.2.3.4.5", "unknown"],
    ["256.1.1.1", "unknown"],
    ["::ffff:198.51.100.9", "198.51.100.0/24"],
    ["::ffff:c633:6409", "198.51.100.0/24"],
    ["2001:db8:1234:5678::1", "2001:db8:1234::/48"],
    ["2001:DB8:0:0:1::1", "2001:db8::/48"],
    ["::1", "::/48"],
    ["::", "::/48"],
    ["fe80::1%eth0", "unknown"],
    ["2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::/48"],
    ["1:2:3:4:5:6:7::", "1:2:3::/48"],
    ["::2:3:4:5:6:7:8", "0:2:3::/48"],
    ["1:2:3:4:5:6:7:8", "1:2:3::/48"],
    ["1:2:3:4:5:6:7:8:9", "unknown"],
    ["1::2::3", "unknown"],
    ["2001:db8::", "2001:db8::/48"],
    ["0:0:1::", "0:0:1::/48"],
    ["::1.2.3.4", "::/48"],
    ["1:2:3:4:5:6:1.2.3.4", "1:2:3::/48"],
    ["1:2:3:4:5:6:7:1.2.3.4", "unknown"],
    [" 1.2.3.4", "unknown"],
    ["", "unknown"],
    ["x", "unknown"],
    ["12345::1", "unknown"],
    ["::ffff:1.2.3.4%x", "unknown"],
    ["64:ff9b::1.2.3.4", "64:ff9b::/48"],
    ["1:0:0:2::", "1::/48"],
    ["0:0:0:1::", "::/48"],
    ["a:b:c::", "a:b:c::/48"],
    ["a:0:0::", "a::/48"],
    ["0:b:0::", "0:b::/48"],
  ];
  for (const [address, want] of cases) {
    assert.equal(clientSubnet(address), want, address);
  }
  assert.equal(clientSubnet(null), "unknown");
});

test("the client address comes from the configured source only", () => {
  const headers = new Headers({
    "CF-Connecting-IP": "198.51.100.1",
    "X-Forwarded-For": "10.0.0.1, 203.0.113.9 ",
    "X-Hostthis-Client-IP": "192.0.2.4",
  });
  assert.equal(clientAddress(headers, "cf-connecting-ip", false), "198.51.100.1");
  assert.equal(clientAddress(headers, "xff-rightmost", false), "203.0.113.9");
  assert.equal(clientAddress(headers, undefined, false), null);
  assert.equal(clientAddress(headers, "remote-addr", false), null);
  // Forwarded requests carry the address hostthisd saw, whatever the source.
  assert.equal(clientAddress(headers, "cf-connecting-ip", true), "192.0.2.4");
  assert.equal(clientAddress(new Headers(), "xff-rightmost", false), null);
});

test("url hosts parse as url.Parse reads them", () => {
  const cases = [
    ["https://abc.example.com", "abc.example.com"],
    ["https://ABC.example.com:8443", "ABC.example.com:8443"],
    ["null", ""],
    ["https://user@abc.example.com/x?y", "abc.example.com"],
    ["https://abc.example.com:x", null],
    ["file://", ""],
    ["https://[::1]:80", "[::1]:80"],
    ["abc.example.com", ""],
    ["https://a b.example.com", null],
    ["https://abc.example.com:", "abc.example.com:"],
    ["//abc.example.com", "abc.example.com"],
    ["https://%41bc.example.com", null],
    ["https://a\nb.example.com", null],
  ];
  for (const [raw, want] of cases) {
    assert.equal(goURL(raw)?.host ?? null, want, raw);
  }
  assert.equal(goURL("HTTPS://x.example.com").scheme, "https");
  assert.equal(goURL("https://u@x.example.com").user, true);
});

test("origins are allowed as hostthisd's websocket.Accept allows them", () => {
  const apex = "example.com";
  const host = "abcdefgh.example.com";
  for (const origin of [
    null, "", "https://abcdefgh.example.com", "https://ABCDEFGH.example.com", "https://other123.example.com",
    "https://example.com", "http://a.b.example.com", "https://.example.com",
  ]) {
    assert.equal(originRefusal(origin, host, apex), null, String(origin));
  }
  for (const origin of [
    "https://evil.com", "https://example.com.evil.com", "https://abcdefgh.example.com:8443", "null",
    "https://xexample.com", "https://abc.example.com:x",
  ]) {
    assert.match(originRefusal(origin, host, apex), /Origin/, origin);
  }
  // The request's own host, port included, is always allowed.
  assert.equal(originRefusal("http://localhost:8787", "localhost:8787", apex), null);
  assert.equal(originRefusal("http://localhost:8787", "abcdefgh.localhost:8787", "localhost:8787"), null);
  assert.equal(
    originRefusal("https://evil.com", host, apex),
    'request Origin "evil.com" is not authorized for Host "abcdefgh.example.com"',
  );
  assert.equal(originRefusal("null", host, apex), 'request Origin "null" is not a valid URL with a host');
});

test("only a GET carrying the websocket upgrade token is passed on", () => {
  const h = (headers) => new Headers(headers);
  assert.equal(upgradeRefusal("GET", h({ Upgrade: "websocket" })), null);
  assert.equal(upgradeRefusal("GET", h({ Upgrade: "h2c, WebSocket" })), null);
  const missing = upgradeRefusal("GET", h({}));
  assert.equal(missing.status, 426);
  assert.deepEqual(missing.headers, { Connection: "Upgrade", Upgrade: "websocket" });
  assert.equal(upgradeRefusal("GET", h({ Upgrade: "websockets" })).status, 426);
  assert.equal(upgradeRefusal("POST", h({ Upgrade: "websocket" })).status, 405);
  assert.equal(upgradeRefusal("HEAD", h({ Upgrade: "websocket" })).status, 405);
});

function body(text) {
  return decodeFirstJSON(new TextEncoder().encode(text));
}

test("push bodies decode as encoding/json decodes them", () => {
  assert.deepEqual(decodeSubscription(body('{"endpoint":"x"} garbage')), { endpoint: "x", keys: { p256dh: "", auth: "" } });
  assert.deepEqual(decodeSubscription(body('{"ENDPOINT":"x","Keys":{"P256DH":"a"}}')), {
    endpoint: "x", keys: { p256dh: "a", auth: "" },
  });
  assert.equal(decodeSubscription(body('{"endpoint":"x","ENDPOINT":"y"}')).endpoint, "y");
  assert.equal(decodeSubscription(body('{"ENDPOINT":"y","endpoint":"x"}')).endpoint, "x");
  assert.equal(decodeSubscription(body('{"endpoint":"a","Endpoint":"b","ENDPOINT":"c"}')).endpoint, "c");
  // A nested document named twice merges into one.
  assert.deepEqual(decodeSubscription(body('{"Keys":{"auth":"q"},"keys":{"p256dh":"r"}}')).keys, { p256dh: "r", auth: "q" });
  // The Kelvin sign and the long s fold to k and s.
  assert.equal(decodeSubscription(body('{"Keys":{"auth":"q"}}')).keys.auth, "q");
  assert.equal(decodeSchedule(body('{"tz":"UTC","itemſ":[{"id":"a"}]}')).items[0].id, "a");
  assert.deepEqual(decodeSubscription(body('{"endpoint":null,"keys":null}')), { endpoint: "", keys: { p256dh: "", auth: "" } });
  assert.deepEqual(decodeSubscription(body("null")), { endpoint: "", keys: { p256dh: "", auth: "" } });
  for (const text of [
    "", "   ", "[1]", '"str"', '{"endpoint":5}', '{"endpoint":true}', '{"keys":{"p256dh":1}}', '{"keys":[]}',
    '{"keys":"x","endpoint":"e"}', '{"endpoint":"x"', `${"[".repeat(10001)}${"]".repeat(10001)}`,
  ]) {
    assert.equal(decodeSubscription(body(text)), null, text.slice(0, 40));
  }
  assert.equal(decodeSubscription(body('{"endpoint":"\\ud800x"}')).endpoint, "�x");
  assert.equal(decodeEndpoint(body('{"endpoint":"e","other":[1,{"x":2}]}')).endpoint, "e");
});

test("schedule bodies decode weekdays only from plain integers", () => {
  const days = (text) => decodeSchedule(body(`{"tz":"UTC","items":[{"id":"a","days":${text}}]}`))?.items[0].days ?? null;
  assert.deepEqual(days("[1,2]"), [1, 2]);
  assert.deepEqual(days("[null,2]"), [0, 2]);
  assert.deepEqual(days("[-0]"), [0]);
  assert.equal(days("null"), null);
  for (const bad of ["[1.0]", "[1e0]", "[99999999999999999999]", "[true]", '[1,"x",3]', '"x"']) {
    assert.equal(decodeSchedule(body(`{"tz":"UTC","items":[{"id":"a","days":${bad}}]}`)), null, bad);
  }
  assert.equal(decodeSchedule(body('{"tz":"UTC","items":null}')).items, null);
  assert.equal(decodeSchedule(body('{"tz":"UTC","items":{}}')), null);
  assert.deepEqual(decodeSchedule(body('{"tz":"UTC","items":[null]}')).items[0].id, "");
  assert.equal(decodeSchedule(body('{"tz":"UTC","items":[{"id":"a","bodykey":"k","BODYKEY":"j"}]}')).items[0].bodyKey, "j");
  assert.equal(decodeSchedule(body('{"tz":"UTC" "x":1}')), null);
  assert.equal(decodeSchedule(body('{"tz":"UTC"}{"tz":"x"}')).tz, "UTC");
});

const P256 = `B${"A".repeat(86)}`;
const AUTH = "A".repeat(22);
const ENDPOINT = "https://fcm.googleapis.com/fcm/send/abc";

test("subscriptions are validated as domain.ValidatePushSubscription does", () => {
  const sub = (fields) => ({ endpoint: ENDPOINT, keys: { p256dh: P256, auth: AUTH }, ...fields });
  assert.equal(pushSubscriptionProblem(sub({})), null);
  assert.equal(pushSubscriptionProblem(sub({ endpoint: "https://web.push.apple.com/x" })), null);
  assert.equal(pushSubscriptionProblem(sub({ keys: { p256dh: `${P256}=`, auth: `${AUTH}==` } })), null);
  const cases = [
    [{ endpoint: "" }, "push: invalid: endpoint is required"],
    [{ endpoint: `${ENDPOINT}/${"x".repeat(1024)}` }, "push: invalid: endpoint exceeds 1024 bytes"],
    [{ endpoint: "http://fcm.googleapis.com/x" }, "push: invalid: endpoint must be an https URL"],
    [{ endpoint: "https:///x" }, "push: invalid: endpoint must be an https URL"],
    [{ endpoint: "https://evil.com/x" }, "push: invalid: endpoint host is not a known push service"],
    [{ endpoint: "https://u@fcm.googleapis.com/x" }, "push: invalid: endpoint host is not a known push service"],
    [{ endpoint: "https://xfcm.googleapis.com/x" }, "push: invalid: endpoint host is not a known push service"],
    [{ keys: { p256dh: `C${"A".repeat(86)}`, auth: AUTH } }, "push: invalid: keys.p256dh must be a base64url uncompressed P-256 point"],
    [{ keys: { p256dh: "", auth: AUTH } }, "push: invalid: keys.p256dh must be a base64url uncompressed P-256 point"],
    [{ keys: { p256dh: P256, auth: "A".repeat(21) } }, "push: invalid: keys.auth must be 16 base64url bytes"],
    [{ keys: { p256dh: P256, auth: `${"A".repeat(21)}+` } }, "push: invalid: keys.auth must be 16 base64url bytes"],
  ];
  for (const [fields, want] of cases) {
    assert.equal(pushSubscriptionProblem(sub(fields)), want, JSON.stringify(fields).slice(0, 60));
  }
  assert.equal(pushEndpointProblem(""), "push: invalid");
  assert.equal(pushEndpointProblem("x".repeat(1025)), "push: invalid");
  assert.equal(pushEndpointProblem("x"), null);
});

// Expected values are time.Parse(time.RFC3339, text) in milliseconds.
test("one-shot times parse as Go's RFC 3339 parser reads them", () => {
  const cases = [
    ["2026-09-12T08:00:00-04:00", 1789214400000],
    ["2026-09-12T08:00:00Z", 1789200000000],
    ["2026-09-12t08:00:00Z", null],
    ["2026-09-12T08:00:00,5Z", 1789200000500],
    ["2026-09-12T08:00:00.123456789123Z", 1789200000123],
    ["2026-02-30T08:00:00Z", null],
    ["2026-09-12T24:00:00Z", null],
    ["2026-09-12T08:00:60Z", null],
    ["2026-09-12T08:00:00+24:00", 1789113600000],
    ["2026-09-12T08:00:00+23:59", 1789113660000],
    ["2026-09-12T08:00:00+23:60", 1789113600000],
    ["2026-09-12T08:00Z", null],
    ["2026-09-12T08:00:00", null],
    ["2026-09-12T08:00:00z", null],
    ["2026-9-12T08:00:00Z", null],
    ["2024-02-29T00:00:00Z", 1709164800000],
    ["2026-09-12T08:00:00.Z", null],
    ["2026-13-12T08:00:00Z", null],
    ["0000-01-01T00:00:00Z", -62167219200000],
  ];
  for (const [text, want] of cases) {
    assert.equal(parseRFC3339(text), want, text);
  }
});

test("schedules are validated as domain.ValidatePushSchedule does", () => {
  const now = Date.parse("2026-09-01T00:00:00Z");
  const item = (fields) => ({
    id: "a", at: "08:00", days: [1], when: "", title: "T", body: "b", bodyKey: "", url: "", tag: "", ...fields,
  });
  const schedule = (items, tz = "America/New_York") => ({ tz, items });
  assert.equal(pushScheduleProblem(schedule([item({})]), now), null);
  assert.equal(pushScheduleProblem(schedule(null), now), null);
  assert.equal(pushScheduleProblem(schedule([item({ at: "", days: null, when: "2026-09-12T08:00:00Z" })]), now), null);
  assert.equal(pushScheduleProblem(schedule([item({ body: "", bodyKey: "push:{date}" })]), now), null);
  const cases = [
    [schedule(Array.from({ length: 17 }, (_, n) => item({ id: `i${n}` }))), "push: invalid: more than 16 items"],
    [schedule([], ""), "push: invalid: tz is required"],
    [schedule([], "Local"), 'push: invalid: unknown tz "Local"'],
    [schedule([], "Mars/Olympus"), 'push: invalid: unknown tz "Mars/Olympus"'],
    [schedule([item({ id: "" })]), "push: invalid: id must be 1 to 32 characters (item 0)"],
    [schedule([item({ id: "x".repeat(33) })]), "push: invalid: id must be 1 to 32 characters (item 0)"],
    [schedule([item({ id: "a b" })]), 'push: invalid: id "a b" has a character outside [A-Za-z0-9_-] (item 0)'],
    [schedule([item({ title: "" })]), "push: invalid: title must be 1 to 64 bytes (item 0)"],
    [schedule([item({ title: "€".repeat(22) })]), "push: invalid: title must be 1 to 64 bytes (item 0)"],
    [schedule([item({ url: "x".repeat(513) })]), "push: invalid: url exceeds 512 bytes (item 0)"],
    [schedule([item({ tag: "x".repeat(65) })]), "push: invalid: tag exceeds 64 bytes (item 0)"],
    [schedule([item({ bodyKey: "k" })]), "push: invalid: exactly one of body and bodyKey is required (item 0)"],
    [schedule([item({ body: "" })]), "push: invalid: exactly one of body and bodyKey is required (item 0)"],
    [schedule([item({ body: "x".repeat(1025) })]), "push: invalid: body exceeds 1024 bytes (item 0)"],
    [schedule([item({ body: "", bodyKey: "push/{date}" })]), "push: invalid: bodyKey: room key is reserved (item 0)"],
    [schedule([item({ body: "", bodyKey: `${"x".repeat(251)}{date}` })]), "push: invalid: bodyKey: room key is too long (item 0)"],
    [schedule([item({ when: "2026-09-12T08:00:00Z" })]), "push: invalid: when excludes at and days (item 0)"],
    [schedule([item({ at: "", days: null, when: "2026-09-12" })]), "push: invalid: when must be RFC 3339 with an offset (item 0)"],
    [schedule([item({ at: "", days: null, when: "2026-08-12T08:00:00Z" })]), "push: invalid: when is not in the future (item 0)"],
    [schedule([item({ at: "24:00" })]), "push: invalid: at must be HH:MM (item 0)"],
    [schedule([item({ at: "8:00" })]), "push: invalid: at must be HH:MM (item 0)"],
    [schedule([item({ days: [] })]), "push: invalid: days must name at least one weekday (item 0)"],
    [schedule([item({ days: [7] })]), "push: invalid: day 7 is outside 0..6 (item 0)"],
    [schedule([item({ days: [1, 1] })]), "push: invalid: day 1 repeats (item 0)"],
    [schedule([item({}), item({ title: "" })]), "push: invalid: title must be 1 to 64 bytes (item 1)"],
    [schedule([item({}), item({})]), 'push: invalid: duplicate id "a"'],
  ];
  for (const [doc, want] of cases) {
    assert.equal(pushScheduleProblem(doc, now), want, want);
  }
});

test("push items encode as Go marshals domain.PushItem", () => {
  assert.deepEqual(
    Object.entries(pushItemWire({ tag: "t", url: "", bodyKey: "k", body: "", title: "T", when: "", days: [1], at: "08:00", id: "a" })),
    [["id", "a"], ["at", "08:00"], ["days", [1]], ["title", "T"], ["bodyKey", "k"], ["tag", "t"]],
  );
  assert.deepEqual(pushItemWire({ id: "a", days: [], title: "T", body: "b" }), { id: "a", title: "T", body: "b" });
});

test("times and quotes format as Go's do", () => {
  assert.equal(goTime("2026-10-11T12:34:56.789Z"), "2026-10-11T12:34:56.789Z");
  assert.equal(goTime("2026-10-11T12:34:56.780Z"), "2026-10-11T12:34:56.78Z");
  assert.equal(goTime("2026-10-11T12:34:56.000Z"), "2026-10-11T12:34:56Z");
  assert.equal(goTime(Date.parse("2026-10-11T12:34:56.5Z")), "2026-10-11T12:34:56.5Z");
  assert.equal(goQuote('a"b\\c\n\x01\x7fé'), '"a\\"b\\\\c\\n\\x01\\x7fé"');
});

test("paths clean, decode and escape as Go's server handles them", () => {
  const clean = [
    ["/api/rooms//x", "/api/rooms/x"],
    ["/api/rooms/x/", "/api/rooms/x/"],
    ["/a/./b", "/a/b"],
    ["/a/../b", "/b"],
    ["/..", "/"],
    ["//", "/"],
    ["", "/"],
    ["/", "/"],
  ];
  for (const [path, want] of clean) {
    assert.equal(cleanPath(path), want, path);
  }
  assert.equal(decodePath("/a%2Fb%7C%ff"), "/a/b|\xff");
  assert.equal(decodePath("/a+b"), "/a+b");
  assert.equal(decodePath("/a%zz"), null);
  assert.equal(decodePath("/a%2"), null);
  assert.equal(escapePath("/a b|\xff$&+,:;=@~"), "/a%20b%7C%FF$&+,:;=@~");
  assert.equal(escapePath("/a?b#"), "/a%3Fb%23");
});

test("an unclean path redirects 307 as ServeMux redirects it", async () => {
  assert.equal(cleanRedirect("GET", "/api/rooms/x", ""), null);
  const get = cleanRedirect("GET", "/api/rooms//%7C/y", "?q=1&r");
  assert.equal(get.status, 307);
  assert.equal(get.headers.get("Location"), "/api/rooms/%7C/y?q=1&r");
  assert.equal(get.headers.get("Content-Type"), "text/html; charset=utf-8");
  assert.equal(await get.text(), '<a href="/api/rooms/%7C/y?q=1&amp;r">Temporary Redirect</a>.\n\n');
  const put = cleanRedirect("PUT", "//x", "");
  assert.equal(put.status, 307);
  assert.equal(put.headers.get("Location"), "/x");
  assert.equal(put.headers.get("Content-Type"), null);
});
