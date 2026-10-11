// The rooms API, room push and the room WebSocket on an app's public origin
// (docs/SPEC.md "Rooms (app persistence)", "Real-time room relay", "Room
// push"), answering as hostthisd's internal/http does. Every cell request is
// built here from parsed values: nothing from the client's URL, query,
// headers or body is passed on as it came.

import {
  APP_MAX_ROOM_BYTES,
  PUSH_BODY_LIMIT,
  ROOM_CREATE_WINDOW_MS,
  ROOM_MAX_BYTES,
  ROOM_MAX_KEYS,
  ROOM_PUT_READ_LIMIT,
  ROOMS_PER_APP,
  ROOMS_PER_SUBNET,
  byteStringText,
  clientAddress,
  clientSubnet,
  decodeEndpoint,
  decodeSchedule,
  decodeSubscription,
  goTime,
  originRefusal,
  pushEndpointProblem,
  pushItemWire,
  pushScheduleProblem,
  pushSubscriptionProblem,
  roomKeyProblem,
  roomsRoute,
  upgradeRefusal,
} from "./policy.js";
import {
  base64Bytes, bytesBase64, decodeFirstJSON, roomValueIsJSON, roomWireValue,
} from "./wire.js";

const NO_STORE = { "Cache-Control": "no-store" };

// http.Error: the message plus the newline Fprintln adds. Most of hostthisd's
// messages carry their own newline too, so their bodies end in two.
function goError(status, message, headers = {}) {
  return new Response(`${message}\n`, {
    status,
    headers: {
      ...NO_STORE,
      "Content-Type": "text/plain; charset=utf-8",
      "X-Content-Type-Options": "nosniff",
      ...headers,
    },
  });
}

const notFound = () => goError(404, "404 page not found");
const internalError = () => goError(500, "internal error\n");
const malformedBody = () => goError(400, "malformed JSON body\n");

function methodNotAllowed(...allowed) {
  return goError(405, "method not allowed\n", { Allow: allowed.join(", ") });
}

// json.Encoder output: the document and a newline.
function goJSON(status, text) {
  return new Response(`${text}\n`, {
    status,
    headers: { ...NO_STORE, "Content-Type": "application/json; charset=utf-8" },
  });
}

const noContent = () => new Response(null, { status: 204, headers: NO_STORE });

// One cell request: { status, body } with body the parsed JSON answer or null.
async function ask(stub, path, { method = "GET", query, body } = {}) {
  const url = new URL(`https://cell${path}`);
  for (const [name, value] of Object.entries(query ?? {})) {
    url.searchParams.set(name, value);
  }
  const init = { method };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { "content-type": "application/json" };
  }
  const res = await stub.fetch(new Request(url, init));
  const text = await res.text();
  let parsed = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    // a plain-text answer such as a cell's "not found"
  }
  return { status: res.status, body: parsed };
}

// The first `limit` bytes of the body; the rest is never read.
async function readCapped(request, limit) {
  if (!request.body) {
    return new Uint8Array(0);
  }
  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  while (total < limit) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    const take = Math.min(value.length, limit - total);
    chunks.push(value.subarray(0, take));
    total += take;
  }
  if (total >= limit) {
    await reader.cancel().catch(() => {});
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

class App {
  constructor(env, slug) {
    this.env = env;
    this.slug = slug;
    this.paste = env.PASTES.get(env.PASTES.idFromName(slug));
  }

  room(id) {
    return this.env.ROOMS.get(this.env.ROOMS.idFromName(`${this.slug}|${id}`));
  }

  // A read that fails counts as no app, as hostthisd's appExists does.
  async exists() {
    try {
      const res = await ask(this.paste, "/paste/exists");
      return res.status === 200 && res.body?.exists === true;
    } catch {
      return false;
    }
  }
}

// The response for a decoded app path, or null when the path is outside the
// rooms surface. client is { forwarded, host }: host is the Host the client
// addressed, port included.
export async function serveRooms(request, env, slug, path, client) {
  const route = roomsRoute(path);
  if (!route) {
    return null;
  }
  try {
    return await serveRoute(request, env, new App(env, slug), route, client);
  } catch {
    return internalError();
  }
}

async function serveRoute(request, env, app, route, client) {
  const { method } = request;
  switch (route.kind) {
    case "pushkey":
      return pushKey(method, app);
    case "collection":
      return method === "POST" ? createRoom(request, env, app, client) : methodNotAllowed("POST");
    case "malformed":
      return goError(400, "malformed room id\n");
    case "scan":
      return method === "GET" ? scanRoom(app.room(route.id)) : methodNotAllowed("GET");
    case "ws":
      return joinRoom(request, env, app, route.id, client);
    case "push":
      return roomPush(request, env, app, route);
    default:
      return roomValue(request, app.room(route.id), route.key);
  }
}

async function createRoom(request, env, app, client) {
  if (!(await app.exists())) {
    return notFound();
  }
  const subnet = clientSubnet(clientAddress(request.headers, env.HOSTTHIS_CLIENT_IP_SOURCE, client.forwarded));
  const now = Date.now();
  const counts = await ask(app.paste, "/paste/roomcounts", {
    query: { subnet, now: String(now), window: String(ROOM_CREATE_WINDOW_MS) },
  });
  if (counts.status !== 200 || !counts.body) {
    return internalError();
  }
  if (counts.body.perSubnet >= ROOMS_PER_SUBNET || counts.body.perApp >= ROOMS_PER_APP) {
    return goError(429, "room creation rate limit reached\n", {
      "Retry-After": String(ROOM_CREATE_WINDOW_MS / 1000),
    });
  }
  const id = crypto.randomUUID();
  const created = await ask(app.room(id), "/room/create", {
    method: "POST",
    body: {
      appSlug: app.slug, id, createdAt: now, updatedAt: now, subnet, at: now, appCap: APP_MAX_ROOM_BYTES,
    },
  });
  if (created.status === 507) {
    return goError(507, "app room storage is at capacity\n");
  }
  if (created.status >= 300) {
    return internalError();
  }
  return goJSON(201, JSON.stringify({ id }));
}

// Each value re-encoded from its stored bytes, keys sorted as Go sorts a map.
async function scanRoom(room) {
  const res = await ask(room, "/room/scan");
  if (res.status === 404) {
    return notFound();
  }
  if (res.status !== 200 || !res.body?.values) {
    return internalError();
  }
  const values = res.body.values;
  const members = Object.keys(values).sort()
    .map((key) => `${JSON.stringify(key)}:${roomWireValue(base64Bytes(values[key]))}`);
  return goJSON(200, `{${members.join(",")}}`);
}

async function roomValue(request, room, key) {
  const { method } = request;
  if (method !== "GET" && method !== "PUT" && method !== "DELETE") {
    return methodNotAllowed("GET", "PUT", "DELETE");
  }
  if (roomKeyProblem(key)) {
    return goError(400, "invalid key\n");
  }
  const text = byteStringText(key);
  if (method === "GET") {
    const res = await ask(room, "/room/get", { query: { key: text } });
    if (res.status === 404) {
      return notFound();
    }
    if (res.status !== 200 || !res.body) {
      return internalError();
    }
    const bytes = base64Bytes(res.body.value ?? "");
    return new Response(bytes, {
      headers: {
        ...NO_STORE,
        "Content-Type": roomValueIsJSON(bytes) ? "application/json; charset=utf-8" : "application/octet-stream",
      },
    });
  }
  let res;
  if (method === "PUT") {
    let bytes;
    try {
      bytes = await readCapped(request, ROOM_PUT_READ_LIMIT);
    } catch {
      return goError(400, "read body\n");
    }
    res = await ask(room, "/room/put", {
      method: "POST",
      body: {
        key: text, value: bytesBase64(bytes), wire: roomWireValue(bytes),
        roomCap: ROOM_MAX_BYTES, keyCap: ROOM_MAX_KEYS, appCap: APP_MAX_ROOM_BYTES, now: Date.now(),
      },
    });
    if (res.status === 413) {
      return goError(413, "room is at its data cap\n");
    }
    if (res.status === 507) {
      return goError(507, "app room storage is at capacity\n");
    }
  } else {
    res = await ask(room, "/room/del", { method: "POST", body: { key: text, now: Date.now() } });
  }
  if (res.status === 404) {
    return notFound();
  }
  return res.status >= 300 ? internalError() : noContent();
}

// The checks hostthisd makes before its 101, then the cell's own answer to a
// join built from the slug and the UUID alone.
async function joinRoom(request, env, app, id, client) {
  if (!(await app.exists())) {
    return notFound();
  }
  const room = app.room(id);
  try {
    if ((await ask(room, "/room/meta")).status !== 200) {
      return notFound();
    }
  } catch {
    return notFound();
  }
  const refused = upgradeRefusal(request.method, request.headers);
  if (refused) {
    return goError(refused.status, refused.message, refused.headers);
  }
  const origin = originRefusal(request.headers.get("Origin"), client.host, env.HOSTTHIS_APEX_DOMAIN);
  if (origin) {
    return goError(403, origin);
  }
  const join = new URL("https://cell/room/join");
  join.searchParams.set("room", `${app.slug}|${id}`);
  const res = await room.fetch(new Request(join, { headers: { Upgrade: "websocket" } }));
  if (res.status === 101 && res.webSocket) {
    return new Response(null, { status: 101, webSocket: res.webSocket, headers: NO_STORE });
  }
  if (res.status === 429) {
    return goError(429, "room connection limit reached\n");
  }
  return internalError();
}

// Null for a body that cannot be read or decoded.
async function readPushBody(request, decode) {
  let bytes;
  try {
    bytes = await readCapped(request, PUSH_BODY_LIMIT);
  } catch {
    return null;
  }
  return decode(decodeFirstJSON(bytes));
}

function pushSubject(env) {
  return `${env.HOSTTHIS_PUBLIC_SCHEME || "https"}://${env.HOSTTHIS_APEX_DOMAIN}`;
}

async function pushKey(method, app) {
  if (!(await app.exists())) {
    return notFound();
  }
  if (method !== "GET") {
    return methodNotAllowed("GET");
  }
  const res = await ask(app.paste, "/paste/pushkey", { method: "POST" });
  if (res.status !== 200 || typeof res.body?.key !== "string") {
    return internalError();
  }
  return goJSON(200, JSON.stringify({ key: res.body.key }));
}

const PUSH_ROUTES = new Set(["/subscriptions", "/schedule", "/test"]);

async function roomPush(request, env, app, route) {
  if (!(await app.exists())) {
    return notFound();
  }
  if (!PUSH_ROUTES.has(route.sub)) {
    return goError(400, "reserved key\n");
  }
  const room = app.room(route.id);
  const { method } = request;
  if (route.sub === "/subscriptions") {
    if (method === "PUT") return putSubscription(request, env, room);
    if (method === "DELETE") return deleteSubscription(request, room);
    if (method === "GET") return listSubscriptions(room);
    return methodNotAllowed("GET", "PUT", "DELETE");
  }
  if (route.sub === "/schedule") {
    if (method === "PUT") return putSchedule(request, env, room);
    if (method === "GET") return getSchedule(room);
    return methodNotAllowed("GET", "PUT");
  }
  return method === "POST" ? testPush(room) : methodNotAllowed("POST");
}

// A push write's answer: 404 and the cap the caller can act on are statuses
// of their own, anything else past 2xx is a fault, as in internal/celld.
function pushWritten(res, capCode) {
  if (res.status === 404) {
    return notFound();
  }
  if (res.status === 413 && res.body?.error === capCode) {
    return goError(413, "room is at its push subscription cap\n");
  }
  return res.status >= 300 ? internalError() : noContent();
}

async function putSubscription(request, env, room) {
  const sub = await readPushBody(request, decodeSubscription);
  if (!sub) {
    return malformedBody();
  }
  const problem = pushSubscriptionProblem(sub);
  if (problem) {
    return goError(400, `${problem}\n`);
  }
  return pushWritten(await ask(room, "/room/pushsubput", {
    method: "POST",
    body: {
      endpoint: sub.endpoint, keys: { p256dh: sub.keys.p256dh, auth: sub.keys.auth },
      now: Date.now(), subject: pushSubject(env),
    },
  }), "too-many-subscriptions");
}

async function deleteSubscription(request, room) {
  const body = await readPushBody(request, decodeEndpoint);
  if (!body) {
    return malformedBody();
  }
  const problem = pushEndpointProblem(body.endpoint);
  if (problem) {
    return goError(400, `${problem}\n`);
  }
  const res = await ask(room, "/room/pushsubdel", { method: "POST", body: { endpoint: body.endpoint } });
  if (res.status === 404) {
    return notFound();
  }
  return res.status >= 300 ? internalError() : noContent();
}

async function listSubscriptions(room) {
  const res = await ask(room, "/room/pushsublist", { method: "POST" });
  if (res.status === 404) {
    return notFound();
  }
  if (res.status !== 200 || !Array.isArray(res.body?.subscriptions)) {
    return internalError();
  }
  const subs = res.body.subscriptions.map((s) => ({ endpoint: s.endpoint, added: goTime(s.added) }));
  return goJSON(200, JSON.stringify(subs));
}

async function putSchedule(request, env, room) {
  const schedule = await readPushBody(request, decodeSchedule);
  if (!schedule) {
    return malformedBody();
  }
  const now = Date.now();
  const problem = pushScheduleProblem(schedule, now);
  if (problem) {
    return goError(400, `${problem}\n`);
  }
  return pushWritten(await ask(room, "/room/pushscheduleput", {
    method: "POST",
    body: {
      tz: schedule.tz, items: (schedule.items ?? []).map(pushItemWire), now, subject: pushSubject(env),
    },
  }), "too-many-items");
}

async function getSchedule(room) {
  const res = await ask(room, "/room/pushscheduleget", { method: "POST" });
  if (res.status === 404) {
    return notFound();
  }
  if (res.status !== 200 || !res.body) {
    return internalError();
  }
  return goJSON(200, JSON.stringify({
    tz: typeof res.body.tz === "string" ? res.body.tz : "",
    items: (res.body.items ?? []).map(pushItemWire),
  }));
}

async function testPush(room) {
  const res = await ask(room, "/room/pushtest", { method: "POST", body: { now: Date.now() } });
  if (res.status === 404) {
    return notFound();
  }
  if (res.status === 429) {
    const wait = Number(res.body?.retryAfter);
    return goError(429, "push test rate limit reached\n", {
      "Retry-After": String(wait > 0 ? Math.ceil(wait) : 60),
    });
  }
  if (res.status >= 300) {
    return internalError();
  }
  return goJSON(202, JSON.stringify({ sent: res.body?.sent ?? 0, pruned: res.body?.pruned ?? 0 }));
}
