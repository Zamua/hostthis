// Package relay is the real-time room relay (SPEC.md "Real-time room relay
// (WebSocket)"): a generic per-room broadcast channel that fans one client's
// message out verbatim to every OTHER client in that room. No app-specific
// server logic lives here; clients hold the app logic and the relay is the live
// wire over the durable room KV.
//
// Conn abstracts the socket so the hub logic (register, broadcast,
// drop-a-laggard, reap, teardown) is testable without real sockets.
package relay

import "github.com/Zamua/hostthis/internal/domain"

// RoomKey identifies one relay hub. Including the app slug makes cross-app
// isolation structural: the same room UUID under a different app resolves to a
// different hub, with no filter a handler could forget. Mirrors the
// (app-slug, room-uuid, key) shape the durable KV tier namespaces by.
type RoomKey struct {
	App domain.Slug
	ID  domain.RoomID
}

// Frame is one relay message: opaque bytes plus the WebSocket message type, so
// a frame round-trips with the flavor the sender chose. The server never parses
// or stamps Data. Server-originated control envelopes (the late-join snapshot,
// the live mirror of a durable PUT) are JSON text frames the client
// distinguishes by their own structure.
type Frame struct {
	// Binary is carried through uninterpreted so the receiving socket
	// re-emits the message type the sender used.
	Binary bool
	// Data is the app's payload, fanned out verbatim.
	Data []byte
}

// Conn is the per-client connection the Hub broadcasts to. Implementations
// must honour:
//
//   - Send enqueues on a bounded buffer and NEVER blocks the broadcast path.
//     False means the buffer is full, and the hub drops that laggard rather
//     than head-of-line-blocking the whole room.
//   - Close is idempotent and safe to call from the hub while the connection's
//     own writer is draining.
//   - ID is unique per connection, not per client: two tabs are two members.
type Conn interface {
	Send(f Frame) (ok bool)
	Close()
	ID() uint64
}
