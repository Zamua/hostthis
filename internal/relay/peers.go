package relay

// The multi-pod tier's outbound port (see SPEC.md "Multi-pod relay: the peer
// transport"). The multi-pod machinery IS the peer set being non-empty, not a
// mode flag: a single-pod deploy wires no publisher and gets the single-pod
// relay exactly.

// PeerPublisher publishes a frame to every OTHER pod of the deployment, which
// delivers it to its own local subscribers of the same room (via
// Relay.DeliverFromPeer). The relay is the ONLY caller; implementations own
// transport, discovery, and queueing.
//
// The contract (SPEC "Delivery semantics"):
//
//   - Publish MUST NOT block and MUST NOT fail the caller: delivery is
//     best-effort per peer, isolated per peer, and never on the commit path. A
//     production implementation enqueues on a bounded per-peer queue and drops
//     when it is full. A dropped durable frame is detectable at every affected
//     subscriber via the dense seq (the client re-snapshots); a dropped
//     ephemeral frame is harmless by definition.
//   - The recipient list is the implementation's concern, so interest-based
//     fan-out can replace "all peers" later with no contract change.
//   - Publish carries BOTH flavors, durable mirror and ephemeral client frame.
//     The frame is opaque to the transport: a durable mirror's seq rides inside
//     f.Data, an ephemeral frame has none.
type PeerPublisher interface {
	Publish(key RoomKey, f Frame)
}

// Peers is the peer-discovery port: the CURRENT peer pods' gRPC addresses,
// self excluded (SPEC "Peer discovery: the ring membership the cluster already
// gossips"). Addresses is called per publish, so the peer set follows
// membership with no subscription machinery; implementations must be safe for
// concurrent use.
type Peers interface {
	Addresses() []string
}

// SetPeerPublisher wires the outbound peer port. Called once at the
// composition root, before the HTTP server accepts connections; nil (the
// default) is the zero-peer deploy. A setter rather than a constructor
// argument because the relay and the transport are constructed in an order the
// composition root owns.
func (rl *Relay) SetPeerPublisher(p PeerPublisher) { rl.peers = p }

// DeliverFromPeer is the receive path of the peer transport: the transport
// adapter calls it for every frame published by a peer pod. The frame is
// broadcast to THIS pod's local subscribers as server-originated (from == 0,
// so every local connection receives it; the originating socket lives on the
// origin pod, which already excluded it from its own fan-out). No local hub
// means no local subscribers and the frame is dropped, which is correct
// because the live path never carries correctness (the durable KV + the dense
// seq do). A received frame is delivered locally ONLY, never re-published, so
// the origin pod is the single fan-out point (full mesh, TTL 1) and no routing
// loop can exist by construction.
func (rl *Relay) DeliverFromPeer(key RoomKey, f Frame) {
	hub := rl.reg.hub(key)
	if hub == nil {
		return
	}
	hub.broadcast(0, f)
}

// publishToPeers fans f out to the peer pods when a publisher is wired. The
// nil check IS the zero-peer degenerate case.
func (rl *Relay) publishToPeers(key RoomKey, f Frame) {
	if rl.peers != nil {
		rl.peers.Publish(key, f)
	}
}
