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

// Heartbeat timing: the server pings each client on PingInterval and reaps it
// if the pong misses PingTimeout. PingInterval must stay UNDER the proxy idle
// defaults (traefik / nginx 60-120 s) so the heartbeat also keeps a
// legitimately-quiet connection alive through the proxy.
const (
	PingInterval = 20 * time.Second
	PingTimeout  = 10 * time.Second

	MaxClientFrameBytes int64 = 32 << 10
	MaxServerFrameBytes int64 = 2 << 20
)

// Per-pod connection caps: they bound one pod's resource use, not a room's
// global audience.
const (
	DefaultMaxConnsPerRoom = 64
	DefaultMaxConnsPerApp  = 1024
)

// Admission errors mapped by the HTTP upgrade handler.
var (
	ErrRoomFull      = errors.New("relay: room connection cap reached")
	ErrAppFull       = errors.New("relay: app connection cap reached")
	ErrRelayDraining = errors.New("relay: draining")
)
