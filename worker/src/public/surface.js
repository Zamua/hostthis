// Composes the public read path for one request's configuration: the caches
// wrap it only when configured and the runtime has a Cache API, and the
// runtime's FixedLengthStream, where it has one, holds each body to its
// stated length.
import { BUILD } from "hostthis:build";

import { openPayload } from "./blobs.js";
import { EDGE_CACHE_MAX_BYTES, withBlobCache, withEdgeCache } from "./caches.js";
import { normalizeHost } from "./host.js";
import { withFixedLength } from "./length.js";
import { readPath } from "./reads.js";


function positive(value) {
  const n = Number(value);
  return Number.isSafeInteger(n) && n > 0 ? n : 0;
}

// cacheStorage is the runtime's caches global and fixedLengthStream its
// FixedLengthStream class, each absent where there is none.
export function publicReads(
  env,
  cacheStorage = typeof caches === "undefined" ? undefined : caches,
  fixedLengthStream = typeof FixedLengthStream === "undefined" ? undefined : FixedLengthStream,
) {
  const cache = cacheStorage?.default;
  const ttl = positive(env.HOSTTHIS_EDGE_CACHE_TTL_S);
  const blobMax = positive(env.HOSTTHIS_BLOB_CACHE_MAX_BYTES);
  let open = openPayload;
  if (cache && blobMax) {
    const origin = `https://${normalizeHost(env.HOSTTHIS_APEX_DOMAIN ?? "")}/_blobs`;
    open = withBlobCache(open, { cache, maxBytes: blobMax, origin });
  }
  let read = readPath(open);
  if (cache && ttl) {
    const maxBytes = positive(env.HOSTTHIS_EDGE_CACHE_MAX_BYTES) || EDGE_CACHE_MAX_BYTES;
    read = withEdgeCache(read, { cache, ttl, build: BUILD, maxBytes });
  }
  if (fixedLengthStream) {
    read = withFixedLength(read, fixedLengthStream);
  }
  return read;
}
