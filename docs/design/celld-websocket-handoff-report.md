# celld: a hibernatable WebSocket does not survive its cell moving, and the client gets no close frame

Standalone so it can be filed later without re-running anything. **Not sent
anywhere.** celld disables pull requests and asks for a `git format-patch` by
email to the maintainer, so anything outbound is direct contact with a person
and that is the operator's decision, not ours.

## Summary

When a cell moves to another node, a client holding a hibernatable (inbound)
WebSocket to that cell has its connection dropped at the TCP level with **no
WebSocket close frame**. The client observes an EOF reading the next frame
header, not a close status.

This happens even when the node the client is connected THROUGH stays healthy
and continues serving other traffic. Reachability through the signed peer tunnel
is therefore not connection survival across a handoff.

The limitations page already says test coverage for close codes and
reconnections across nodes is thinner than for a single node, so this is offered
as a concrete instance rather than a surprise.

## Version

    ghcr.io/denoland/celld@sha256:f47d97c2980aa98aef1d9c42205a313442f48acb606c5987dbb9b32983a23aaf

celld v0.3.0, linux/arm64, k3s, MinIO as the fleet bucket.

## What was observed

    status=StatusCode(-1)
    err=failed to get reader: failed to read frame header: EOF

`StatusCode(-1)` is the coder/websocket sentinel for "connection ended without a
close frame".

## Reproduction, with the control that makes it attributable

A Durable Object accepting hibernatable sockets:

```js
export class Room {
  async fetch(request) {
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    this.state.acceptWebSocket(server);          // hibernatable, not server.accept()
    return new Response(null, { status: 101, webSocket: client });
  }
  async webSocketMessage(ws, message) { ws.send(String(message)); }
}
```

Then, on a fleet of **two** nodes:

1. Connect a client to the room **through node A**, and confirm it echoes.
2. Delete **node B**.
3. Confirm **node A is still serving** (`GET /healthz` returns 200 after the
   deletion).
4. The client's socket ends with the EOF above.

**Step 3 is the point.** The obvious version of this test - port-forward to a
node, kill that node, observe EOF - cannot distinguish celld closing the socket
from the tunnel dying with the node. Two such readings were taken first and
discarded as unusable. Killing a node the client is NOT connected through, and
proving the client's own node survived, is what makes the observation
attributable to the cell move.

The same outcome occurs on a single-node fleet during a rolling restart, but
that case is not attributable for the same reason, so the two-node form is the
one to trust.

## Why it matters to a consumer

A client cannot distinguish "the cell moved, reconnect" from "the network
broke": there is no close code to branch on. Any reconnect policy keyed on a
status such as 1001 or 1012 will silently never fire.

The safe consumer behaviour is to reconnect unconditionally on any disconnect
with backoff. That is what we do, so this costs us nothing, but it is only
correct by accident unless it is documented.

## Suggestion

Either send a close frame with a defined status when a cell is handed over, so
clients can tell a move from a failure, or state in the limitations page that a
hibernatable socket ends without a close frame when its cell moves and that
clients must reconnect unconditionally. The documentation change alone would be
enough; the current text says the coverage is thin, which reads as "may be
buggy" rather than "this is the defined behaviour".

## Not claimed

Single fleet, small cells, one client per room. No claim about behaviour under
load, about outbound Durable Object sockets (documented as not surviving a move
anyway), or about whether the drop is intended.
