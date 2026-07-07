package relay

// The multi-pod tier's outbound port (see SPEC.md "Multi-pod relay: the
// peer transport"). A single-pod deploy has no publisher wired (nil), and
// the relay is exactly the single-pod relay - the multi-pod machinery IS
// the peer set being non-empty, not a mode flag.

// PeerPublisher publishes a frame to every OTHER pod of the deployment,
// which delivers it to its own local subscribers of the same room
// (Relay.DeliverFromPeer on the receiving side). The relay is the ONLY
// caller; implementations own transport, discovery, and queueing.
//
// The contract (SPEC "Delivery semantics"):
//
//   - Publish MUST NOT block and MUST NOT fail the caller: delivery is
//     best-effort per peer, isolated per peer, and never on the commit
//     path. A production implementation enqueues on a bounded per-peer
//     queue (a full queue drops the frame); a dropped durable frame is
//     detectable at every affected subscriber via the dense seq (the
//     client re-snapshots), and a dropped ephemeral frame is harmless by
//     definition.
//   - The recipient list is the implementation's concern, behind this
//     port, so interest-based fan-out (publish a room's frames only to
//     pods with live subscribers to that room) can replace "all peers"
//     later as a pure optimization with no contract change.
//   - Publish carries BOTH flavors: a durable mirror (published by
//     CommitAndMirror after its commit) and an ephemeral client frame
//     (published by the read loop as it broadcasts locally). The frame is
//     opaque to the transport - a durable mirror's seq rides inside
//     f.Data, an ephemeral frame has none.
type PeerPublisher interface {
	Publish(key RoomKey, f Frame)
}

// SetPeerPublisher wires the outbound peer port. Called once at the
// composition root, before the HTTP server accepts connections; nil (the
// default) is the zero-peer deploy. Late-bound as a setter because the
// relay and the transport are constructed in an order the composition
// root owns.
func (rl *Relay) SetPeerPublisher(p PeerPublisher) { rl.peers = p }

// DeliverFromPeer is the receive path of the peer transport: the transport
// adapter calls it for every frame published by a peer pod. The frame is
// broadcast to THIS pod's local subscribers of the room as
// server-originated (from == 0: every local connection receives it; the
// originating socket, if any, lives on the origin pod, which already
// excluded it from its own local fan-out). No local hub means no local
// subscribers: the frame is dropped, which is correct because the live
// path never carries correctness (the durable KV + the dense seq do). A
// received frame is delivered locally ONLY - never re-published to peers -
// so the origin pod is the single fan-out point (full mesh, TTL 1) and no
// routing loop can exist by construction.
func (rl *Relay) DeliverFromPeer(key RoomKey, f Frame) {
	hub := rl.reg.hub(key)
	if hub == nil {
		return
	}
	hub.broadcast(0, f)
}

// publishToPeers fans f out to the peer pods when a publisher is wired.
// The nil check IS the zero-peer degenerate case: no publisher, no peer
// work, the single-pod relay unchanged.
func (rl *Relay) publishToPeers(key RoomKey, f Frame) {
	if rl.peers != nil {
		rl.peers.Publish(key, f)
	}
}
