// Bodies held to the length their response states (docs/SPEC.md "The public
// surface"), so bytes that end early never pass for a whole body.

export class LengthError extends Error {}

// stream, erroring before it gives more than size bytes or when it ends short
// of them. The chunk that would run past is never passed on, so a long body
// also reaches the client short. The chunk that completes size is held until
// the source has ended cleanly: a decoder verifies its checksum after its
// last output, and a body that fails there must still end short.
export function exactLength(stream, size) {
  const reader = stream.getReader();
  let seen = 0;
  const pastSize = () => {
    reader.cancel().catch(() => {});
    return new LengthError(`body ran past ${size} bytes`);
  };
  return new ReadableStream({
    async pull(controller) {
      const next = await reader.read();
      if (next.done) {
        if (seen < size) {
          throw new LengthError(`body ended at ${seen} of ${size} bytes`);
        }
        controller.close();
        return;
      }
      seen += next.value.byteLength;
      if (seen > size) {
        throw pastSize();
      }
      if (seen < size) {
        controller.enqueue(next.value);
        return;
      }
      let end = await reader.read();
      while (!end.done && end.value.byteLength === 0) {
        end = await reader.read();
      }
      if (!end.done) {
        throw pastSize();
      }
      controller.enqueue(next.value);
      controller.close();
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  }, { highWaterMark: 0 });
}

// The Content-Length a response states, or null.
export function statedLength(headers) {
  const value = headers.get("Content-Length");
  if (value === null || !/^[0-9]+$/.test(value)) {
    return null;
  }
  const n = Number(value);
  return Number.isSafeInteger(n) ? n : null;
}

// The read path with every body whose response states a Content-Length sent
// through the runtime's FixedLengthStream: a body that errors before that many
// bytes then aborts the response, where a chunked one would end as if whole.
export function withFixedLength(read, FixedLengthStream) {
  return async function fixedRead(request, env, ctx, target) {
    const res = await read(request, env, ctx, target);
    const length = statedLength(res.headers);
    if (length === null || !res.body) {
      return res;
    }
    const fixed = new FixedLengthStream(length);
    res.body.pipeTo(fixed.writable).catch(() => {});
    return new Response(fixed.readable, res);
  };
}
