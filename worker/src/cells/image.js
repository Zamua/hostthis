import { b64url } from "../solid/jose.js";
import { unb64u } from "../push.js";
import { putInBatches, setOwn } from "./records.js";

// --- moving cells between runtimes -------------------------------------------
//
// Every cell class answers /__cell/name, /__cell/image, /__cell/restore and
// /__cell/disarm on
// its own fetch, ahead of its op table. The Worker's router forwards only
// /identity/, /paste/, /room/ and /subnet/ paths, so only code holding a class
// binding reaches these.

const CELL_PATH = "/__cell/";
const CELL_IMAGE_FORMAT = "hostthis.cell-image";
const CELL_IMAGE_VERSION = 1;
// Cloudflare refuses a put of more than 128 entries.
export const MAX_PUT_ENTRIES = 128;
const VALUE_TAG = "$t";
// Progress of a restore in parts; no app key starts with "__".
const RESTORE_MARKER = "__cell:restore";
const NON_FINITE = ["NaN", "Infinity", "-Infinity", "-0"];
// Uint8Array travels as "bytes"; these as "view" with their name.
const TYPED_VIEWS = {
  Int8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array,
  Float32Array, Float64Array, BigInt64Array, BigUint64Array, DataView,
};

class UnsupportedValue extends Error {}
class InvalidImage extends Error {}

// One stored value as JSON. Strings, booleans, null, finite numbers, plain
// objects and dense arrays are themselves; every other type storage can hold
// travels as { "$t": type, v }. A plain object owning a "$t" key is wrapped so
// it never reads as a tag.
function encodeValue(value) {
  switch (typeof value) {
    case "string":
    case "boolean":
      return value;
    case "number":
      if (Number.isFinite(value) && !Object.is(value, -0)) {
        return value;
      }
      return { $t: "number", v: Object.is(value, -0) ? "-0" : String(value) };
    case "bigint":
      return { $t: "bigint", v: String(value) };
    case "undefined":
      return { $t: "undefined" };
    case "object":
      break;
    default:
      throw new UnsupportedValue(typeof value);
  }
  if (value === null) {
    return null;
  }
  if (Array.isArray(value)) {
    // JSON has no hole and no named property on an array.
    if (Object.keys(value).length !== value.length) {
      throw new UnsupportedValue("sparse or keyed array");
    }
    return value.map(encodeValue);
  }
  const proto = Object.getPrototypeOf(value);
  if (proto === Object.prototype || proto === null) {
    const out = {};
    for (const [key, item] of Object.entries(value)) {
      setOwn(out, key, encodeValue(item));
    }
    return Object.hasOwn(value, VALUE_TAG) ? { $t: "object", v: out } : out;
  }
  if (value instanceof Date) {
    return { $t: "date", v: encodeValue(value.getTime()) };
  }
  if (value instanceof Map) {
    return { $t: "map", v: [...value].map(([key, item]) => [encodeValue(key), encodeValue(item)]) };
  }
  if (value instanceof Set) {
    return { $t: "set", v: [...value].map(encodeValue) };
  }
  if (value instanceof ArrayBuffer) {
    return { $t: "buffer", v: b64url(new Uint8Array(value)) };
  }
  if (ArrayBuffer.isView(value)) {
    const bytes = b64url(new Uint8Array(value.buffer, value.byteOffset, value.byteLength));
    if (value.constructor === Uint8Array) {
      return { $t: "bytes", v: bytes };
    }
    const kind = value.constructor.name;
    if (Object.hasOwn(TYPED_VIEWS, kind) && TYPED_VIEWS[kind] === value.constructor) {
      return { $t: "view", k: kind, v: bytes };
    }
  }
  throw new UnsupportedValue(proto?.constructor?.name ?? "object");
}

function decodeObject(object) {
  const out = {};
  for (const [key, item] of Object.entries(object)) {
    setOwn(out, key, decodeValue(item));
  }
  return out;
}

function decodeBytes(text) {
  const bytes = unb64u(text);
  if (!bytes) {
    throw new InvalidImage("bytes are not base64");
  }
  return bytes;
}

function decodeList(list) {
  if (!Array.isArray(list)) {
    throw new InvalidImage("tagged list is not an array");
  }
  return list;
}

function decodeValue(value) {
  if (value === null || typeof value !== "object") {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map(decodeValue);
  }
  if (!Object.hasOwn(value, VALUE_TAG)) {
    return decodeObject(value);
  }
  const { v } = value;
  switch (value[VALUE_TAG]) {
    case "undefined":
      return undefined;
    case "number":
      if (!NON_FINITE.includes(v)) {
        throw new InvalidImage(`number ${v} needs no tag`);
      }
      return Number(v);
    case "bigint":
      if (typeof v !== "string" || !/^-?\d+$/.test(v)) {
        throw new InvalidImage("bigint is not an integer");
      }
      return BigInt(v);
    case "object":
      if (v === null || typeof v !== "object" || Array.isArray(v)) {
        throw new InvalidImage("wrapped object is not an object");
      }
      return decodeObject(v);
    case "date": {
      const time = decodeValue(v);
      if (typeof time !== "number") {
        throw new InvalidImage("date is not a time");
      }
      return new Date(time);
    }
    case "map":
      return new Map(decodeList(v).map((pair) => {
        if (!Array.isArray(pair) || pair.length !== 2) {
          throw new InvalidImage("map entry is not a pair");
        }
        return [decodeValue(pair[0]), decodeValue(pair[1])];
      }));
    case "set":
      return new Set(decodeList(v).map(decodeValue));
    case "buffer":
      return decodeBytes(v).buffer;
    case "bytes":
      return decodeBytes(v);
    case "view":
      if (!Object.hasOwn(TYPED_VIEWS, value.k)) {
        throw new InvalidImage(`unknown view ${value.k}`);
      }
      return new TYPED_VIEWS[value.k](decodeBytes(v).buffer);
    default:
      throw new InvalidImage(`unknown tag ${value[VALUE_TAG]}`);
  }
}

// The caller holds blockConcurrencyWhile, so the entries and the alarm describe
// one state. With start or limit the answer is one page of it.
async function cellImage(storage, url) {
  const options = {};
  const start = url.searchParams.get("start");
  if (start !== null) {
    options.start = start;
  }
  let limit = null;
  if (url.searchParams.has("limit")) {
    limit = Number(url.searchParams.get("limit"));
    if (!Number.isSafeInteger(limit) || limit < 1) {
      return Response.json({ error: "invalid-page" }, { status: 400 });
    }
    // One more than the page, to learn the next key.
    options.limit = limit + 1;
  }
  const kv = [];
  let next = null;
  for (const [key, value] of await storage.list(options)) {
    if (kv.length === limit) {
      next = key;
      break;
    }
    try {
      kv.push([key, encodeValue(value)]);
    } catch (error) {
      if (!(error instanceof UnsupportedValue)) {
        throw error;
      }
      return Response.json({ error: "unsupported-value", key, type: error.message }, { status: 500 });
    }
  }
  const alarm = (await storage.getAlarm()) ?? null;
  return Response.json({ format: CELL_IMAGE_FORMAT, version: CELL_IMAGE_VERSION, kv, alarm, next });
}

function decodePart(part) {
  if (part === undefined) {
    return null;
  }
  const { id, index, count } = part ?? {};
  if (typeof id !== "string" || id === "" || !Number.isSafeInteger(count) || count < 1 ||
      !Number.isSafeInteger(index) || index < 0 || index >= count) {
    throw new InvalidImage("part is not { id, index, count }");
  }
  return { id, index, count };
}

function decodeImage(image) {
  if (image === null || typeof image !== "object" || image.format !== CELL_IMAGE_FORMAT ||
      image.version !== CELL_IMAGE_VERSION || !Array.isArray(image.kv)) {
    throw new InvalidImage("not a version 1 cell image");
  }
  const alarm = image.alarm ?? null;
  if (alarm !== null && !Number.isFinite(alarm)) {
    throw new InvalidImage("alarm is not a time");
  }
  const entries = new Map();
  for (const pair of image.kv) {
    if (!Array.isArray(pair) || pair.length !== 2 || typeof pair[0] !== "string") {
      throw new InvalidImage("entry is not a [key, value] pair");
    }
    if (entries.has(pair[0])) {
      throw new InvalidImage(`key ${pair[0]} appears twice`);
    }
    if (pair[0] === RESTORE_MARKER) {
      throw new InvalidImage(`key ${pair[0]} is reserved`);
    }
    try {
      entries.set(pair[0], decodeValue(pair[1]));
    } catch (error) {
      throw new InvalidImage(`key ${pair[0]}: ${error.message}`);
    }
  }
  return { entries: [...entries], alarm, part: decodePart(image.part) };
}

async function holdsAny(storage, keys) {
  for (let i = 0; i < keys.length; i += MAX_PUT_ENTRIES) {
    if ((await storage.get(keys.slice(i, i + MAX_PUT_ENTRIES))).size > 0) {
      return true;
    }
  }
  return false;
}

// One transaction per image or part, so a refused write leaves the cell as the
// previous part left it, for a retry.
async function restoreCell(state, request) {
  let image;
  try {
    image = decodeImage(await request.json());
  } catch (error) {
    return Response.json({ error: "invalid-image", detail: String(error?.message ?? error) }, { status: 400 });
  }
  const part = image.part ?? { id: "", index: 0, count: 1 };
  const last = part.index === part.count - 1;
  return state.blockConcurrencyWhile(async () => {
    const { storage } = state;
    if (part.index === 0) {
      if ((await storage.list({ limit: 1 })).size > 0 || ((await storage.getAlarm()) ?? null) !== null) {
        return Response.json({ restored: false, reason: "not-blank" }, { status: 409 });
      }
    } else {
      const progress = await storage.get(RESTORE_MARKER);
      if (progress?.id !== part.id || progress.next !== part.index || progress.count !== part.count) {
        return Response.json({ restored: false, reason: "out-of-order" }, { status: 409 });
      }
      if (await holdsAny(storage, image.entries.map(([key]) => key))) {
        return Response.json({ restored: false, reason: "overlap" }, { status: 409 });
      }
    }
    try {
      await storage.transaction(async (tx) => {
        await putInBatches(tx, image.entries);
        if (!last) {
          await tx.put(RESTORE_MARKER, { id: part.id, next: part.index + 1, count: part.count });
          return;
        }
        if (part.index > 0) {
          await tx.delete(RESTORE_MARKER);
        }
        if (image.alarm !== null) {
          await tx.setAlarm(Math.max(image.alarm, Date.now()));
        }
      });
    } catch (error) {
      return Response.json({ error: "restore-failed", detail: String(error?.message ?? error) }, { status: 500 });
    }
    const answer = { restored: true, entries: image.entries.length };
    return Response.json(image.part ? { ...answer, part: part.index, complete: last } : answer);
  });
}

const CELL_ROUTES = {
  name: { method: "GET", run: (state) => Response.json({ name: state.id.name ?? null }) },
  image: {
    method: "GET",
    run: (state, request) => state.blockConcurrencyWhile(() => cellImage(state.storage, new URL(request.url))),
  },
  restore: { method: "POST", run: restoreCell },
  // Stops a cell's alarm and answers the time it was set for, so a mover can
  // hand the alarm to the copy and the original never fires again.
  disarm: {
    method: "POST",
    run: (state) => state.blockConcurrencyWhile(async () => {
      const alarm = (await state.storage.getAlarm()) ?? null;
      await state.storage.deleteAlarm();
      return Response.json({ alarm });
    }),
  },
};

// Null for a path outside /__cell/, which the class then routes itself.
export async function cellRoute(state, request) {
  const { pathname } = new URL(request.url);
  if (!pathname.startsWith(CELL_PATH)) {
    return null;
  }
  const name = pathname.slice(CELL_PATH.length);
  if (!Object.hasOwn(CELL_ROUTES, name)) {
    return new Response("unknown cell route\n", { status: 404 });
  }
  if (request.method !== CELL_ROUTES[name].method) {
    return new Response("method not allowed\n", { status: 405 });
  }
  return CELL_ROUTES[name].run(state, request);
}
