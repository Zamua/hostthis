// Single byte ranges on stored bodies (docs/SPEC.md "Byte ranges"), the
// rules hostthisd's domain.ParseRange applies.
import { LengthError } from "./length.js";

// A number too large to be exact still compares past any real size, which is
// how hostthisd's saturating parse answers it.
function rangeNumber(s) {
  return /^[0-9]+$/.test(s) ? Number(s) : null;
}

// { verdict: "ignored" | "satisfiable" | "unsatisfiable", start, length }
// for a Range header against a body of size bytes (size > 0).
export function parseRange(header, size) {
  const ignored = { verdict: "ignored" };
  if (header === null) {
    return ignored;
  }
  const eq = header.indexOf("=");
  if (eq < 0 || header.slice(0, eq).trim().toLowerCase() !== "bytes") {
    return ignored;
  }
  const spec = header.slice(eq + 1).trim();
  const dash = spec.indexOf("-");
  if (dash < 0 || spec.includes(",")) {
    return ignored;
  }
  const first = spec.slice(0, dash);
  const last = spec.slice(dash + 1);
  if (first === "") {
    const n = rangeNumber(last);
    if (n === null) {
      return ignored;
    }
    if (n === 0) {
      return { verdict: "unsatisfiable" };
    }
    const length = Math.min(n, size);
    return { verdict: "satisfiable", start: size - length, length };
  }
  const start = rangeNumber(first);
  if (start === null) {
    return ignored;
  }
  let end = size - 1;
  if (last !== "") {
    const e = rangeNumber(last);
    if (e === null || e < start) {
      return ignored;
    }
    end = Math.min(e, end);
  }
  if (start >= size) {
    return { verdict: "unsatisfiable" };
  }
  return { verdict: "satisfiable", start, length: end - start + 1 };
}

// stream with its first start bytes dropped and at most length passed on.
// The source is cancelled once the range is complete; one that ends before
// the range does errors.
export function sliceStream(stream, start, length) {
  const reader = stream.getReader();
  let skipped = 0;
  let sent = 0;
  return new ReadableStream({
    async pull(controller) {
      for (;;) {
        const next = await reader.read();
        if (next.done) {
          throw new LengthError(`body ended at ${skipped + sent} of ${start + length} bytes`);
        }
        let chunk = next.value;
        if (skipped < start) {
          const drop = Math.min(start - skipped, chunk.byteLength);
          skipped += drop;
          chunk = chunk.subarray(drop);
        }
        if (chunk.byteLength === 0) {
          continue;
        }
        chunk = chunk.subarray(0, length - sent);
        sent += chunk.byteLength;
        controller.enqueue(chunk);
        if (sent === length) {
          reader.cancel().catch(() => {});
          controller.close();
        }
        return;
      }
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });
}
