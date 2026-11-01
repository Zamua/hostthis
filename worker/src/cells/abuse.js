import { cellRoute } from "./image.js";
import { dispatch, mutation, read } from "./ops.js";

// The ABUSE cell (docs/SPEC.md "Quarantine"): serializes quarantine, release
// and watcher key bans, keeps the quarantine index and the audit log, and
// applies the watcher's guards. The router decides the actor from the
// credential and stamps `now`; neither comes from the caller.

// Bans only: a quarantine is reversible, so the watcher's are not capped.
const WATCHER_BANS_PER_HOUR = 5;
const HOUR_MS = 3_600_000;
const AUDIT_MAX = 1000;
const REASON_MAX = 200;
const INDEX = "q:";
const KEY = /^SHA256:[0-9a-f]{64}$/;
const SLUG = /^[a-z0-9]{1,64}$/;

const ACTIONS = { quarantine: "quarantine", release: "release", bankey: "ban-key", roomdelete: "room-delete" };
const ROOM_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

async function ownerHash(identity) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(identity));
  return [...new Uint8Array(digest)].slice(0, 8).map((b) => b.toString(16).padStart(2, "0")).join("");
}

function fail(status, error) {
  return Response.json({ error }, { status });
}

const ABUSE_OPS = {
  quarantine: mutation((self, body) => self.quarantine(body)),
  release: mutation((self, body) => self.release(body)),
  bankey: mutation((self, body) => self.banKey(body)),
  roomdelete: mutation((self, body) => self.roomDelete(body)),
  list: read((self) => self.list()),
  audit: read((self) => self.auditLog()),
};

export class Abuse {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, ABUSE_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  // Items are full fingerprints (with or without `key:`) or 16-hex owner
  // hashes, as the usage scan prints them.
  async isProtected(identity) {
    const items = String(this.env?.HOSTTHIS_PROTECTED_OWNERS ?? "").split(",").map((s) => s.trim()).filter(Boolean);
    if (items.length === 0) {
      return false;
    }
    const hash = await ownerHash(identity);
    const fp = identity.replace(/^key:/, "");
    return items.some((item) => item === hash || item.replace(/^key:/, "") === fp);
  }

  async record(body, target, outcome, extra = {}) {
    const log = (await this.state.storage.get("audit")) ?? [];
    log.push({
      at: body.now, actor: body.actor, action: ACTIONS[body.op], target, reason: body.reason ?? "", outcome, ...extra,
    });
    await this.state.storage.put("audit", log.slice(-AUDIT_MAX));
  }

  // Accepted watcher actions of one kind in the hour before now.
  async watcherCount(action, now) {
    const log = (await this.state.storage.get("audit")) ?? [];
    return log.filter((e) => e.actor === "watcher" && e.action === action && e.outcome === "ok"
      && now - e.at < HOUR_MS).length;
  }

  validReason(reason) {
    return typeof reason === "string" && new TextEncoder().encode(reason).length <= REASON_MAX;
  }

  paste(slug) {
    return this.env.PASTES.get(this.env.PASTES.idFromName(slug));
  }

  async quarantine(input) {
    const body = { ...input, op: "quarantine" };
    if (!SLUG.test(body.slug ?? "") || !this.validReason(body.reason)) {
      return fail(400, "invalid-request");
    }
    const q = `slug=${encodeURIComponent(body.slug)}`;
    const current = await this.paste(body.slug).fetch(new Request(`https://cell/paste/operatorrow?${q}`));
    if (current.status === 404) {
      await this.record(body, body.slug, "no-such-paste");
      return fail(404, "no-such-paste");
    }
    if (!current.ok) {
      throw new Error(`operatorrow answered ${current.status}`);
    }
    const { row } = await current.json();
    if (body.actor === "watcher" && await this.isProtected(row.identity)) {
      await this.record(body, body.slug, "protected-owner");
      return fail(403, "protected-owner");
    }
    const res = await this.paste(body.slug).fetch(new Request(`https://cell/paste/quarantine?${q}`, {
      method: "POST", body: JSON.stringify({ reason: body.reason, actor: body.actor, at: body.now }),
    }));
    if (!res.ok) {
      throw new Error(`quarantine answered ${res.status}`);
    }
    const { already } = await res.json();
    if (!already) {
      await this.state.storage.put(INDEX + body.slug, {
        owner: row.identity, reason: body.reason, actor: body.actor, at: body.now,
      });
    }
    await this.record(body, body.slug, "ok", { owner: await ownerHash(row.identity), already });
    return Response.json({ slug: body.slug, quarantined: true, already });
  }

  async release(input) {
    const body = { ...input, op: "release" };
    if (!SLUG.test(body.slug ?? "")) {
      return fail(400, "invalid-request");
    }
    const res = await this.paste(body.slug).fetch(new Request(
      `https://cell/paste/release?slug=${encodeURIComponent(body.slug)}`, { method: "POST", body: "{}" }));
    if (!res.ok) {
      throw new Error(`release answered ${res.status}`);
    }
    const { released } = await res.json();
    await this.state.storage.delete(INDEX + body.slug);
    await this.record(body, body.slug, "ok", { released });
    return Response.json({ slug: body.slug, released });
  }

  // The key is named directly, or as the owner of a slug, which keeps full
  // fingerprints out of callers that only see owner hashes.
  async banKey(input) {
    const body = { ...input, op: "bankey" };
    if (!this.validReason(body.reason)) {
      return fail(400, "invalid-request");
    }
    let fp = typeof body.identity === "string" ? body.identity.replace(/^key:/, "") : "";
    if (!fp && SLUG.test(body.slug ?? "")) {
      const res = await this.paste(body.slug).fetch(new Request(
        `https://cell/paste/operatorrow?slug=${encodeURIComponent(body.slug)}`));
      if (res.status === 404) {
        await this.record(body, body.slug, "no-such-paste");
        return fail(404, "no-such-paste");
      }
      if (!res.ok) {
        throw new Error(`operatorrow answered ${res.status}`);
      }
      fp = String((await res.json()).row.identity ?? "").replace(/^key:/, "");
    }
    if (!KEY.test(fp)) {
      return fail(400, "invalid-request");
    }
    if (body.actor === "watcher" && await this.watcherCount(ACTIONS.bankey, body.now) >= WATCHER_BANS_PER_HOUR) {
      await this.record(body, fp, "rate-capped");
      return fail(429, "rate-capped");
    }
    if (body.actor === "watcher" && await this.isProtected(`key:${fp}`)) {
      await this.record(body, fp, "protected-owner");
      return fail(403, "protected-owner");
    }
    const ns = this.env.DENYLIST;
    const res = await ns.get(ns.idFromName("denylist")).fetch(new Request("https://cell/denylist/add", {
      method: "POST", body: JSON.stringify({ entry: fp, reason: body.reason }),
    }));
    if (!res.ok) {
      await this.record(body, fp, `denylist-${res.status}`);
      return fail(res.status === 507 ? 507 : 502, res.status === 507 ? "denylist-full" : "denylist-failed");
    }
    await this.record(body, fp, "ok");
    return Response.json({ identity: fp, banned: true });
  }

  // Operator only: the router never maps the watcher's credential here.
  async roomDelete(input) {
    const body = { ...input, op: "roomdelete" };
    if (!SLUG.test(body.app ?? "") || !ROOM_ID.test(body.room ?? "")) {
      return fail(400, "invalid-request");
    }
    const target = `${body.app}/${await ownerHash(body.room)}`;
    const ns = this.env.ROOMS;
    const res = await ns.get(ns.idFromName(`${body.app}|${body.room}`)).fetch(new Request(
      `https://cell/room/remove?room=${encodeURIComponent(`${body.app}|${body.room}`)}`,
      { method: "POST", body: JSON.stringify({ now: body.now }) },
    ));
    const answer = await res.json().catch(() => ({}));
    if (!res.ok) {
      await this.record(body, target, `room-${res.status}`);
      return fail(502, answer.error ?? "room-remove-failed");
    }
    await this.record(body, target, answer.removed ? "ok" : "absent");
    return Response.json({ app: body.app, removed: Boolean(answer.removed) });
  }

  async list() {
    const rows = await this.state.storage.list({ prefix: INDEX });
    const quarantined = [...rows].map(([key, row]) => ({ slug: key.slice(INDEX.length), ...row }));
    quarantined.sort((a, b) => a.at - b.at);
    return Response.json({ quarantined });
  }

  async auditLog() {
    return Response.json({ entries: (await this.state.storage.get("audit")) ?? [] });
  }
}
