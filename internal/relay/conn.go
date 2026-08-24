// Package relay is the real-time room relay (SPEC.md "Real-time room relay
// (WebSocket)"): a generic per-room broadcast channel that fans one client's
// message out verbatim to every OTHER client in that room. No app-specific
// server logic lives here; clients hold the app logic and the relay is the live
// wire over the durable room KV.
//
// Conn abstracts the socket so the hub logic (register, broadcast,
// drop-a-laggard, reap, teardown) is testable without real sockets.
package relay

import "github.com/Zamua/hostthis/internal/roomwire"

// RoomKey and Frame are the shared room vocabulary, defined in roomwire so the
// celld proxy can speak them without importing this package's hub machinery.
type RoomKey = roomwire.RoomKey

// Frame aliases the shared frame shape; see roomwire.Frame.
type Frame = roomwire.Frame

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
