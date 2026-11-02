import { PodError, payloads } from "./solid/pod.js";

// Payload bytes live in the PAYLOADS R2 binding. The Worker stores them
// opaque (compression stays in hostthisd) and never reads a body into memory.
export const BLOB_PATH = "/blob/";
const BLOB_DELETE_PREFIX = "uploads/";
const R2_DELETE_BATCH = 1000;

// The Go adapters' rule: relative, slash-separated, plain segments.
function validBlobKey(key) {
  if (typeof key !== "string" || key === "" || /[\\\0]/.test(key)) {
    return false;
  }
  return key.split("/").every((seg) => seg !== "" && seg !== "." && seg !== "..");
}

// A key under uploads/ plus a trailing slash, so a delete ends on a segment
// boundary and can never reach past one upload.
function validBlobPrefix(prefix) {
  if (typeof prefix !== "string" || !prefix.endsWith("/")) {
    return false;
  }
  const body = prefix.slice(0, -1);
  return body.startsWith(BLOB_DELETE_PREFIX) && validBlobKey(body);
}

function blobKeyOf(url) {
  try {
    return decodeURIComponent(url.pathname.slice(BLOB_PATH.length));
  } catch {
    return null;
  }
}

// Lists the whole prefix before deleting, so no cursor has to survive the
// deletes. One upload's key names are bounded by its file count.
async function deleteBlobPrefix(bucket, prefix) {
  const keys = [];
  let cursor;
  do {
    const page = await bucket.list({ prefix, cursor });
    for (const object of page.objects) keys.push(object.key);
    cursor = page.truncated ? page.cursor : undefined;
  } while (cursor);
  for (let i = 0; i < keys.length; i += R2_DELETE_BATCH) {
    await bucket.delete(keys.slice(i, i + R2_DELETE_BATCH));
  }
}

// One closed byte range, the only form the Go adapter sends. Anything else is
// null.
function blobRange(header) {
  const match = /^bytes=(\d+)-(\d+)$/.exec(header ?? "");
  if (!match) {
    return null;
  }
  const offset = Number(match[1]);
  const end = Number(match[2]);
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(end) || end < offset) {
    return null;
  }
  return { offset, length: end - offset + 1 };
}

// A pod's refusal is a 502 naming its answer (docs/SPEC.md "Pod storage").
export async function handleBlob(request, env, url) {
  try {
    return await handleBlobRoute(request, env, url);
  } catch (error) {
    if (error instanceof PodError) {
      return Response.json({ error: "pod", status: error.status, message: error.message }, { status: 502 });
    }
    throw error;
  }
}

// Deletes every payload object under one upload's prefix, wherever it lives.
export async function deleteUploadPrefix(env, prefix) {
  if (!validBlobPrefix(prefix)) {
    throw new Error(`invalid upload prefix ${prefix}`);
  }
  if (!(await payloads.deletePodPrefix(env, prefix))) {
    await deleteBlobPrefix(env.PAYLOADS, prefix);
  }
}

async function handleBlobRoute(request, env, url) {
  if (url.pathname === "/blob" && request.method === "DELETE") {
    const prefix = url.searchParams.get("prefix");
    if (!validBlobPrefix(prefix)) {
      return new Response("invalid prefix\n", { status: 400 });
    }
    await deleteUploadPrefix(env, prefix);
    return new Response(null, { status: 204 });
  }
  if (!url.pathname.startsWith(BLOB_PATH)) {
    return new Response("not found\n", { status: 404 });
  }
  const key = blobKeyOf(url);
  if (!validBlobKey(key)) {
    return new Response("invalid key\n", { status: 400 });
  }
  if (request.method === "DELETE") {
    if (!key.startsWith(BLOB_DELETE_PREFIX)) {
      return new Response("invalid key\n", { status: 400 });
    }
    await payloads.delete(env, key);
    return new Response(null, { status: 204 });
  }
  if (request.method === "PUT") {
    // A zero-length request has no body stream.
    const length = Number(request.headers.get("Content-Length"));
    await payloads.put(env, key, request.body, request.headers.has("Content-Length") ? length : undefined);
    return new Response(null, { status: 204 });
  }
  if (request.method === "GET") {
    const range = request.headers.has("Range") ? blobRange(request.headers.get("Range")) : undefined;
    if (range === null) {
      return new Response("invalid range\n", { status: 400 });
    }
    const object = await payloads.get(env, key, range);
    if (object === null) {
      return new Response("not found\n", { status: 404 });
    }
    if (range) {
      // A pack member: exactly the recorded slice, which must lie inside the
      // object.
      if (range.offset + range.length > object.size) {
        await object.body?.cancel();
        return new Response("range outside object\n", { status: 416 });
      }
      return new Response(object.body, {
        status: 206,
        headers: {
          "Content-Type": "application/octet-stream",
          "Content-Length": String(range.length),
          "X-Blob-Size": String(range.length),
        },
      });
    }
    // The runtime streams a body chunked and drops Content-Length, so the
    // size travels in its own header.
    return new Response(object.body, {
      headers: {
        "Content-Type": "application/octet-stream",
        "Content-Length": String(object.size),
        "X-Blob-Size": String(object.size),
      },
    });
  }
  return new Response("method not allowed\n", { status: 405 });
}
