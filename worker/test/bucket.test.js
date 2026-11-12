import assert from "node:assert/strict";
import { test } from "node:test";

import { bucketGet } from "../src/bucket.js";

function flaky(failures, object = { body: "bytes" }) {
  const calls = [];
  return {
    calls,
    async get(key, options) {
      calls.push([key, options]);
      if (calls.length <= failures) {
        throw new Error("We encountered an internal error. Please try again. (10001)");
      }
      return object;
    },
  };
}

const quiet = async (fn) => {
  const warn = console.warn;
  const warned = [];
  console.warn = (line) => warned.push(line);
  try {
    return [await fn(), warned];
  } finally {
    console.warn = warn;
  }
};

test("a bucket get that fails for a moment is tried again and answers", async () => {
  const bucket = flaky(2);
  const [object, warned] = await quiet(() => bucketGet(bucket, "uploads/a/0", { range: { offset: 1, length: 2 } }, [0, 0]));
  assert.deepEqual(object, { body: "bytes" });
  assert.equal(bucket.calls.length, 3);
  assert.deepEqual(bucket.calls[2], ["uploads/a/0", { range: { offset: 1, length: 2 } }]);
  assert.equal(warned.length, 2);
  assert.match(warned[0], /bucket get retry: uploads\/a\/0: /);
});

test("a bucket get that keeps failing throws its last error", async () => {
  const bucket = flaky(3);
  await quiet(() => assert.rejects(bucketGet(bucket, "k", undefined, [0, 0]), /10001/));
  assert.equal(bucket.calls.length, 3);
});

test("a missing object is answered at once, not retried", async () => {
  const bucket = flaky(0, null);
  assert.equal(await bucketGet(bucket, "k", undefined, [0, 0]), null);
  assert.equal(bucket.calls.length, 1);
});
