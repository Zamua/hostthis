// Composes the public read path for one request's configuration.
import { openPayload } from "./blobs.js";
import { readPath } from "./reads.js";

const openStored = (env, entry) => openPayload(env, entry.key);

export function publicReads(_env) {
  return readPath(openStored);
}
