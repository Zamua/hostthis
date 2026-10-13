// The warning is once per isolate, so this file has the process to itself.
import assert from "node:assert/strict";
import test from "node:test";

import { APEX, at, FORWARDED, SLUG, TOKEN, world } from "./public-world.js";

async function warnings(run) {
  const warned = [];
  const original = console.warn;
  console.warn = (...args) => warned.push(args.join(" "));
  try {
    await run();
  } finally {
    console.warn = original;
  }
  return warned;
}

test("direct public requests without a client IP source warn once per isolate", async () => {
  const quiet = await warnings(async () => {
    for (const source of ["cf-connecting-ip", "xff-rightmost"]) {
      await (await world({ HOSTTHIS_CLIENT_IP_SOURCE: source }).fetch(at("/"))).arrayBuffer();
    }
    // A forwarded request's address is the forwarding header's.
    const forwarded = world({ HOSTTHIS_CLIENT_IP_SOURCE: undefined });
    await (await forwarded.fetch("http://worker.internal/api/rooms", {
      method: "POST",
      headers: { Authorization: `Bearer ${TOKEN}`, [FORWARDED]: `${SLUG}.${APEX}` },
    })).arrayBuffer();
  });
  assert.deepEqual(quiet, []);

  const warned = await warnings(async () => {
    const unset = world({ HOSTTHIS_CLIENT_IP_SOURCE: undefined });
    for (const url of [at("/"), at("/api/rooms"), `https://${APEX}/healthz`]) {
      await (await unset.fetch(url)).arrayBuffer();
    }
    await (await world({ HOSTTHIS_CLIENT_IP_SOURCE: "x-real-ip" }).fetch(at("/"))).arrayBuffer();
  });
  assert.equal(warned.length, 1, warned.join("\n"));
  assert.match(warned[0], /HOSTTHIS_CLIENT_IP_SOURCE/);
  assert.match(warned[0], /one room creation bucket/);
});
