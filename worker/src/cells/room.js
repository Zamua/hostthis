import { normalizeHost } from "../public/host.js";
import { PUSH_DAILY_CAP, PUSH_KEY, PUSH_MAX_PAYLOAD, PUSH_MAX_SUBSCRIPTIONS, PUSH_PRUNE_GRACE_MS, PUSH_SEND_TIMEOUT_MS, PUSH_STALE_MS, PUSH_TEST_INTERVAL_MS, encryptPush, localDate, nextDue, publicItem, unb64u, utf8, validateSchedule, validateSubscription } from "../push.js";
import { PING_FRAME, PONG_FRAME, ROOM_CLIENT_FRAME_LIMIT, ROOM_PING_STALE_MS, ROOM_SOCKET_CAP, ROOM_STALE_CLOSE } from "../rooms/policy.js";
import { base64Bytes, compareCodePoints, roomWireValue } from "../rooms/wire.js";
import { cellRoute } from "./image.js";
import { cellCall, dispatch, mutation, read, runOp } from "./ops.js";
import { setOwn } from "./records.js";

// A Room cell owns one room's KV document, dense mutation sequence, budget
// recovery state, and hibernatable WebSockets. Its app's Paste cell serializes
// cross-room byte allocations.
const ROOM_OPS = {
  count: read((self) => Response.json({ sockets: self.state.getWebSockets().length })),
  meta: read((self, _body, _url, doc) => Response.json(doc.meta), { room: true }),
  get: read((self, _body, url, doc) => self.getValue(doc, url.searchParams.get("key")), { room: true }),
  scan: read((self, _body, _url, doc) => Response.json({ values: doc.kv, seq: doc.seq, wire: wireTexts(doc) }), { room: true }),
  create: mutation((self, body) => self.createBlocked(body)),
  put: mutation((self, body, _url, doc) => self.putBlocked(doc, body), { room: true }),
  del: mutation((self, body, _url, doc) => self.delBlocked(doc, body), { room: true }),
  pushsublist: read((self) => self.pushSubList(), { room: true }),
  pushscheduleget: read((self) => self.pushScheduleGet(), { room: true }),
  pushsubput: mutation((self, body) => self.pushSubPut(body), { room: true }),
  pushsubdel: mutation((self, body) => self.pushSubDel(body), { room: true }),
  pushscheduleput: mutation((self, body) => self.pushSchedulePut(body), { room: true }),
  pushtest: mutation((self, body, _url, doc) => self.pushTest(doc, body), { room: true }),
};

// The kind of a budget operation: a growth carries the mutation it waits to
// apply; a release was applied before it was recorded.
export function budgetKind(pending) {
  if (!pending) return "idle";
  return pending.mutation !== undefined ? "growing" : "releasing";
}

// The alarm instant a room needs: the earlier of its budget retry and its next
// push, or null for none.
export function alarmDeadline(pending, push) {
  const times = [pending?.retryAt, push?.nextFireAt].filter((t) => typeof t === "number");
  return times.length ? Math.min(...times) : null;
}

export class Room {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    // Without an apex the cell relays every frame and admits every join:
    // hostthisd's proxy owns admission and the frame limit there.
    this.publicSurface = publicSurfaceOn(env);
    if (this.publicSurface) {
      // The runtime answers the ping itself, without waking the cell, and
      // records when it last did: the liveness admit() reads.
      state.setWebSocketAutoResponse?.(new WebSocketRequestResponsePair(PING_FRAME, PONG_FRAME));
    }
  }

  // The app's paste cell, which owns the per-app room budget and the creation
  // ledger. Reached CELL TO CELL from inside this room's event: the app cell
  // decides against its own current totals, so the per-app cap is exact, and
  // the caller pays one round trip instead of three.
  // The room's whole state as ONE document: five separate keys made a commit
  // five sequential storage writes, which was most of a write's latency. A
  // room is capped at 256 KiB of values, so one document is always small.
  async loadRoom() {
    let doc = await this.state.storage.get("state");
    if (doc === undefined) {
      // A room written before the single-document layout: assemble once from
      // the legacy keys; the next commit persists the consolidated form.
      const meta = await this.state.storage.get("meta");
      if (meta === undefined) {
        return undefined;
      }
      doc = {
        meta,
        kv: (await this.state.storage.get("kv")) ?? {},
        wire: (await this.state.storage.get("wire")) ?? {},
        bytes: (await this.state.storage.get("bytes")) ?? 0,
        seq: (await this.state.storage.get("seq")) ?? 0,
        budgetVersion: 0,
      };
    }
    doc.budgetVersion ??= 0;
    return doc;
  }

  async saveRoom(doc) {
    await this.state.storage.put("state", doc);
  }

  // Broadcast to every connected socket, dead ones dropped silently: send() on
  // a closing socket throws, and one dead client must not cost the rest their
  // frame.
  broadcast(frame) {
    this.broadcastText(JSON.stringify(frame));
  }

  broadcastText(data) {
    for (const ws of this.state.getWebSockets()) {
      try {
        ws.send(data);
      } catch {
        // reaped by the runtime; nothing to do
      }
    }
  }

  appCall(appSlug, op, body) {
    return cellCall(this.env.PASTES, appSlug, `/paste/${op}`, "slug", body);
  }

  async fetch(request) {
    const handled = (await cellRoute(this.state, request)) ?? (await dispatch(this, ROOM_OPS, request));
    if (handled) {
      return handled;
    }
    if (request.headers.get("Upgrade") !== "websocket") {
      return new Response("expected websocket\n", { status: 426 });
    }
    // Nothing awaits between the cap check and the accept, so concurrent joins
    // cannot both take the last place.
    const refused = this.admit(Date.now());
    if (refused) {
      return refused;
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    // acceptWebSocket, not server.accept(): the hibernatable form is what lets
    // the runtime evict the cell while the socket stays open.
    this.state.acceptWebSocket(server);
    // The snapshot goes out in the SAME event that attaches the socket, so no
    // mutation can land between the state read and the attach - the atomicity
    // the multi-pod design had to buy with the seq splice contract.
    server.send(this.snapshotFrame(await this.loadRoom()));
    return new Response(null, { status: 101, webSocket: client });
  }

  // The state is the whole-room GET's object byte for byte: each value's
  // stored text spliced in, keys in Go's order.
  snapshotFrame(doc) {
    const members = doc ? Object.keys(doc.kv).sort(compareCodePoints)
      .map((key) => `${JSON.stringify(key)}:${wireOf(doc, key)}`) : [];
    return `{"type":"snapshot","seq":${doc ? doc.seq : 0},"state":{${members.join(",")}}}`;
  }

  // A join at the cap first closes the sockets whose last ping answer is
  // stale, then is refused with the 429 the client sees in place of the 101
  // if the room is still full. Under the cap nothing is evicted, so a client
  // whose pings are only late keeps its socket. A socket that never pinged has
  // no answer to go stale; it leaves when it closes or a write to it fails.
  // ROOM_PING_STALE_MS in env is a test hook.
  admit(now) {
    if (!this.publicSurface) {
      return null;
    }
    // A socket already closing is on its way out.
    const open = this.state.getWebSockets().filter((ws) => !(ws.readyState >= 2));
    if (open.length < ROOM_SOCKET_CAP) {
      return null;
    }
    const staleAfter = Number(this.env?.ROOM_PING_STALE_MS ?? ROOM_PING_STALE_MS);
    let kept = open.length;
    for (const ws of open) {
      const answered = this.state.getWebSocketAutoResponseTimestamp?.(ws) ?? null;
      if (answered !== null && now - answered.getTime() > staleAfter) {
        try {
          ws.close(ROOM_STALE_CLOSE, "stale");
        } catch {
          // closed already
        }
        kept--;
      }
    }
    if (kept < ROOM_SOCKET_CAP) {
      return null;
    }
    return new Response("room connection limit reached\n", {
      status: 429,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }

  // Direct entry points, same gating as the op table.
  create(body) {
    return runOp(this, ROOM_OPS.create, body);
  }

  put(body) {
    return runOp(this, ROOM_OPS.put, body);
  }

  del(body) {
    return runOp(this, ROOM_OPS.del, body);
  }

  async recordRoomCreated(body) {
    const ledger = await this.appCall(body.appSlug, "roomcreated", {
      id: body.id, subnet: body.subnet, at: body.at,
    });
    return ledger.ok;
  }

  async createBlocked(body) {
    const doc = await this.loadRoom();
    if (doc) {
      if (!await this.recordRoomCreated(body)) {
        return Response.json({ error: "room-ledger-unavailable" }, { status: 502 });
      }
      return Response.json({ created: false });
    }
    if (body.appCap > 0) {
      const preflight = await this.appCall(body.appSlug, "roompreflight", {
        appCap: body.appCap,
      });
      if (preflight.status === 507) {
        return Response.json({ error: "app-full" }, { status: 507 });
      }
      if (!preflight.ok) {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
    }
    if (!await this.recordRoomCreated(body)) {
      return Response.json({ error: "room-ledger-unavailable" }, { status: 502 });
    }
    await this.saveRoom({
      meta: {
        appSlug: body.appSlug, id: body.id,
        createdAt: body.createdAt, updatedAt: body.updatedAt,
      },
      kv: {}, wire: {}, bytes: 0, seq: 0, budgetVersion: 0,
    });
    return Response.json({ created: true });
  }

  getValue(doc, key) {
    if (!Object.hasOwn(doc.kv, key)) {
      return new Response("not found\n", { status: 404 });
    }
    return Response.json({ value: doc.kv[key] });
  }

  async putBlocked(doc, body) {
    if (doc.pending) {
      const recovered = await this.resumePending(doc);
      if (recovered.outcome === "unavailable") {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
      doc = recovered.doc;
    }
    const checked = this.checkPut(doc, body);
    if (checked.error) {
      return checked.error;
    }
    if (checked.next === doc.bytes) {
      return this.commitEqualPut(doc, body, checked.next);
    }
    return this.putBudgeted(doc, body, checked.next);
  }

  checkPut(doc, body) {
    const size = b64len(body.value);
    const prior = Object.hasOwn(doc.kv, body.key) ? b64len(doc.kv[body.key]) : 0;
    const next = doc.bytes - prior + size;
    if (!Object.hasOwn(doc.kv, body.key) && body.keyCap > 0 &&
        Object.keys(doc.kv).length >= body.keyCap) {
      return { error: Response.json({ error: "room-full" }, { status: 413 }) };
    }
    if (body.roomCap > 0 && next > body.roomCap) {
      return { error: Response.json({ error: "room-full" }, { status: 413 }) };
    }
    return { next };
  }

  applyPut(doc, body, next) {
    setOwn(doc.kv, body.key, body.value);
    // A put without its text must not leave the prior value's behind.
    if (typeof body.wire === "string") {
      setOwn(doc.wire, body.key, body.wire);
    } else {
      delete doc.wire[body.key];
    }
    doc.bytes = next;
    doc.seq++;
    doc.meta.updatedAt = body.now;
  }

  async commitEqualPut(doc, body, next) {
    this.applyPut(doc, body, next);
    await this.saveRoom(doc);
    this.broadcastPut(doc, body.key);
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  async putBudgeted(doc, body, targetBytes) {
    const growing = targetBytes > doc.bytes;
    doc.budgetVersion++;
    const pending = { targetBytes, appCap: body.appCap };
    doc.pending = pending;
    if (growing) {
      pending.mutation = {
        key: body.key, value: body.value, wire: body.wire, now: body.now,
      };
    } else {
      this.applyPut(doc, body, targetBytes);
    }
    await this.persistPending(doc);
    if (!growing) {
      this.broadcastPut(doc, body.key);
      await this.resumePending(doc);
      return Response.json({ seq: doc.seq, bytes: doc.bytes });
    }

    const result = await this.resumePending(doc);
    if (result.outcome === "unavailable") {
      return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
    }
    if (result.outcome === "refused") {
      return Response.json({ error: "app-full" }, { status: 507 });
    }
    return Response.json({ seq: result.doc.seq, bytes: result.doc.bytes });
  }

  async delBlocked(doc, body) {
    if (doc.pending) {
      const recovered = await this.resumePending(doc);
      if (recovered.outcome === "unavailable") {
        return Response.json({ error: "app-budget-unavailable" }, { status: 502 });
      }
      doc = recovered.doc;
    }
    if (!Object.hasOwn(doc.kv, body.key)) {
      return this.commitDelete(doc, body);
    }

    const targetBytes = doc.bytes - b64len(doc.kv[body.key]);
    doc.budgetVersion++;
    doc.pending = { targetBytes, appCap: 0 };
    this.applyDelete(doc, body.key, body.now);
    await this.persistPending(doc);
    this.broadcast({ type: "delete", seq: doc.seq, key: body.key });
    await this.resumePending(doc);
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  async commitDelete(doc, body) {
    this.applyDelete(doc, body.key, body.now);
    await this.saveRoom(doc);
    this.broadcast({ type: "delete", seq: doc.seq, key: body.key });
    return Response.json({ seq: doc.seq, bytes: doc.bytes });
  }

  applyDelete(doc, key, now) {
    if (Object.hasOwn(doc.kv, key)) {
      doc.bytes -= b64len(doc.kv[key]);
      delete doc.kv[key];
      delete doc.wire[key];
    }
    doc.seq++;
    doc.meta.updatedAt = now;
  }

  // The room's budget operation, as one of: none, a growth holding the
  // mutation it waits to apply, or a release already applied (docs/SPEC.md
  // "Room storage"). Every change of it persists with the alarm it implies.
  async persistPending(doc, retryAt = Date.now()) {
    doc.pending.retryAt = retryAt;
    await this.state.storage.transaction(async (tx) => {
      await tx.put("state", doc);
      await this.rearm(doc, tx);
    });
  }

  async clearPending(doc) {
    delete doc.pending;
    await this.state.storage.transaction(async (tx) => {
      await tx.put("state", doc);
      await this.rearm(doc, tx);
    });
  }

  async retryPendingLater(doc) {
    await this.persistPending(doc, Date.now() + 1000);
    return "unavailable";
  }

  async resumePending(doc, rearm = false) {
    doc ??= await this.loadRoom();
    if (!doc?.pending) {
      return { outcome: "complete", doc };
    }
    const pending = doc.pending;
    const version = doc.budgetVersion;
    const growing = budgetKind(pending) === "growing";
    if (rearm) {
      await this.persistPending(doc);
    }

    let response;
    let decision;
    try {
      response = await this.appCall(doc.meta.appSlug, "roomdecide", {
        app: doc.meta.appSlug,
        room: doc.meta.id,
        version,
        targetBytes: pending.targetBytes,
        appCap: pending.appCap,
      });
      decision = await response.json();
    } catch {
      await this.retryPendingLater(doc);
      return { outcome: "unavailable", doc };
    }
    if (response.status === 507 && growing &&
        decision.granted === false && decision.version === version) {
      await this.clearPending(doc);
      return { outcome: "refused", doc };
    }
    if (!response.ok || decision.granted !== true ||
        decision.version !== version ||
        decision.allocated !== pending.targetBytes) {
      await this.retryPendingLater(doc);
      return { outcome: "unavailable", doc };
    }

    if (growing) {
      this.applyPut(doc, pending.mutation, pending.targetBytes);
      await this.clearPending(doc);
      this.broadcastPut(doc, pending.mutation.key);
    } else {
      await this.clearPending(doc);
    }
    return { outcome: "complete", doc };
  }

  broadcastPut(doc, key) {
    this.broadcastText(`{"type":"put","seq":${doc.seq},"key":${JSON.stringify(key)},"value":${wireOf(doc, key)}}`);
  }

  // info.now is a test hook; the runtime passes retry metadata only.
  async alarm(info = {}) {
    await this.state.blockConcurrencyWhile(async () => {
      await this.resumePending(undefined, true);
      await this.firePush(info.now ?? Date.now());
    });
  }

  // One alarm serves both the budget retry and the push timer.
  async rearm(doc, tx = this.state.storage) {
    const at = alarmDeadline(doc?.pending, await tx.get(PUSH_KEY));
    if (at === null) {
      await tx.deleteAlarm();
    } else {
      await tx.setAlarm(at);
    }
  }

  async loadPush() {
    return (await this.state.storage.get(PUSH_KEY)) ??
      { subscriptions: [], schedule: null, nextFireAt: null, lastTestAt: 0 };
  }

  // A pending budget already holds the alarm at its retry instant, which the
  // push save must keep; without one the push deadline is the whole alarm.
  async savePush(push) {
    const dues = (push.schedule?.items ?? []).map((item) => item.due);
    push.nextFireAt = dues.length ? Math.min(...dues) : null;
    await this.state.storage.transaction(async (tx) => {
      await tx.put(PUSH_KEY, push);
      const doc = await tx.get("state");
      // A budget operation recorded without its retry instant keeps the one
      // the runtime holds.
      if (doc?.pending && doc.pending.retryAt === undefined) {
        doc.pending.retryAt = (await tx.getAlarm()) ?? Date.now();
      }
      await this.rearm(doc, tx);
    });
  }

  async pushSubPut(body) {
    const checked = validateSubscription(body);
    if (checked.error) {
      return checked.error;
    }
    const push = await this.loadPush();
    const existing = push.subscriptions.find((s) => s.endpoint === checked.subscription.endpoint);
    if (existing) {
      Object.assign(existing, checked.subscription);
    } else {
      if (push.subscriptions.length >= PUSH_MAX_SUBSCRIPTIONS) {
        return Response.json({ error: "too-many-subscriptions" }, { status: 413 });
      }
      push.subscriptions.push({ ...checked.subscription, added: body.now ?? Date.now(), sent: {} });
    }
    if (typeof body.subject === "string") {
      push.subject = body.subject;
    }
    await this.savePush(push);
    return new Response(null, { status: 204 });
  }

  async pushSubDel(body) {
    if (typeof body?.endpoint !== "string") {
      return Response.json({ error: "invalid-endpoint" }, { status: 400 });
    }
    const push = await this.loadPush();
    const kept = push.subscriptions.filter((s) => s.endpoint !== body.endpoint);
    if (kept.length !== push.subscriptions.length) {
      push.subscriptions = kept;
      await this.savePush(push);
    }
    return new Response(null, { status: 204 });
  }

  async pushSubList() {
    const push = await this.loadPush();
    return Response.json({
      subscriptions: push.subscriptions.map((s) => ({
        endpoint: s.endpoint, added: new Date(s.added).toISOString(),
      })),
    });
  }

  async pushSchedulePut(body) {
    const now = body.now ?? Date.now();
    const checked = validateSchedule(body, now);
    if (checked.error) {
      return checked.error;
    }
    const { schedule } = checked;
    for (const item of schedule.items) {
      item.due = nextDue(item, schedule.tz, now);
    }
    const push = await this.loadPush();
    push.schedule = schedule;
    if (typeof body.subject === "string") {
      push.subject = body.subject;
    }
    await this.savePush(push);
    return new Response(null, { status: 204 });
  }

  async pushScheduleGet() {
    const push = await this.loadPush();
    return Response.json({
      tz: push.schedule?.tz ?? "",
      items: (push.schedule?.items ?? []).map(publicItem),
    });
  }

  async pushTest(doc, body) {
    const now = body?.now ?? Date.now();
    const push = await this.loadPush();
    const elapsed = now - (push.lastTestAt ?? 0);
    if (elapsed < PUSH_TEST_INTERVAL_MS) {
      return Response.json({
        error: "rate-limited",
        retryAfter: Math.ceil((PUSH_TEST_INTERVAL_MS - elapsed) / 1000),
      }, { status: 429 });
    }
    push.lastTestAt = now;
    await this.savePush(push);
    const payload = utf8(JSON.stringify({
      title: "Test notification", body: "Push notifications are working.", tag: "hostthis-test",
    }));
    const date = localDate(now, push.schedule?.tz ?? "UTC");
    const outcome = await this.deliver(push, this.dispatch(push, doc, payload, date, new Map(), now));
    await this.savePush(push);
    return Response.json(outcome);
  }

  // Fires every item whose due instant has passed. The schedule advances and
  // persists before any send, so a crash mid-fire loses sends rather than
  // repeating them.
  async firePush(now) {
    const push = await this.loadPush();
    if (!push.schedule?.items.length || push.nextFireAt === null) {
      return;
    }
    if (push.nextFireAt > now) {
      // The runtime consumed the alarm; the deadline must be set again.
      await this.savePush(push);
      return;
    }
    const { tz } = push.schedule;
    const fires = [];
    const kept = [];
    for (const item of push.schedule.items) {
      if (item.due > now) {
        kept.push(item);
        continue;
      }
      if (now - item.due <= PUSH_STALE_MS) {
        fires.push({ item, date: localDate(item.due, tz) });
      }
      if (item.when === undefined) {
        item.due = nextDue(item, tz, now);
        kept.push(item);
      }
    }
    push.schedule.items = kept;
    await this.savePush(push);
    const doc = fires.length ? await this.loadRoom() : null;
    if (!doc) {
      return;
    }
    const signed = new Map();
    const sends = [];
    for (const { item, date } of fires) {
      const payload = this.resolvePayload(item, doc, date);
      if (payload) {
        sends.push(...this.dispatch(push, doc, payload, date, signed, now));
      }
    }
    await this.deliver(push, sends);
    await this.savePush(push);
  }

  // date is the item's local due date in tz.
  resolvePayload(item, doc, date) {
    let body = item.body;
    if (body === undefined) {
      const key = item.bodyKey.replaceAll("{date}", date);
      if (!Object.hasOwn(doc.kv, key)) {
        return null;
      }
      const bytes = unb64u(doc.kv[key]);
      if (!bytes) {
        return null;
      }
      body = new TextDecoder().decode(bytes);
    }
    const payload = utf8(JSON.stringify({ title: item.title, body, url: item.url, tag: item.tag }));
    return payload.length > PUSH_MAX_PAYLOAD ? null : payload;
  }

  // Charges the daily counter of every subscription under the cap and starts
  // one send per charged subscription. The cap is decided here, before any
  // send runs, so concurrent sends never race on it.
  dispatch(push, doc, payload, date, signed, now) {
    const sends = [];
    for (const sub of push.subscriptions) {
      const count = sub.sent?.[date] ?? 0;
      if (count >= PUSH_DAILY_CAP) {
        continue;
      }
      sub.sent = { [date]: count + 1 };
      sends.push(this.sendPush(sub, payload, doc.meta.appSlug, push.subject, signed, now).then((result) => ({ sub, result })));
    }
    return sends;
  }

  // Awaits the sends of one fire together and prunes gone subscriptions.
  // Mutates push; the caller persists.
  async deliver(push, sends) {
    const outcome = { sent: 0, pruned: 0 };
    const gone = new Set();
    for (const settled of await Promise.allSettled(sends)) {
      if (settled.status !== "fulfilled") {
        continue;
      }
      if (settled.value.result === "gone") {
        gone.add(settled.value.sub);
      } else if (settled.value.result === "sent") {
        outcome.sent++;
      }
    }
    outcome.pruned = gone.size;
    push.subscriptions = push.subscriptions.filter((sub) => !gone.has(sub));
    return outcome;
  }

  // signed caches the in-flight signing per origin so concurrent sends to one
  // push service share a token.
  vapidHeader(origin, appSlug, subject, signed) {
    if (!signed.has(origin)) {
      const claims = { aud: origin };
      if (subject) {
        claims.sub = subject;
      }
      signed.set(origin, this.appCall(appSlug, "pushsign", claims).then(async (res) => {
        if (!res.ok) {
          throw new Error("vapid signing unavailable");
        }
        const { token, key } = await res.json();
        return `vapid t=${token}, k=${key}`;
      }));
    }
    return signed.get(origin);
  }

  async sendPush(sub, payload, appSlug, subject, signed, now) {
    try {
      const authorization = await this.vapidHeader(new URL(sub.endpoint).origin, appSlug, subject, signed);
      const body = await encryptPush(sub, payload);
      const res = await (this.env.PUSH_FETCH ?? fetch)(sub.endpoint, {
        method: "POST",
        headers: {
          "Content-Encoding": "aes128gcm",
          "Content-Type": "application/octet-stream",
          TTL: "86400",
          Urgency: "normal",
          Authorization: authorization,
        },
        body,
        redirect: "manual",
        signal: AbortSignal.timeout(this.env.PUSH_SEND_TIMEOUT_MS ?? PUSH_SEND_TIMEOUT_MS),
      });
      await res.body?.cancel();
      if ((res.status === 404 || res.status === 410) && now - sub.added >= PUSH_PRUNE_GRACE_MS) {
        return "gone";
      }
      return res.ok ? "sent" : "failed";
    } catch {
      return "failed";
    }
  }

  // A runtime that does not reply to a peer's close frame leaves the socket
  // closing until the handler closes it too.
  async webSocketClose(ws, code, reason) {
    answerClose(ws, code, reason);
  }

  async webSocketError(ws) {
    answerClose(ws);
  }

  async webSocketMessage(ws, message) {
    if (this.publicSurface && overFrameLimit(message)) {
      try {
        ws.close(1009, "frame too large");
      } catch {
        // closed already
      }
      return;
    }
    if (typeof message === "string") {
      try {
        const frame = JSON.parse(message);
        if (frame !== null && typeof frame === "object" &&
            ["snapshot", "put", "delete"].includes(frame.type)) {
          ws.close(1008, "reserved control frame");
          return;
        }
      } catch {
        // Opaque text is an ephemeral frame, not necessarily JSON.
      }
    }
    for (const peer of this.state.getWebSockets()) {
      if (peer === ws) {
        continue;
      }
      try {
        peer.send(message);
      } catch {
        // reaped by the runtime
      }
    }
  }
}

// The Worker serves the public surface once it has an apex.
function publicSurfaceOn(env) {
  return normalizeHost(env?.HOSTTHIS_APEX_DOMAIN ?? "") !== "";
}

// Every UTF-16 unit is one to three bytes of UTF-8, so only a frame between
// the two bounds needs encoding to be measured.
function overFrameLimit(message) {
  if (typeof message !== "string") {
    return message.byteLength > ROOM_CLIENT_FRAME_LIMIT;
  }
  if (message.length > ROOM_CLIENT_FRAME_LIMIT) {
    return true;
  }
  return message.length * 3 > ROOM_CLIENT_FRAME_LIMIT && utf8(message).length > ROOM_CLIENT_FRAME_LIMIT;
}

// 1000 and application codes (3000-4999) are the close() codes every runtime
// accepts, so any other code, and the 1005 a codeless close reports, is
// answered with 1000. close() on a socket the runtime already closed throws.
function answerClose(ws, code, reason) {
  try {
    if (code === 1000 || (code >= 3000 && code <= 4999)) {
      ws.close(code, reason);
    } else {
      ws.close(1000);
    }
  } catch {
    // closed already
  }
}

// The JSON text every reader gets for a value: the text stored with it, so
// the frames and both servers' whole-room GET carry the same bytes. A value
// stored without text, or with text that is not JSON, is encoded from its
// bytes rather than breaking the room for every reader.
function wireOf(doc, key) {
  const text = Object.hasOwn(doc.wire, key) ? doc.wire[key] : undefined;
  if (typeof text === "string") {
    try {
      JSON.parse(text);
      return text;
    } catch {
      // not JSON: encoded below
    }
  }
  return roomWireValue(base64Bytes(doc.kv[key]));
}

function wireTexts(doc) {
  const texts = {};
  for (const key of Object.keys(doc.kv)) {
    setOwn(texts, key, wireOf(doc, key));
  }
  return texts;
}

// Decoded length of a base64 payload, without decoding it. Room values are
// arbitrary bytes and travel base64-encoded, but every cap is stated in the
// app's OWN bytes, so the encoded length would charge 4/3 of the truth.
function b64len(b64) {
  if (!b64) {
    return 0;
  }
  let pad = 0;
  if (b64.endsWith("==")) {
    pad = 2;
  } else if (b64.endsWith("=")) {
    pad = 1;
  }
  return (b64.length * 3) / 4 - pad;
}
