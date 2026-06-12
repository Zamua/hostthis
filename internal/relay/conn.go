// Package relay is the real-time room relay bounded context (see SPEC.md
// "Real-time room relay (WebSocket)"). It is a generic, per-room
// broadcast channel: a message from one client in a room is fanned out
// verbatim to every OTHER client in that same room. hostthis runs no
// app-specific server logic here - the clients hold all the app logic and
// the relay is the live wire plus, via the room KV, the durable backing
// store.
//
// The package is split so the hub logic (register, broadcast,
// drop-a-laggard, reap, teardown) is unit-testable WITHOUT real sockets:
//
//   - Conn is the connection abstraction the hub talks to (send a frame,
//     close, identity). A fake Conn drives the hub in tests.
//   - Hub is the in-memory registry + broadcast fan-out for ONE room. It
//     is concurrency-safe and depends on nothing but Conn and a clock.
//   - Registry maps a RoomKey to its live Hub and owns the per-app + total
//     caps. It is the only structure the upgrade handler touches.
//   - Relay wires a real coder/websocket connection into the Hub: it owns
//     the per-connection reader, writer, and heartbeat goroutines and the
//     snapshot-then-stream join.
package relay

import "github.com/Zamua/hostthis/internal/domain"

// RoomKey identifies one relay hub. The outermost segment is the app
// slug, so an identical room-UUID-shaped string under a different app
// resolves to a DIFFERENT key and therefore a different hub: one app's
// live traffic is disjoint from another's structurally, not by a filter a
// handler could forget. This mirrors the (app-slug, room-uuid, key) shape
// the durable KV tier namespaces by.
type RoomKey struct {
	App domain.Slug
	ID  domain.RoomID
}

// Frame is one relay message: opaque bytes plus the WebSocket message
// type (text vs binary) so a frame round-trips with the flavor the sender
// chose. hostthis never parses Data - it is the app's opaque payload,
// fanned out verbatim. A frame is also used for the control envelopes the
// server itself originates (the late-join snapshot, the live mirror of a
// durable PUT); those are JSON text frames the client distinguishes by
// their own structure, never by anything hostthis stamps onto Data here.
type Frame struct {
	// Binary is true for a WebSocket binary message, false for text. The
	// hub does not interpret either; it carries the flag through so the
	// receiving socket re-emits the same type the sender used.
	Binary bool
	// Data is the opaque payload, fanned out verbatim.
	Data []byte
}

// Conn is the per-client connection abstraction the Hub broadcasts to. It
// is deliberately tiny so the hub logic is testable against a fake that
// records what it received and can be made to fill its buffer to exercise
// the backpressure path. The real adapter wraps a coder/websocket
// connection (see Relay).
//
// The contract:
//
//   - Send enqueues f on the connection's bounded send buffer and returns
//     immediately. It NEVER blocks the caller (the broadcast path): if the
//     buffer is full, Send returns false and the hub drops this laggard
//     rather than head-of-line-blocking the whole room. A successful
//     enqueue returns true.
//   - Close tears the connection down (closes the socket, stops its
//     goroutines). It is idempotent and safe to call from the hub while the
//     connection's own writer is draining.
//   - ID is a stable per-connection identity used only as the hub's map
//     key, so two connections from the same client (two tabs) are distinct
//     hub members. It is unique for the connection's lifetime.
type Conn interface {
	Send(f Frame) (ok bool)
	Close()
	ID() uint64
}
