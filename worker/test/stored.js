// Test payloads in the at-rest format: the magic, then a zstd frame of raw
// (uncompressed) blocks, which the decoder handles like any other block.
import { MAGIC } from "../src/public/zstd.js";

const BLOCK = 128 * 1024;
const CHECKSUM_FLAG = 0x04;

// The frame header: no content size, a 128 KiB window (the largest raw block),
// and with checksum the content checksum flag.
const frameHeader = (checksum) => [0x28, 0xb5, 0x2f, 0xfd, checksum ? CHECKSUM_FLAG : 0x00, 7 << 3];

// With checksum, the frame ends in the content checksum the decoder verifies
// (the low 32 bits of XXH64, little-endian), as hostthisd writes it.
export function stored(content, { checksum = false } = {}) {
  const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
  const header = frameHeader(checksum);
  const blocks = Math.max(1, Math.ceil(bytes.length / BLOCK));
  const out = new Uint8Array(MAGIC.length + header.length + blocks * 3 + bytes.length + (checksum ? 4 : 0));
  out.set(MAGIC);
  out.set(header, MAGIC.length);
  let w = MAGIC.length + header.length;
  for (let b = 0; b < blocks; b++) {
    const part = bytes.subarray(b * BLOCK, (b + 1) * BLOCK);
    const blockHeader = (part.length << 3) | (b === blocks - 1 ? 1 : 0);
    out.set([blockHeader & 0xff, (blockHeader >> 8) & 0xff, (blockHeader >> 16) & 0xff], w);
    out.set(part, w + 3);
    w += 3 + part.length;
  }
  if (checksum) {
    new DataView(out.buffer).setUint32(w, Number(xxh64(bytes) & 0xffffffffn), true);
  }
  return out;
}

const P1 = 11400714785074694791n;
const P2 = 14029467366897019727n;
const P3 = 1609587929392839161n;
const P4 = 9650029242287828579n;
const P5 = 2870177450012600261n;
const U64 = (1n << 64n) - 1n;
const rotl = (x, r) => ((x << BigInt(r)) | (x >> BigInt(64 - r))) & U64;
const round = (acc, lane) => (rotl((acc + lane * P2) & U64, 31) * P1) & U64;
const merge = (acc, v) => (((acc ^ round(0n, v)) * P1) + P4) & U64;

// XXH64 with seed 0.
function xxh64(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const len = bytes.length;
  let p = 0;
  let h;
  if (len >= 32) {
    const v = [(P1 + P2) & U64, P2, 0n, (U64 + 1n - P1) & U64];
    for (; p + 32 <= len; p += 32) {
      for (let i = 0; i < 4; i++) v[i] = round(v[i], view.getBigUint64(p + 8 * i, true));
    }
    h = (rotl(v[0], 1) + rotl(v[1], 7) + rotl(v[2], 12) + rotl(v[3], 18)) & U64;
    for (const lane of v) h = merge(h, lane);
  } else {
    h = P5;
  }
  h = (h + BigInt(len)) & U64;
  for (; p + 8 <= len; p += 8) {
    h = ((rotl(h ^ round(0n, view.getBigUint64(p, true)), 27) * P1) + P4) & U64;
  }
  if (p + 4 <= len) {
    h = ((rotl(h ^ ((BigInt(view.getUint32(p, true)) * P1) & U64), 23) * P2) + P3) & U64;
    p += 4;
  }
  for (; p < len; p++) {
    h = (rotl(h ^ ((BigInt(bytes[p]) * P5) & U64), 11) * P1) & U64;
  }
  h = ((h ^ (h >> 33n)) * P2) & U64;
  h = ((h ^ (h >> 29n)) * P3) & U64;
  return h ^ (h >> 32n);
}

// A payload bucket over a map of key to stored bytes, recording every call.
// bodies records each body handed out: its key, how far it was read and
// whether it was cancelled.
export function bucket(objects = new Map(), calls = []) {
  const bodies = [];
  return {
    objects,
    calls,
    bodies,
    async get(key) {
      calls.push(`get ${key}`);
      const bytes = objects.get(key);
      if (bytes === undefined) return null;
      const stats = { key, sent: 0, cancelled: false };
      bodies.push(stats);
      // 4 KiB chunks, read on demand, as a real bucket sends them.
      const body = new ReadableStream({
        pull(controller) {
          if (stats.sent >= bytes.length) {
            controller.close();
            return;
          }
          controller.enqueue(bytes.slice(stats.sent, stats.sent + 4096));
          stats.sent = Math.min(stats.sent + 4096, bytes.length);
        },
        cancel() {
          stats.cancelled = true;
        },
      });
      return { body, size: bytes.length };
    },
    async put(key) {
      calls.push(`put ${key}`);
    },
    async delete(key) {
      calls.push(`delete ${key}`);
    },
    async list() {
      calls.push("list");
      return { objects: [], truncated: false };
    },
    async head(key) {
      calls.push(`head ${key}`);
      return null;
    },
  };
}
