import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { parseRange, sliceStream } from "../src/public/range.js";

const vectors = JSON.parse(readFileSync(new URL("../../testdata/byte-ranges.json", import.meta.url)));

test("parseRange runs the vectors hostthisd's ParseRange runs", () => {
  for (const c of vectors.cases) {
    const want = c.verdict === "satisfiable"
      ? { verdict: c.verdict, start: c.start, length: c.length }
      : { verdict: c.verdict };
    assert.deepEqual(parseRange(c.header === "" ? null : c.header, vectors.size), want, c.header);
  }
});

function chunked(bytes, size) {
  let at = 0;
  return new ReadableStream({
    pull(controller) {
      if (at >= bytes.length) {
        controller.close();
        return;
      }
      controller.enqueue(bytes.subarray(at, at + size));
      at += size;
    },
  });
}

test("sliceStream passes exactly the range across chunk boundaries", async () => {
  const bytes = Uint8Array.from({ length: 100 }, (_, i) => i);
  for (const [start, length] of [[0, 100], [7, 13], [95, 5], [0, 1]]) {
    const got = new Uint8Array(await new Response(sliceStream(chunked(bytes, 6), start, length)).arrayBuffer());
    assert.deepEqual(got, bytes.subarray(start, start + length), `${start}+${length}`);
  }
});

test("sliceStream errors when the source ends before the range", async () => {
  const bytes = new Uint8Array(10);
  await assert.rejects(new Response(sliceStream(chunked(bytes, 4), 5, 10)).arrayBuffer());
});
