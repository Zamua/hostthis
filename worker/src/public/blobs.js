// Stored payloads as response bodies: an object from PAYLOADS, its at-rest
// encoding checked and decoded on the way out.
import { zstdDecoder } from "./zstd.js";

// The object is missing or does not decode; the message names why.
export class BlobError extends Error {}

// The stream's first chunk is read before returning, so a missing magic or a
// damaged frame header surfaces here, where the caller can still answer 500.
// A later decode error errors the returned stream.
async function primed(stream) {
  const reader = stream.getReader();
  let first;
  try {
    first = await reader.read();
  } catch (error) {
    throw new BlobError(error.message);
  }
  return new ReadableStream({
    async pull(controller) {
      const next = first ?? await reader.read();
      first = null;
      if (next.done) {
        controller.close();
      } else {
        controller.enqueue(next.value);
      }
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });
}

// stream, calling onError with the error that ends it early, if one does.
export function reportingErrors(stream, onError) {
  const reader = stream.getReader();
  return new ReadableStream({
    async pull(controller) {
      let next;
      try {
        next = await reader.read();
      } catch (error) {
        onError(error);
        throw error;
      }
      if (next.done) {
        controller.close();
      } else {
        controller.enqueue(next.value);
      }
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });
}

// The decoded bytes an entry names: its whole object, or its pack slice.
export async function openPayload(env, { key, offset, length }) {
  const object = await env.PAYLOADS.get(key, length > 0 ? { range: { offset, length } } : undefined);
  if (object === null) {
    throw new BlobError(`object ${key} missing`);
  }
  return primed(object.body.pipeThrough(zstdDecoder()));
}
