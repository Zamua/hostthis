// Activity events (docs/SPEC.md "Activity events"): one Analytics Engine data
// point per publish and per page read, for counting active publishing keys
// and usage. An actor is written only as a keyed hash; a page read has none.
// Nothing here may fail or delay the request it describes: a write is not
// awaited, and every error is dropped.
const APP = "hostthis";
const NO_ACTOR = "-";
const HASH_HEX = 16;

const encoder = new TextEncoder();
let keyed = { salt: null, key: null };

async function saltKey(salt) {
  if (keyed.salt !== salt) {
    const key = crypto.subtle.importKey("raw", encoder.encode(salt), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    keyed = { salt, key };
  }
  return keyed.key;
}

// The first 16 hex characters of HMAC-SHA256(salt, "hostthis:" + id).
export async function actorHash(salt, id) {
  const mac = await crypto.subtle.sign("HMAC", await saltKey(salt), encoder.encode(`${APP}:${id}`));
  return [...new Uint8Array(mac, 0, HASH_HEX / 2)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// The dataset samples per index value. Only `active` is counted by distinct
// actors, so it alone keeps the bare actor; every other action has an index
// value of its own, and a burst of one cannot sample another away.
function write(dataset, actor, action, detail) {
  const index = action === "active" ? actor : `${actor}:${action}`;
  dataset.writeDataPoint({ indexes: [index], blobs: [APP, action, detail], doubles: [1] });
}

async function record(env, actor, events) {
  const dataset = env?.ACTIVITY;
  if (!dataset) {
    return;
  }
  let index = NO_ACTOR;
  if (actor !== null) {
    // Without a salt an actor cannot be written at all.
    if (!env.ACTIVITY_SALT) {
      return;
    }
    index = await actorHash(env.ACTIVITY_SALT, actor);
  }
  for (const [action, detail = ""] of events) {
    write(dataset, index, action, detail);
  }
}

// waiter is whatever keeps work alive past the response: a request's
// execution context or a cell's state. events are [action, detail] pairs.
function note(env, waiter, actor, events) {
  try {
    const done = record(env, actor, events).catch(() => {});
    waiter?.waitUntil?.(done);
  } catch {
    // An activity event is never worth an error.
  }
}

const DIRECTORY_KINDS = new Set(["site", "knowledgebase"]);

// A publish by an owner: the owner is active, and published one thing.
export function notePublish(env, waiter, owner, kind) {
  if (typeof owner !== "string" || owner === "") {
    return;
  }
  note(env, waiter, owner, [["active"], ["publish", DIRECTORY_KINDS.has(kind) ? "site" : "paste"]]);
}

// A room is created by a visitor of an app, who has no key: no actor.
export function noteRoomCreated(env, waiter) {
  note(env, waiter, null, [["publish", "room"]]);
}

const AUTOMATED = /bot|crawl|spider|slurp|preview|monitor|headless|curl|wget|python|go-http|okhttp|hostthis/i;

// Whether a response is a person's page view: a GET that answered a paste's
// HTML, not a status page, a subresource or an automated client.
export function isPageRead(request, res, contentCacheControl) {
  if (request.method !== "GET" || res.headers.get("Cache-Control") !== contentCacheControl) {
    return false;
  }
  const dest = request.headers.get("Sec-Fetch-Dest");
  if (res.status === 304) {
    // A revalidated page is a view too; a 304 states no type, so only a
    // navigation counts.
    if (dest !== "document") {
      return false;
    }
  } else if (res.status !== 200 || !(res.headers.get("Content-Type") ?? "").startsWith("text/html")
    || (dest !== null && dest !== "document")) {
    return false;
  }
  const agent = request.headers.get("User-Agent") ?? "";
  return agent !== "" && !AUTOMATED.test(agent);
}

// Wraps the public read path so each page view is counted.
export function countingPageReads(read, contentCacheControl) {
  return async function counted(request, env, ctx, target) {
    const res = await read(request, env, ctx, target);
    try {
      if (isPageRead(request, res, contentCacheControl)) {
        note(env, ctx, null, [["page_read"]]);
      }
    } catch {
      // As above.
    }
    return res;
  };
}
