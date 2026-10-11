// Fixtures shared by the read surface suites.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { REPO } from "../build/shells.mjs";
import worker, { Paste } from "../src/index.js";
import { FakeStorage, state } from "./harness.js";
import { bucket, stored } from "./stored.js";

// hostthisd's responses, recorded by internal/http's TestShells_GoldenResponses.
export const GOLDEN = JSON.parse(readFileSync(join(REPO, "testdata", "shells-golden.json"), "utf8"));

export const APEX = "paste.test";
export const SLUG = "abc23456";
export const TOKEN = "t0ken";
export const FORWARDED = "X-Hostthis-Forwarded-Host";
export const UPDATED = Date.UTC(2026, 5, 7, 14);

// The public Worker over real Paste cells in memory and a recording payload
// bucket. Every request a Paste cell receives is recorded.
export function world(vars = {}) {
  const storages = new Map();
  const cellRequests = [];
  const payloadCalls = [];
  const payloads = bucket(new Map(), payloadCalls);
  const assetCalls = [];
  const storageOf = (slug) => {
    if (!storages.has(slug)) storages.set(slug, new FakeStorage());
    return storages.get(slug);
  };
  const env = {
    HOSTTHIS_APEX_DOMAIN: APEX,
    HOSTTHIS_CELLD_TOKEN: TOKEN,
    HOSTTHIS_CLIENT_IP_SOURCE: "cf-connecting-ip",
    ...vars,
    PAYLOADS: payloads,
    PASTES: {
      idFromName: (name) => name,
      get: (name) => ({
        async fetch(request) {
          const body = request.body ? await request.text() : null;
          cellRequests.push({ name, method: request.method, url: request.url, body });
          const init = { method: request.method, headers: request.headers };
          if (body !== null) init.body = body;
          return new Paste(state(storageOf(name)), env).fetch(new Request(request.url, init));
        },
      }),
    },
  };
  const waits = [];
  const ctx = { waitUntil: (promise) => waits.push(promise) };
  const w = {
    env,
    ctx,
    waits,
    storageOf,
    cellRequests,
    payloadCalls,
    assetCalls,
    row(slug, row) {
      storageOf(slug).data.set("row", structuredClone(row));
    },
    blob(key, content) {
      payloads.objects.set(key, typeof content === "string" || content instanceof Uint8Array ? stored(content) : content);
    },
    // Raw bytes, as the deploy stores renderer assets.
    object(key, bytes) {
      payloads.objects.set(key, bytes);
    },
    assets(serve) {
      env.ASSETS = {
        async fetch(request) {
          assetCalls.push(`${request.method} ${new URL(request.url).pathname}`);
          return serve(request);
        },
      };
    },
    fetch(url, init = {}) {
      return worker.fetch(new Request(url, init), env, ctx);
    },
  };
  return w;
}

export const at = (path, slug = SLUG) => `https://${slug}.${APEX}${path}`;

export function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

export function headersOf(res) {
  const out = {};
  for (const [name, value] of res.headers) {
    if (name !== "content-length") out[name] = value;
  }
  return out;
}

// The response equals hostthisd's record: status, every header, and the body.
export async function assertGolden(res, name) {
  const want = GOLDEN[name];
  assert.ok(want, `no golden ${name}`);
  assert.equal(res.status, want.status, name);
  const wantHeaders = Object.fromEntries(Object.entries(want.header).map(([k, v]) => [k.toLowerCase(), v]));
  assert.deepEqual(headersOf(res), wantHeaders, name);
  const body = new Uint8Array(await res.arrayBuffer());
  assert.equal(body.length, want.size, name);
  assert.equal(sha256(body), want.sha256, name);
}

// A document stored at uploads/a/0, size bytes when decoded.
export function documentRow(kind, status = "ready", size = 23) {
  return {
    kind, status, updatedAt: UPDATED, uploadId: "ua",
    manifest: { Files: { "/": { Key: "uploads/a/0", Size: size, CompressedSize: 20, ContentType: "", Kind: kind } } },
  };
}

export const HI = "<h1>hi</h1>";

// An HTML paste of HI.
export function hiRow() {
  return documentRow("html", "ready", HI.length);
}
