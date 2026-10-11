import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";

import { REPO } from "../build/shells.mjs";
import { DecodeError, INPUT_BATCH, MAGIC, zstdDecoder } from "../src/public/zstd.js";

const VENDOR = join(REPO, "celld", "vendor", "zstd");
const OUT_CHUNK = 128 * 1024;

setFlagsFromString("--expose-gc");
const gc = runInNewContext("gc");

let dir;
let manifest;

// The corpus comes from internal/zstdenc, the encoder hostthisd stores with.
before(() => {
  dir = mkdtempSync(join(tmpdir(), "hostthis-zstd-"));
  execFileSync("go", ["run", "./celld/test/zstdcorpus", dir], { cwd: REPO, stdio: ["ignore", "ignore", "inherit"] });
  manifest = JSON.parse(readFileSync(join(dir, "manifest.json"), "utf8"));
});

after(() => rmSync(dir, { recursive: true, force: true }));

function stored(name) {
  return new Uint8Array(readFileSync(join(dir, `${name}.hz`)));
}

// bytes as a stream of chunkSize slices; pulled counts what the decoder took.
function source(bytes, chunkSize = 4096) {
  const stats = { pulled: 0, cancelled: false };
  let off = 0;
  const stream = new ReadableStream({
    pull(controller) {
      if (off >= bytes.length) {
        controller.close();
        return;
      }
      const chunk = bytes.subarray(off, off + chunkSize);
      off += chunk.length;
      stats.pulled = off;
      controller.enqueue(chunk);
    },
    cancel() {
      stats.cancelled = true;
    },
  }, { highWaterMark: 0 });
  return { stream, stats };
}

// Hashes the decoded stream without keeping it. onChunk runs after each chunk.
async function drain(readable, onChunk = () => {}) {
  const hash = createHash("sha256");
  let size = 0;
  let largest = 0;
  for await (const chunk of readable) {
    hash.update(chunk);
    size += chunk.length;
    largest = Math.max(largest, chunk.length);
    onChunk(size);
  }
  return { size, sha256: hash.digest("hex"), largest };
}

function decode(bytes, chunkSize) {
  return source(bytes, chunkSize).stream.pipeThrough(zstdDecoder());
}

function frame(windowDescriptor) {
  // Frame header without a checksum or content size, then one empty last raw
  // block.
  return Uint8Array.of(...MAGIC, 0x28, 0xb5, 0x2f, 0xfd, 0x00, windowDescriptor, 0x01, 0x00, 0x00);
}

test("the committed decoder module is the one its build records", () => {
  const [want, name] = readFileSync(join(VENDOR, "zd.wasm.sha256"), "utf8").trim().split(/\s+/);
  assert.equal(name, "zd.wasm");
  assert.equal(createHash("sha256").update(readFileSync(join(VENDOR, "zd.wasm"))).digest("hex"), want);
});

test("every corpus blob decodes to its bytes, whatever the input chunking", async () => {
  for (const [name, want] of Object.entries(manifest)) {
    if (name === "repeat100m") continue;
    for (const chunkSize of [1, 4096, 1 << 20]) {
      if (chunkSize === 1 && want.size > 1 << 20) continue;
      const got = await drain(decode(stored(name), chunkSize));
      assert.equal(got.size, want.size, `${name} in ${chunkSize}-byte chunks`);
      assert.equal(got.sha256, want.sha256, `${name} in ${chunkSize}-byte chunks`);
      assert.ok(got.largest <= OUT_CHUNK, `${name}: a ${got.largest}-byte chunk`);
    }
  }
});

// CLAUDE.md principle 1: the decoder holds one output buffer at a time, so
// 100 MiB of output costs no more memory than 100 KiB.
test("a 100 MiB decode stays under a few MiB of live memory", async () => {
  // Warm the pooled instance to a 1 MiB window, so its heap is in the baseline.
  await drain(decode(stored("text")));
  gc();
  const baseline = process.memoryUsage().arrayBuffers;
  let peak = 0;
  let nextSample = 0;
  const got = await drain(decode(stored("repeat100m"), 4096), (size) => {
    if (size >= nextSample) {
      nextSample += 1 << 20;
      gc();
      peak = Math.max(peak, process.memoryUsage().arrayBuffers - baseline);
    }
  });
  assert.equal(got.size, manifest.repeat100m.size);
  assert.equal(got.sha256, manifest.repeat100m.sha256);
  assert.ok(got.largest <= OUT_CHUNK);
  assert.ok(peak < 4 * 1024 * 1024, `peak ${(peak / 1048576).toFixed(1)} MiB over the baseline`);
});

test("output is decoded on demand, reading the input no further than one batch ahead", async () => {
  const { stream, stats } = source(stored("incompressible"), 4096);
  const reader = stream.pipeThrough(zstdDecoder()).getReader();
  const first = await reader.read();
  assert.ok(first.value.length > 0);
  assert.ok(stats.pulled <= INPUT_BATCH + 2 * 4096, `pulled ${stats.pulled} bytes for the first chunk`);
  await reader.cancel();
  assert.ok(stats.cancelled, "cancelling the output cancels the input");
});

async function rejects(bytes, pattern, chunkSize) {
  await assert.rejects(drain(decode(bytes, chunkSize)), (error) => {
    assert.ok(error instanceof DecodeError, `${error}`);
    assert.match(error.message, pattern);
    return true;
  });
}

test("a truncated stream is an error", async () => {
  const text = stored("text");
  await rejects(text.subarray(0, text.length >> 1), /truncated/);
  await rejects(text.subarray(0, text.length - 2), /truncated/);
  await rejects(stored("onebyte").subarray(0, 10), /truncated/);
  // The magic and no frame at all.
  await rejects(MAGIC, /truncated/);
});

test("a flipped byte is an error", async () => {
  // A raw block: only the frame checksum can catch it.
  const raw = stored("incompressible");
  raw[raw.length >> 1] ^= 0x01;
  await rejects(raw, /checksum/);
  const text = stored("text");
  text[text.length >> 1] ^= 0x40;
  await rejects(text, /zstd:/);
  const checksum = stored("onebyte");
  checksum[checksum.length - 1] ^= 0x01;
  await rejects(checksum, /checksum/);
});

test("an object without the magic is an error", async () => {
  const bad = stored("text");
  bad[1] = 0x58;
  await rejects(bad, /bad magic/);
  await rejects(stored("onebyte").subarray(MAGIC.length), /bad magic/);
  await rejects(new Uint8Array(0), /missing magic/);
  await rejects(MAGIC.subarray(0, 3), /missing magic/, 1);
});

test("bytes after the stream are an error", async () => {
  const good = stored("onebyte");
  const trailing = new Uint8Array(good.length + 8);
  trailing.set(good);
  trailing.fill(0x41, good.length);
  await rejects(trailing, /zstd:/);
});

test("a window over 8 MiB is refused, and one of 8 MiB is not", async () => {
  // Window descriptor: exponent in the top five bits, log = 10 + exponent.
  await rejects(frame(14 << 3), /memory|window/i);
  assert.equal((await drain(decode(frame(13 << 3)))).size, 0);
});

test("a decoder that failed decodes the next stream correctly", async () => {
  const raw = stored("incompressible");
  raw[raw.length >> 1] ^= 0x01;
  await assert.rejects(drain(decode(raw)));
  for (const name of ["text", "onebyte"]) {
    const got = await drain(decode(stored(name)));
    assert.equal(got.sha256, manifest[name].sha256, name);
  }
});
