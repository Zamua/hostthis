package http

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
)

// wsKey is the reserved trailing segment that turns a room path into the
// real-time relay upgrade: GET (Upgrade: websocket) /api/rooms/<uuid>/ws.
// It is the one key the KV verbs do not serve as data (a key path is
// /api/rooms/<uuid>/<key>; the relay claims <key> == "ws"), so the relay
// shares the room's path space without colliding with a stored value.
const wsKey = "ws"

// RoomRelay is the relay surface the HTTP layer needs to stand up a
// WebSocket connection for a room. internal/relay.Relay satisfies it.
// Optional: when nil, the /ws path 404s (no relay wired on this backend).
type RoomRelay interface {
	// Admit reserves a connection slot under the per-room / per-app /
	// total-rooms caps, returning a relay-layer admission error
	// (relay.ErrRoomFull / ErrAppFull / ErrTooManyRooms) the HTTP layer
	// maps to a status. Called BEFORE the websocket handshake.
	Admit(key relay.RoomKey) (*relay.Hub, uint64, error)
	// Serve runs the accepted connection's full lifecycle (snapshot,
	// stream, heartbeat) and blocks until it ends.
	Serve(ctx context.Context, key relay.RoomKey, id uint64, ws *websocket.Conn)
	// Release frees a slot reserved by Admit but never handed to Serve (the
	// websocket Accept failed after admission).
	Release(key relay.RoomKey, id uint64)
	// CommitAndMirror runs a durable write's KV commit (with no relay lock
	// held - a slow commit never stalls the room), then broadcasts its
	// live mirror locally and publishes it to the peer pods. The HTTP
	// PUT/DELETE handlers pass a commit callback that performs the durable
	// write and returns the mirror frame - built AFTER the write so it
	// carries the per-room sequence the commit assigned, which is what
	// orders / de-duplicates it at every subscriber.
	CommitAndMirror(key relay.RoomKey, commit func() (relay.Frame, error)) error
}

// handleRoomWS performs the WebSocket upgrade for a room's relay. It is
// reached for the reserved /<uuid>/ws path with id already parsed. It
// validates (in order, refusing with a normal HTTP status BEFORE any 101
// so a client's open fails cleanly):
//
//   - the app slug names a LIVE app (the same appExists gate room creation
//     rides) - else 404
//   - the room EXISTS - else 404 (a relay to a never-created room has
//     nothing to back its late-join snapshot)
//   - the connection caps admit it - else 429 (room/app full) or 503
//     (too many active relay rooms)
//   - the request is a real Upgrade with an allowed Origin (enforced by
//     websocket.Accept) - else Accept writes a 400/426
//
// Only after all of these does it accept the socket and hand it to the
// relay. No hub is ever reached by a request that fails validation. (A
// malformed UUID is a 400 handled by the caller's ParseRoomID, before
// this is reached.)
func (s *Server) handleRoomWS(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID) {
	if s.Relay == nil || s.Rooms == nil {
		http.NotFound(w, r)
		return
	}

	// 1. The app slug must name a LIVE app - the same existence gate room
	// creation rides, so the relay cannot be opened under one of the ~10^12
	// well-formed-but-empty slugs (an unknown slug 404s, the
	// existence-not-leaked shape).
	if !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}

	// 2. The room must EXIST. A scan of a nonexistent room is a 404 (the KV
	// path's existence-not-leaked shape); a relay to a room that was never
	// created has nothing to back its late-join snapshot. Any error here
	// (not-found OR a backend read error) refuses the upgrade as a 404 - we
	// do not stand a relay up for a room we cannot confirm exists, and we
	// do not leak which it was.
	if _, err := s.Rooms.Scan(appSlug, id); err != nil {
		http.NotFound(w, r)
		return
	}

	key := relay.RoomKey{App: appSlug, ID: id}

	// 3. Enforce the connection caps BEFORE the handshake, so an over-limit
	// upgrade is refused with a normal HTTP status and no socket is ever
	// accepted for it.
	_, connID, err := s.Relay.Admit(key)
	if err != nil {
		writeRelayAdmitError(w, err)
		return
	}

	// 4. Accept the websocket. coder/websocket's Accept rejects a
	// non-Upgrade request (writes a 426/400) and enforces the same-origin
	// Origin policy (a cross-origin upgrade a browser would gate by CORS is
	// refused). On failure it has already written the response; release the
	// reserved slot and return so we do not leak a connection count.
	conn, err := websocket.Accept(w, r, wsAcceptOptions(s.ApexDomain))
	if err != nil {
		s.Relay.Release(key, connID)
		return
	}

	// 5. Hand the live connection to the relay. Serve blocks for the
	// connection's lifetime (the websocket is hijacked out from under the
	// http.Server's ReadTimeout / WriteTimeout, which must NOT reap a live
	// relay connection; the relay manages its own per-connection deadlines
	// via the heartbeat). r.Context() is cancelled when the underlying
	// connection drops, so it bounds the connection's life.
	s.Relay.Serve(r.Context(), key, connID, conn)
}

// wsAcceptOptions builds the coder/websocket Accept options. The relay is
// a same-origin app API: the only authorized Origin is the app's own
// subdomain under the apex, plus the apex itself for path-mode dev. When
// no apex is configured (some tests), origin verification is disabled so
// the test harness's localhost origin is accepted.
func wsAcceptOptions(apexDomain string) *websocket.AcceptOptions {
	if apexDomain == "" {
		return &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	// "*."+apex matches any app subdomain; apex itself matches path mode.
	return &websocket.AcceptOptions{
		OriginPatterns: []string{"*." + apexDomain, apexDomain},
	}
}

// writeRelayAdmitError maps the relay admission sentinels to status codes:
// a room / app connection cap is 429; the service-wide live-room cap is
// 503 (an upgrade that would create a NEW hub past the cap; joins to live
// rooms still succeed).
func writeRelayAdmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, relay.ErrRoomFull), errors.Is(err, relay.ErrAppFull):
		http.Error(w, "room connection limit reached\n", http.StatusTooManyRequests)
	case errors.Is(err, relay.ErrTooManyRooms):
		http.Error(w, "relay at capacity\n", http.StatusServiceUnavailable)
	default:
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}
