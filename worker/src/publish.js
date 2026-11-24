// The publish route: a new paste's whole synchronous commit in one internal
// request (docs/SPEC.md "Create: the synchronous half"). It runs the same three
// cell ops a caller could send one at a time, reserve on the Identity cell, put
// on the Paste cell, confirm on the Identity cell, so every guarantee those ops
// make holds unchanged; only the round trips from the caller collapse into one.
import { cellCall } from "./cells/ops.js";

export const PUBLISH_PATH = "/publish/paste";

const CREATE_PASTE = "create_paste";

// A refusal the caller can name, with the status a cell would have answered.
function refuse(status, error, extra = {}) {
  return Response.json({ error, ...extra }, { status });
}

// The answer body of a cell response, or {} when it carries none.
async function bodyOf(res) {
  try {
    return await res.json();
  } catch {
    return {};
  }
}

// One cell call that a thrown fetch (the callee reset, the runtime refused it)
// turns into null instead of an exception.
async function tryCell(ns, id, path, query, body) {
  try {
    return await cellCall(ns, id, path, query, body);
  } catch {
    return null;
  }
}

function shapeOk(body) {
  const row = body?.row;
  return row && typeof row === "object"
    && typeof row.slug === "string" && row.slug !== ""
    && typeof row.identity === "string" && row.identity !== ""
    && typeof body.generation === "string" && body.generation !== ""
    && typeof body.fingerprint === "string" && body.fingerprint !== ""
    && typeof body.intentId === "string" && body.intentId !== ""
    && (body.pod === undefined || typeof body.pod === "string");
}

// POST /publish/paste. The body is {row, generation, fingerprint, intentId,
// userCap, now, pod?}; the row names the slug and the owner, so both cells are
// addressed from it and no reservation can name one slug while the row names
// another.
//
// 200 {active} is a committed paste, active being the owner's charged total
// after it. A refusal is the status and code the failing cell op answers:
// 507 over-quota, 409 slug-taken (reserve, or a put whose slug was claimed or
// whose create was aborted, its charge released), 409 pod-moved {pod} (the
// owner's pod link is not the one the row's upload id was minted for; nothing
// was written), 413 version-too-large (released). A put that never answered is
// 502 put-unanswered: its intent stays and resolves the create, as for a
// caller that lost the put's response.
export async function publishRoute(request, env) {
  if (request.method !== "POST") {
    return new Response("method not allowed\n", { status: 405 });
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return refuse(400, "malformed-publish");
  }
  if (!shapeOk(body)) {
    return refuse(400, "malformed-publish");
  }
  const { row, generation, fingerprint, intentId } = body;
  const owner = row.identity;
  const slug = row.slug;

  // 1. Reserve: quota, charge and the durable intent in one Identity commit.
  const reserveBody = {
    slug, generation,
    size: row.size, userCap: body.userCap, now: body.now,
    status: row.status, updatedAt: row.updatedAt, kind: row.kind, name: row.name,
    intent: { id: intentId, kind: CREATE_PASTE, subject: slug, fingerprint, startedAt: body.now },
  };
  if (body.pod !== undefined) {
    reserveBody.pod = body.pod;
  }
  const reserved = await tryCell(env.IDENTITY, owner, "/identity/reserve", "scope", reserveBody);
  if (!reserved) {
    return refuse(502, "reserve-unanswered");
  }
  if (!reserved.ok) {
    return reserved;
  }
  const { active } = await bodyOf(reserved);

  // 2. Put. Retried once on no answer: the cell replays a same-generation,
  //    same-fingerprint put, so the retry settles whether the row landed.
  const putBody = { row, generation, fingerprint };
  let put = await tryCell(env.PASTES, slug, "/paste/put", "slug", putBody);
  put ??= await tryCell(env.PASTES, slug, "/paste/put", "slug", putBody);
  if (!put) {
    // Not released: the row may have landed. The intent resolves it.
    return refuse(502, "put-unanswered");
  }
  if (!put.ok) {
    const refusal = await bodyOf(put);
    const neverLands = (put.status === 409 && (refusal.error === "slug-taken" || refusal.error === "create-aborted"))
      || put.status === 413;
    if (!neverLands) {
      // Another conflict may name this generation's own row: the intent
      // resolves it.
      return refuse(put.status, refusal.error ?? "put-refused");
    }
    // This generation's row can never land, so its charge is released. A lost
    // release leaves the intent, whose recovery releases it.
    const released = await tryCell(env.IDENTITY, owner, "/identity/release", "scope", { slug, generation, intentId });
    await released?.body?.cancel();
    return put.status === 413 ? refuse(413, "version-too-large") : refuse(409, "slug-taken");
  }

  await put.body?.cancel();

  // 3. Confirm: the row is durable, so the intent is discharged. An unanswered
  //    confirm is still a committed paste; the intent's recovery confirms it.
  const confirmed = await tryCell(env.IDENTITY, owner, "/identity/confirm", "scope",
    { slug, generation, status: row.status, intentId });
  if (confirmed && !confirmed.ok) {
    const refusal = await bodyOf(confirmed);
    return refuse(502, "confirm-refused", { status: confirmed.status, detail: refusal.error ?? "" });
  }
  await confirmed?.body?.cancel();
  return Response.json({ active: active ?? 0 });
}
