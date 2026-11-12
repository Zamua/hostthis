// Reads of the payload bucket. A bucket get can fail for a moment while the
// object is there, so an open is tried again, briefly, before the caller
// answers for it. Only the open is retried: once a body is flowing, an error
// ends it.
const RETRY_DELAYS_MS = [50, 200];

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// bucket.get(key, options), tried again after each delay while it throws. A
// missing object is an answer (null), not a failure, and is not retried.
export async function bucketGet(bucket, key, options, delays = RETRY_DELAYS_MS) {
  for (const delay of delays) {
    try {
      return await bucket.get(key, options);
    } catch (error) {
      console.warn(`warn: bucket get retry: ${key}: ${error.message}`);
      await pause(delay);
    }
  }
  return bucket.get(key, options);
}
