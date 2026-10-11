// Decodes the at-rest blob format, the magic HZ\0\x01 then one zstd stream,
// with libzstd's decompressor compiled to WASM (vendor/zstd). Pull-based: a
// call decodes only when the consumer reads and yields at most one output
// buffer, so memory stays flat however far the bytes expand (CLAUDE.md
// principle 1).
import zdModule from "../../vendor/zstd/zd.wasm";

export const MAGIC = Uint8Array.of(0x48, 0x5a, 0x00, 0x01);

// 8 MiB. hostthisd writes 1 MiB windows; a frame declaring more than this is
// refused before the decoder allocates for it.
export const MAX_WINDOW_LOG = 23;

// The decoder waits for this much input before a call, unless the input has
// ended: a call per small chunk costs more than the decoding.
export const INPUT_BATCH = 64 * 1024;

const POOL_MAX = 4;

// An instance's heap never shrinks, so one grown past this (an 8 MiB window
// takes about 19 MiB; hostthisd's 1 MiB windows about 5) is dropped rather
// than kept idle in the pool.
const POOL_MAX_MEMORY = 16 * 1024 * 1024;

export class DecodeError extends Error {}

const imports = {
  // wasi-libc's allocator seeds a heap canary from it; nothing here is secret.
  wasi_snapshot_preview1: { random_get: () => 0 },
};

function cString(memory, ptr) {
  const bytes = new Uint8Array(memory.buffer, ptr);
  return new TextDecoder().decode(bytes.subarray(0, bytes.indexOf(0)));
}

// One instance and its decoding context. WASM memory never shrinks, so pooling
// keeps an instance's grown heap for the next decode.
class Instance {
  constructor() {
    const x = new WebAssembly.Instance(zdModule, imports).exports;
    x._initialize();
    this.x = x;
    this.dctx = x.zd_create(MAX_WINDOW_LOG);
    if (!this.dctx) {
      throw new DecodeError("zstd: cannot create a context");
    }
    this.inCap = Math.min(x.zd_in_size(), INPUT_BATCH);
    this.outCap = x.zd_out_size();
    this.inPtr = x.zd_malloc(this.inCap);
    this.outPtr = x.zd_malloc(this.outCap);
    // Two ZSTD_{in,out}Buffer structs: { ptr, size, pos } each, as u32s.
    this.ioPtr = x.zd_malloc(24);
    if (!this.inPtr || !this.outPtr || !this.ioPtr) {
      throw new DecodeError("zstd: cannot allocate buffers");
    }
  }

  reset() {
    if (this.x.zd_is_error(this.x.zd_reset(this.dctx))) {
      throw new DecodeError("zstd: cannot reset a context");
    }
  }

  // Views are taken per call: any call may grow memory, which detaches them.
  heap() {
    return new Uint8Array(this.x.memory.buffer);
  }

  // One ZSTD_decompressStream call over the first inLen bytes of the input
  // buffer, from inPos. Returns the new inPos, the bytes produced and whether
  // the call ended a frame.
  step(inLen, inPos) {
    const io = new Uint32Array(this.x.memory.buffer, this.ioPtr, 6);
    io.set([this.inPtr, inLen, inPos, this.outPtr, this.outCap, 0]);
    const ret = this.x.zd_stream(this.dctx, this.ioPtr);
    if (this.x.zd_is_error(ret)) {
      throw new DecodeError(`zstd: ${cString(this.x.memory, this.x.zd_error_name(ret))}`);
    }
    const after = new Uint32Array(this.x.memory.buffer, this.ioPtr, 6);
    return { inPos: after[2], produced: after[5], frameEnd: ret === 0 };
  }
}

const pool = [];
let busy = 0;

function acquire() {
  const instance = pool.pop() ?? new Instance();
  instance.reset();
  busy++;
  return instance;
}

// keep is false for an instance in an unknown state.
function release(instance, keep) {
  busy--;
  if (keep && pool.length < POOL_MAX && instance.x.memory.buffer.byteLength <= POOL_MAX_MEMORY) {
    pool.push(instance);
  }
}

// Instances idle in the pool and in use by a decode.
export function decoderStats() {
  return { idle: pool.length, busy };
}

// Input chunks waiting for the decoder, consumed front to back without copying
// the chunks they arrived in.
class Queue {
  constructor() {
    this.chunks = [];
    this.length = 0;
  }

  push(chunk) {
    if (chunk.length) {
      this.chunks.push(chunk);
      this.length += chunk.length;
    }
  }

  // Moves up to n bytes into target at offset; returns how many.
  drainInto(target, offset, n) {
    let moved = 0;
    while (moved < n && this.chunks.length) {
      const head = this.chunks[0];
      const k = Math.min(head.length, n - moved);
      target.set(head.subarray(0, k), offset + moved);
      moved += k;
      if (k === head.length) {
        this.chunks.shift();
      } else {
        this.chunks[0] = head.subarray(k);
      }
    }
    this.length -= moved;
    return moved;
  }
}

function asBytes(chunk) {
  if (chunk instanceof Uint8Array) {
    return chunk;
  }
  if (ArrayBuffer.isView(chunk)) {
    return new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
  }
  if (chunk instanceof ArrayBuffer) {
    return new Uint8Array(chunk);
  }
  throw new TypeError("zstd: input chunks must be bytes");
}

// A { writable, readable } pair for pipeThrough. The readable errors with a
// DecodeError on a bad magic, a truncated or corrupt stream (libzstd verifies
// the frame checksum), trailing bytes, or a window over the cap.
export function zstdDecoder() {
  const { readable: input, writable } = new TransformStream();
  const reader = input.getReader();
  const queue = new Queue();
  let head = new Uint8Array(0);
  let magicSeen = false;
  let inputEnded = false;
  let instance = null;
  let inLen = 0;
  let inPos = 0;
  // The last call filled the output buffer, so the decoder may hold more.
  let more = false;
  // The last call that made progress ended a frame.
  let frameEnd = false;

  const drop = (keep) => {
    if (instance) {
      release(instance, keep);
    }
    instance = null;
  };

  async function readInput() {
    const { done, value } = await reader.read();
    if (done) {
      inputEnded = true;
      if (!magicSeen) {
        throw new DecodeError("hz: missing magic");
      }
      return;
    }
    let chunk = asBytes(value);
    if (!magicSeen) {
      if (head.length + chunk.length < MAGIC.length) {
        const joined = new Uint8Array(head.length + chunk.length);
        joined.set(head);
        joined.set(chunk, head.length);
        head = joined;
        return;
      }
      const need = MAGIC.length - head.length;
      for (let i = 0; i < MAGIC.length; i++) {
        if ((i < head.length ? head[i] : chunk[i - head.length]) !== MAGIC[i]) {
          throw new DecodeError("hz: bad magic");
        }
      }
      chunk = chunk.subarray(need);
      head = null;
      magicSeen = true;
      instance = acquire();
    }
    queue.push(chunk);
  }

  // The next output chunk, or null at a clean end.
  async function next() {
    for (;;) {
      if (inPos === inLen && !more) {
        if (queue.length < INPUT_BATCH && !inputEnded) {
          await readInput();
          continue;
        }
        if (queue.length === 0) {
          if (!frameEnd) {
            throw new DecodeError("zstd: truncated");
          }
          return null;
        }
        inLen = queue.drainInto(instance.heap(), instance.inPtr, instance.inCap);
        inPos = 0;
      }
      const hadInput = inPos < inLen;
      const step = instance.step(inLen, inPos);
      const consumed = step.inPos - inPos;
      inPos = step.inPos;
      more = step.produced === instance.outCap;
      // A call after a frame's last byte that only reports the next frame's
      // header size makes no progress and leaves frameEnd as it was.
      if (step.frameEnd) {
        frameEnd = true;
      } else if (consumed > 0 || step.produced > 0) {
        frameEnd = false;
      }
      if (step.produced > 0) {
        return instance.heap().slice(instance.outPtr, instance.outPtr + step.produced);
      }
      if (hadInput && consumed === 0) {
        throw new DecodeError("zstd: no progress");
      }
    }
  }

  const readable = new ReadableStream({
    async pull(controller) {
      try {
        const chunk = await next();
        if (chunk) {
          controller.enqueue(chunk);
          return;
        }
        drop(true);
        controller.close();
      } catch (error) {
        // A trap leaves the instance in an unknown state; a zstd error code
        // does not, and the next acquire resets it.
        drop(error instanceof DecodeError);
        controller.error(error);
        reader.cancel(error).catch(() => {});
      }
    },
    cancel(reason) {
      drop(true);
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });

  return { writable, readable };
}
