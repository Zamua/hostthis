import { cellRoute } from "./image.js";

// Deprecated, retained only so the v1 migration that created it stays
// resolvable. Superseded by Identity, which co-locates intents with the quota
// and index they must agree with. Nothing routes here.
export class IntentLog {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    return (await cellRoute(this.state, request))
      ?? new Response("IntentLog is superseded by Identity\n", { status: 410 });
  }
}
