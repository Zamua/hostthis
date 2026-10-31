import { cellRoute } from "./image.js";
import { dispatch, mutation } from "./ops.js";

// A SUBNET cell: the Sybil admission rows for one network.
//
// The subnet is the rate-limit unit, so it is the cell. Mutating and pruning
// decisions run under blockConcurrencyWhile because v0.4 may serve several
// fetches concurrently.
const SUBNET_OPS = {
  admit: mutation((self, body) => self.admit(body)),
  snapshot: mutation((self, _body, url) => self.snapshot(url), { body: false }),
};

export class Subnet {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? (await dispatch(this, SUBNET_OPS, request))
      ?? new Response("unknown op\n", { status: 404 });
  }

  // Rows outside the window are dropped as they are walked past: nothing reads
  // them again, because no admission decision can turn on a row the window has
  // already excluded.
  async live(now, windowMs) {
    const rows = (await this.state.storage.get("rows")) ?? {};
    let changed = false;
    for (const [id, firstSeen] of Object.entries(rows)) {
      if (now - firstSeen >= windowMs) {
        delete rows[id];
        changed = true;
      }
    }
    if (changed) {
      await this.state.storage.put("rows", rows);
    }
    return rows;
  }

  async admit(body) {
    const rows = await this.live(body.now, body.window);
    if (Object.hasOwn(rows, body.identity)) {
      // Already on file: no rate-limit accounting, and the first-seen stamp
      // must not move or a returning key would refresh its own window.
      return Response.json({ knownAlready: true, admitted: true });
    }
    if (Object.keys(rows).length >= body.limit) {
      return Response.json({ knownAlready: false, admitted: false });
    }
    rows[body.identity] = body.now;
    await this.state.storage.put("rows", rows);
    return Response.json({ knownAlready: false, admitted: true });
  }

  async snapshot(url) {
    const rows = await this.live(Number(url.searchParams.get("now")),
      Number(url.searchParams.get("window")));
    const stamps = Object.values(rows);
    return Response.json({
      freshCount: stamps.length,
      oldestFirstSeen: stamps.length ? Math.min(...stamps) : 0,
    });
  }
}
