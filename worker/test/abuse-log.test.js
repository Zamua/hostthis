import assert from "node:assert/strict";
import test from "node:test";

import { abuseEventOf, abuseLog, logSubnet } from "../src/abuse-log.js";
import { formatIPv6, parseIP } from "../src/rooms/policy.js";

function logged(run) {
  const lines = [];
  const original = console.log;
  console.log = (...args) => lines.push(args.join(" "));
  try {
    run();
  } finally {
    console.log = original;
  }
  return lines;
}

// mulberry32: a fixed seed keeps a failure reproducible.
function prng(seed) {
  let a = seed;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const KEYS = ["event", "route", "status", "subnet", "country", "asn"];

test("an abuse line carries the six fields and never the input address or its host bits", () => {
  const rand = prng(20261103);
  const int = (n) => Math.floor(rand() * n);
  for (let i = 0; i < 600; i++) {
    let inputs;
    let prefix;
    if (i % 2 === 0) {
      const octets = [1 + int(255), int(256), int(256), 1 + int(255)];
      inputs = [octets.join(".")];
      prefix = 24;
    } else {
      // Host bits (past the 40th) are nonzero; runs of zero groups exercise "::".
      const groups = Array.from({ length: 8 }, () => (rand() < 0.3 ? 0 : int(0x10000)));
      groups[2] |= 1;
      groups[3] = 1 + int(0xffff);
      groups[7] = 1 + int(0xffff);
      inputs = [groups.map((g) => g.toString(16)).join(":"), formatIPv6(groups), formatIPv6(groups).toUpperCase()];
      prefix = 40;
    }
    for (const ip of inputs) {
      const [line] = logged(() => abuseLog("rate-limited", { route: "collection", status: 429, ip, country: "US", asn: 64500 }));
      assert.deepEqual(Object.keys(JSON.parse(line)), KEYS, `seed run ${i}: ${ip}`);
      assert.ok(!line.includes(ip), `the line contains the input ${ip}: ${line}`);
      const [subnet, length] = JSON.parse(line).subnet.split("/");
      assert.equal(Number(length), prefix, `${ip}: ${line}`);
      // Parsed back, the logged address is the input's first `prefix` bits and zeros after.
      const want = parseIP(ip);
      const got = parseIP(subnet);
      assert.ok(want && got, `${ip}: ${line}`);
      const bits = (groups) => groups.map((g) => g.toString(2).padStart(16, "0")).join("");
      const wantBits = bits(want);
      const gotBits = bits(got);
      const offset = prefix === 24 ? 96 : 0;
      assert.equal(gotBits.slice(0, offset + prefix), wantBits.slice(0, offset + prefix), `${ip}: ${line}`);
      assert.match(gotBits.slice(offset + prefix), /^0*$/, `host bits survive for ${ip}: ${line}`);
    }
  }
});

test("a v4 address logs its /24, an IPv6 address its /40", () => {
  assert.equal(logSubnet("203.0.113.77"), "203.0.113.0/24");
  assert.equal(logSubnet("2001:db8:abcd:1234::1"), "2001:db8:ab00::/40");
  assert.equal(logSubnet("2001:0db8:ab12:ffff:ffff:ffff:ffff:ffff"), "2001:db8:ab00::/40");
  assert.equal(logSubnet(" 198.51.100.9 "), "198.51.100.0/24");
});

test("an IPv4-mapped IPv6 address counts as IPv4", () => {
  assert.equal(logSubnet("::ffff:203.0.113.77"), "203.0.113.0/24");
  assert.equal(logSubnet("::ffff:cb00:714d"), "203.0.113.0/24");
});

test("an unparseable address logs unknown", () => {
  for (const ip of ["", "garbage", "1.2.3", "256.1.1.1", "1.2.3.4.5", "fe80::1%eth0", "2001:db8::g", null, undefined, 5, {}]) {
    assert.equal(logSubnet(ip), "unknown", String(ip));
  }
  const [line] = logged(() => abuseLog("created", { route: "collection", status: 201, ip: "garbage" }));
  assert.equal(JSON.parse(line).subnet, "unknown");
});

test("only the named fields are logged, however many a caller passes", () => {
  assert.equal(abuseLog.length, 2);
  const [line] = logged(() => abuseLog("oversize", {
    route: "value",
    status: 413,
    ip: "203.0.113.77",
    headers: new Headers({ cookie: "secret-cookie", "user-agent": "secret-agent" }),
    body: "secret-body",
    url: "https://x.example/secret-path",
    userAgent: "secret-agent",
  }));
  assert.deepEqual(Object.keys(JSON.parse(line)), KEYS);
  assert.doesNotMatch(line, /secret/);
});

test("country and asn are logged only when well formed", () => {
  const read = (fields) => JSON.parse(logged(() => abuseLog("capacity", { route: "collection", status: 507, ip: "192.0.2.1", ...fields }))[0]);
  assert.deepEqual([read({ country: "us", asn: 64500 }).country, read({ country: "us", asn: 64500 }).asn], ["US", 64500]);
  for (const country of ["<script>", "USA", "", 7, null, undefined]) {
    assert.equal(read({ country }).country, null, String(country));
  }
  for (const asn of ["AS1", -1, 0, 1.5, null, undefined, Number.MAX_VALUE]) {
    assert.equal(read({ asn }).asn, null, String(asn));
  }
});

test("only abuse statuses are events, and a created room is one only on the collection", () => {
  assert.deepEqual(
    [201, 401, 403, 413, 429, 507].map((status) => abuseEventOf("collection", status)),
    ["created", "auth-failure", "auth-failure", "oversize", "rate-limited", "capacity"],
  );
  assert.equal(abuseEventOf("value", 201), null);
  for (const status of [200, 101, 204, 304, 400, 404, 405, 500]) {
    assert.equal(abuseEventOf("collection", status), null, String(status));
  }
});
