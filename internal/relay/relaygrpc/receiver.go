// Package relaygrpc is the relay's peer-transport adapter (see
// docs/SPEC.md "Multi-pod relay: the peer transport"): the gRPC service
// every pod registers on the cluster-internal server shale forwarding
// already runs (the Receiver), and the dialing PeerPublisher that fans a
// frame out to every peer over bounded per-peer queues (the Publisher).
// The core relay package stays transport-free: it consumes the two small
// ports (relay.Peers, relay.PeerPublisher) this package implements.
package relaygrpc

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
)

// DeliverFunc is the receiver's local-delivery target: the relay's
// DeliverFromPeer, which broadcasts the frame to THIS pod's local
// subscribers of the room.
type DeliverFunc func(key relay.RoomKey, f relay.Frame)

// Receiver is the RoomRelay service implementation: it accepts a peer
// pod's Publish and hands the frame to the local relay. Its delivery
// target is LATE-BOUND (SPEC "The peer transport -> Listener"): the
// receiver is constructed BEFORE the storage repo (whose config carries
// the registrar hook that puts this service on the shale gRPC server),
// while the relay is constructed after - so Bind wires the hook once the
// relay exists, and a frame arriving before wiring completes (a boot
// race) is dropped, correct because no client can be connected before the
// HTTP server is up.
type Receiver struct {
	UnimplementedRoomRelayServer

	// maxFrameBytes is the defense-in-depth re-check of the per-frame size
	// cap (SPEC "Trust boundary"): every abuse limit is enforced at the
	// ORIGIN pod against the client socket before any peer fan-out, so
	// peer input is trusted like shale's own forwarded writes are - this
	// is the cheap belt on top. <= 0 disables the check.
	maxFrameBytes int64

	deliver atomic.Pointer[DeliverFunc]
}

// NewReceiver builds a receiver enforcing maxFrameBytes on arriving
// frames (pass the relay's Limits.MaxMessageBytes).
func NewReceiver(maxFrameBytes int64) *Receiver {
	return &Receiver{maxFrameBytes: maxFrameBytes}
}

// Bind late-binds the local delivery target (the relay's DeliverFromPeer).
// Called once at the composition root after the relay is constructed.
func (r *Receiver) Bind(deliver DeliverFunc) {
	r.deliver.Store(&deliver)
}

// Register puts the RoomRelay service on g. It is the opaque
// func(*grpc.Server) hook the composition root passes into the storage
// config, so the relay service rides the same cluster-internal server +
// advertised address shale forwarding uses; storage stays relay-agnostic.
func (r *Receiver) Register(g *grpc.Server) {
	RegisterRoomRelayServer(g, r)
}

// Publish is the receive path of the peer transport: broadcast the frame
// to this pod's local subscribers of the room, server-originated. A frame
// arriving before Bind ran (the boot race) is dropped; a frame over the
// size cap is refused (defense in depth). Delivery is local ONLY - never
// re-forwarded - so the origin pod stays the single fan-out point.
func (r *Receiver) Publish(_ context.Context, req *PublishRequest) (*PublishResponse, error) {
	if r.maxFrameBytes > 0 && int64(len(req.GetData())) > r.maxFrameBytes {
		return nil, status.Errorf(codes.InvalidArgument, "frame of %d bytes exceeds the %d-byte cap", len(req.GetData()), r.maxFrameBytes)
	}
	d := r.deliver.Load()
	if d == nil {
		// Boot race: the relay is not wired yet, so this pod has no
		// subscribers. Dropping is correct - the live path never carries
		// correctness (the durable KV + the dense seq do).
		return &PublishResponse{}, nil
	}
	key := relay.RoomKey{App: domain.Slug(req.GetAppSlug()), ID: domain.RoomID(req.GetRoomId())}
	(*d)(key, relay.Frame{Binary: req.GetBinary(), Data: req.GetData()})
	return &PublishResponse{}, nil
}
