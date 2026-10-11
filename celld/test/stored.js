// Test payloads in the at-rest format: the magic, then a zstd frame of raw
// (uncompressed) blocks, which the decoder handles like any other block.
import { MAGIC } from "../src/public/zstd.js";

const BLOCK = 128 * 1024;
// No checksum, no content size, a 128 KiB window: the largest raw block.
const FRAME_HEADER = [0x28, 0xb5, 0x2f, 0xfd, 0x00, 7 << 3];

export function stored(content) {
  const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
  const blocks = Math.max(1, Math.ceil(bytes.length / BLOCK));
  const out = new Uint8Array(MAGIC.length + FRAME_HEADER.length + blocks * 3 + bytes.length);
  out.set(MAGIC);
  out.set(FRAME_HEADER, MAGIC.length);
  let w = MAGIC.length + FRAME_HEADER.length;
  for (let b = 0; b < blocks; b++) {
    const part = bytes.subarray(b * BLOCK, (b + 1) * BLOCK);
    const header = (part.length << 3) | (b === blocks - 1 ? 1 : 0);
    out.set([header & 0xff, (header >> 8) & 0xff, (header >> 16) & 0xff], w);
    out.set(part, w + 3);
    w += 3 + part.length;
  }
  return out;
}

// A payload bucket over a map of key to stored bytes, recording every call.
export function bucket(objects = new Map(), calls = []) {
  return {
    objects,
    calls,
    async get(key) {
      calls.push(`get ${key}`);
      const bytes = objects.get(key);
      if (bytes === undefined) return null;
      let sent = false;
      const body = new ReadableStream({
        pull(controller) {
          if (sent) {
            controller.close();
            return;
          }
          sent = true;
          // A few chunks, as a real bucket sends them.
          for (let off = 0; off < bytes.length; off += 4096) controller.enqueue(bytes.slice(off, off + 4096));
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
