// Package roomwire is the shared vocabulary of the real-time room surface: the
// identifiers, frame shape, cap sentinels and heartbeat timing that every
// implementation of the room relay speaks.
//
// A leaf on purpose. Two implementations exist - the in-process hub relay and
// the celld cell proxy - and they must agree on exactly these values without
// either importing the other's machinery. It depends on domain and nothing
// else, so any layer may reach it.
package roomwire

import (
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomKey identifies one room's real-time scope. Including the app slug makes
// cross-app isolation structural: the same room UUID under a different app
// resolves to a different scope, with no filter a handler could forget.
type RoomKey struct {
	App domain.Slug
	ID  domain.RoomID
}

// Frame is one relay message: opaque bytes plus the WebSocket message type, so
// a frame round-trips with the flavor the sender chose. The server never
// parses or stamps Data.
type Frame struct {
	// Binary is carried through uninterpreted so the receiving socket
	// re-emits the message type the sender used.
	Binary bool
	// Data is the app's payload, fanned out verbatim.
	Data []byte
}

// Heartbeat timing: the server pings each client on PingInterval and reaps it
// if the pong misses PingTimeout. PingInterval must stay UNDER the proxy idle
// defaults (traefik / nginx 60-120 s) so the heartbeat also keeps a
// legitimately-quiet connection alive through the proxy.
const (
	PingInterval = 20 * time.Second
	PingTimeout  = 10 * time.Second
)

// Per-pod connection caps. They bound one pod's resource use, not a room's
// global audience, and both implementations enforce the same numbers so a
// deploy cannot change limits by swapping backends.
const (
	DefaultMaxConnsPerRoom = 64
	DefaultMaxConnsPerApp  = 1024
)

// Admission errors the upgrade handler maps to HTTP status codes.
var (
	// ErrRoomFull: the per-room connection cap is hit. HTTP 429.
	ErrRoomFull = errors.New("relay: room connection cap reached")
	// ErrAppFull: the per-app aggregate connection cap is hit. HTTP 429.
	ErrAppFull = errors.New("relay: app connection cap reached")
	// ErrTooManyRooms: the live-room cap is hit and this upgrade would create
	// a NEW room scope. HTTP 503; joins to already-live rooms still succeed.
	ErrTooManyRooms = errors.New("relay: too many active relay rooms")
)
