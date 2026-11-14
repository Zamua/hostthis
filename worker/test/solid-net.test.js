import assert from "node:assert/strict";
import test from "node:test";

import { allowedPodUrl, podNetFetch, readTextCapped, RefusedAddress } from "../src/solid/net.js";

const ENV = { HOSTTHIS_APEX_DOMAIN: "example.com" };

test("a profile may name only public https addresses", () => {
  const allowed = ["https://pod.example.org/alice/", "https://id.provider.net/.well-known/openid-configuration"];
  const refused = [
    "http://pod.example.org/",
    "https://10.0.0.3/",
    "https://169.254.169.254/latest/",
    "https://[::1]/",
    "https://0x7f000001/",
    "https://2130706433/",
    "https://localhost/",
    "https://kube-apiserver/",
    "https://db.internal/",
    "https://printer.local/",
    "https://example.com/",
    "https://abc.example.com/",
    "https://hostthis.someone.workers.dev/",
    "https://user:pw@pod.example.org/",
    "file:///etc/passwd",
    "not a url",
  ];
  for (const url of allowed) assert.equal(allowedPodUrl(url, ENV), true, url);
  for (const url of refused) assert.equal(allowedPodUrl(url, ENV), false, url);
});

test("loopback is reachable only from a local runtime", () => {
  const local = { ...ENV, HOSTTHIS_SOLID_ALLOW_LOOPBACK: "1" };
  assert.equal(allowedPodUrl("http://localhost:8080/alice/", ENV), false);
  assert.equal(allowedPodUrl("http://localhost:8080/alice/", local), true);
  assert.equal(allowedPodUrl("http://pod.localhost/", local), true);
  assert.equal(allowedPodUrl("http://127.0.0.1/", local), false);
});

async function withFetch(answer, fn) {
  const real = globalThis.fetch;
  const seen = [];
  globalThis.fetch = async (url, init) => {
    seen.push([url, init.redirect]);
    return answer(url);
  };
  try {
    await fn(seen);
  } finally {
    globalThis.fetch = real;
  }
}

test("a disallowed address is refused before any request", async () => {
  await withFetch(() => new Response("x"), async (seen) => {
    await assert.rejects(podNetFetch(ENV, "https://10.0.0.3/card"), RefusedAddress);
    assert.equal(seen.length, 0);
  });
});

test("a redirect is followed only to an allowed address", async () => {
  await withFetch((url) => (url.startsWith("https://a.example.org")
    ? new Response(null, { status: 302, headers: { Location: "https://b.example.org/card" } })
    : new Response("ok")), async (seen) => {
    const res = await podNetFetch(ENV, "https://a.example.org/card");
    assert.equal(await res.text(), "ok");
    assert.deepEqual(seen.map(([, mode]) => mode), ["manual", "manual"]);
  });
  await withFetch(() => new Response(null, { status: 302, headers: { Location: "http://10.0.0.3/" } }), async () => {
    await assert.rejects(podNetFetch(ENV, "https://a.example.org/card"), RefusedAddress);
  });
});

test("a POST does not follow a redirect", async () => {
  await withFetch(() => new Response(null, { status: 307, headers: { Location: "https://b.example.org/" } }), async (seen) => {
    const res = await podNetFetch(ENV, "https://a.example.org/token", { method: "POST" });
    assert.equal(res.status, 307);
    assert.equal(seen.length, 1);
  });
});

test("a body over the cap is refused", async () => {
  assert.equal(await readTextCapped(new Response("abc"), 3), "abc");
  await assert.rejects(readTextCapped(new Response("abcd"), 3), /larger than 3 bytes/);
});
