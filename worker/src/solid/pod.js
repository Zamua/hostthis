// The payload store: the PAYLOADS bucket, or the owner's pod when an object's
// upload id names a pod link (docs/SPEC.md "Pod storage").
import { agentHeaders, solidEnabled } from "./identity.js";

const POD_UPLOAD = /^uploads\/pod_([a-f0-9]{16})_[A-Za-z0-9]+\//;
const LDP_CONTAINS = "http://www.w3.org/ns/ldp#contains";

// The pod refused or failed a request; status is its answer, or 0 when it was
// unreachable.
export class PodError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export function podLinkOfKey(key) {
  return POD_UPLOAD.exec(key)?.[1] ?? null;
}

export function linkCellName(linkId) {
  return `podlink:${linkId}`;
}

// A linked record never changes, so each isolate keeps the ones it has read.
const linked = new Map();

async function podBase(env, linkId) {
  const cached = linked.get(linkId);
  if (cached) {
    return cached;
  }
  if (!solidEnabled(env)) {
    throw new PodError(0, "pod storage is not configured");
  }
  const name = linkCellName(linkId);
  const res = await env.IDENTITY.get(env.IDENTITY.idFromName(name))
    .fetch(`https://cell/identity/linkget?scope=${encodeURIComponent(name)}`);
  const record = res.ok ? await res.json() : null;
  if (!record || record.state !== "linked") {
    throw new PodError(0, `pod link ${linkId} not found`);
  }
  const base = `${record.storage}hostthis/`;
  linked.set(linkId, base);
  return base;
}

async function podFetch(env, method, url, init = {}) {
  let res;
  try {
    res = await fetch(url, { ...init, method, headers: { ...(init.headers ?? {}), ...(await agentHeaders(env, method, url)) } });
  } catch (error) {
    throw new PodError(0, `pod unreachable: ${error.message}`);
  }
  return res;
}

async function refused(res, what) {
  await res.body?.cancel();
  return new PodError(res.status, `pod answered ${res.status} to ${what}`);
}

// An object as the bucket's get answers it: { body, size } or null.
async function podGet(env, base, key, range) {
  const url = base + key;
  const headers = range ? { Range: `bytes=${range.offset}-${range.offset + range.length - 1}` } : {};
  const res = await podFetch(env, "GET", url, { headers });
  if (res.status === 404) {
    await res.body?.cancel();
    return null;
  }
  if (range ? res.status !== 206 : res.status !== 200) {
    throw await refused(res, `GET ${key}`);
  }
  // The whole object's size, as the bucket reports it, so a range is checked
  // against the object it slices.
  const size = range
    ? Number(/\/(\d+)$/.exec(res.headers.get("Content-Range") ?? "")?.[1] ?? range.offset + range.length)
    : Number(res.headers.get("Content-Length"));
  if (!Number.isSafeInteger(size)) {
    throw await refused(res, `GET ${key} without a length`);
  }
  return { body: res.body, size };
}

async function podPut(env, base, key, body, length) {
  let payload = body;
  if (body && Number.isSafeInteger(length)) {
    const fixed = new FixedLengthStream(length);
    body.pipeTo(fixed.writable).catch(() => {});
    payload = fixed.readable;
  }
  const res = await podFetch(env, "PUT", base + key, {
    body: payload ?? new Uint8Array(0),
    headers: { "Content-Type": "application/octet-stream" },
  });
  if (!res.ok) {
    throw await refused(res, `PUT ${key}`);
  }
  await res.body?.cancel();
}

async function podDelete(env, url, what) {
  const res = await podFetch(env, "DELETE", url);
  await res.body?.cancel();
  if (!res.ok && res.status !== 404) {
    throw new PodError(res.status, `pod answered ${res.status} to DELETE ${what}`);
  }
}

// Every object an upload container lists.
async function containerMembers(env, url) {
  const res = await podFetch(env, "GET", url, { headers: { Accept: "application/ld+json" } });
  if (res.status === 404) {
    await res.body?.cancel();
    return null;
  }
  if (!res.ok) {
    throw await refused(res, `GET ${url}`);
  }
  const doc = await res.json();
  const nodes = Array.isArray(doc) ? doc : (doc["@graph"] ?? [doc]);
  const members = [];
  for (const node of nodes) {
    for (const ref of [].concat(node[LDP_CONTAINS] ?? node["ldp:contains"] ?? node.contains ?? [])) {
      const id = typeof ref === "string" ? ref : ref?.["@id"];
      if (id) members.push(new URL(id, url).href);
    }
  }
  return members;
}

async function podDeletePrefix(env, base, prefix) {
  const container = base + prefix;
  const members = await containerMembers(env, container);
  if (members === null) {
    return;
  }
  for (const member of members) {
    if (member.startsWith(container) && !member.endsWith("/")) {
      await podDelete(env, member, member.slice(base.length));
    }
  }
  await podDelete(env, container, prefix);
}

// The payload store's four operations, routed by key.
export const payloads = {
  async get(env, key, range) {
    const linkId = podLinkOfKey(key);
    if (!linkId) {
      const object = await env.PAYLOADS.get(key, range ? { range } : undefined);
      return object && { body: object.body, size: object.size };
    }
    return podGet(env, await podBase(env, linkId), key, range);
  },
  async put(env, key, body, length) {
    const linkId = podLinkOfKey(key);
    if (!linkId) {
      await env.PAYLOADS.put(key, body ?? new Uint8Array(0));
      return;
    }
    await podPut(env, await podBase(env, linkId), key, body, length);
  },
  async delete(env, key) {
    const linkId = podLinkOfKey(key);
    if (!linkId) {
      await env.PAYLOADS.delete(key);
      return;
    }
    const base = await podBase(env, linkId);
    await podDelete(env, base + key, key);
  },
  // True when the prefix is a pod upload's, which the caller then leaves alone.
  async deletePodPrefix(env, prefix) {
    const linkId = podLinkOfKey(prefix);
    if (!linkId) {
      return false;
    }
    await podDeletePrefix(env, await podBase(env, linkId), prefix);
    return true;
  },
};
