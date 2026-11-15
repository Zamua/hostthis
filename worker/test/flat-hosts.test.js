import assert from "node:assert/strict";
import test from "node:test";

import worker from "../dist/index.js";
import { publicHost, slugHostSuffix, slugOfHost } from "../src/public/host.js";
import { originRefusal } from "../src/rooms/policy.js";
import { allowedPodUrl } from "../src/solid/net.js";

// A deployment whose apps live at "<slug>-staging.example.com" beside an apex
// of "staging.example.com", as a free certificate covers one level only.
const FLAT = { HOSTTHIS_APEX_DOMAIN: "staging.example.com", HOSTTHIS_SLUG_HOST_SUFFIX: "-staging.example.com" };

test("the slug host suffix defaults to a subdomain of the apex", () => {
  assert.equal(slugHostSuffix({ HOSTTHIS_APEX_DOMAIN: "Example.com." }), ".example.com");
  assert.equal(slugHostSuffix(FLAT), "-staging.example.com");
  assert.equal(slugHostSuffix({}), "");
});

test("a flat suffix names an app by the prefix before it", () => {
  const suffix = slugHostSuffix(FLAT);
  assert.equal(slugOfHost("abcdefgh-staging.example.com", suffix), "abcdefgh");
  for (const host of [
    "abcdefgh.example.com", "abcdefgh.staging.example.com", "x.abcdefgh-staging.example.com",
    "abcdefg-staging.example.com", "-staging.example.com", "abcdefgh-staging.example.org",
  ]) {
    assert.equal(slugOfHost(host, suffix), null, host);
  }
  assert.ok(publicHost("abcdefgh-staging.example.com", "staging.example.com", suffix));
  assert.ok(publicHost("staging.example.com", "staging.example.com", suffix));
  assert.ok(!publicHost("abcdefgh.example.com", "staging.example.com", suffix));
});

// Namespaces that record the names they are asked for; every cell answers 404.
function env(vars, asked) {
  const cell = { fetch: async () => new Response(null, { status: 404 }) };
  const ns = (binding) => ({ idFromName: (name) => (asked.push(`${binding}:${name}`), { name }), get: () => cell });
  const out = { ...vars, PAYLOADS: { get: async () => null } };
  for (const binding of ["IDENTITY", "PASTES", "ROOMS", "SUBNETS", "INTENT_LOGS"]) out[binding] = ns(binding);
  return out;
}

test("an app host under a flat suffix reaches the public router", async () => {
  const asked = [];
  const read = await worker.fetch(new Request("https://abcdefgh-staging.example.com/"), env(FLAT, asked));
  assert.notEqual(read.status, 503);
  assert.ok(asked.some((name) => name.startsWith("PASTES:") && name.includes("abcdefgh")), asked.join(" "));
  // The nested shape is no app's host when the suffix is flat.
  const nested = await worker.fetch(new Request("https://abcdefgh.staging.example.com/"), env(FLAT, []));
  assert.equal(nested.status, 404);
  // Outside the apex and the suffix, the internal router answers.
  const other = await worker.fetch(new Request("https://abcdefgh.example.com/"), env(FLAT, []));
  assert.equal(other.status, 503);
});

test("an app host under a flat suffix may open a room socket", () => {
  const host = "abcdefgh-staging.example.com";
  for (const origin of ["https://abcdefgh-staging.example.com", "https://other234-staging.example.com", "https://staging.example.com"]) {
    assert.equal(originRefusal(origin, host, "staging.example.com", "-staging.example.com"), null, origin);
  }
  assert.match(originRefusal("https://evil-staging.example.org", host, "staging.example.com", "-staging.example.com"), /Origin/);
});

test("a pod address may not be an app host under a flat suffix", () => {
  assert.equal(allowedPodUrl("https://abcdefgh-staging.example.com/card", FLAT), false);
  assert.equal(allowedPodUrl("https://pod.example.org/card", FLAT), true);
});
