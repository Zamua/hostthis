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
// real-time relay upgrade: GET (Upgrade: websocket) /api/rooms/<uuid>/ws. It
// is the one key the KV verbs do not serve as data, so the relay shares the
// room's path space without colliding with a stored value.
const wsKey = "ws"

// RoomRelay is the relay surface the HTTP layer needs to stand up a WebSocket
// connection for a room. internal/relay.Relay satisfies it. Optional: when
// nil, the /ws path 404s.
type RoomRelay interface {
	// Admit reserves a connection slot under the per-room / per-app /
	// total-rooms caps, returning relay.ErrRoomFull / ErrAppFull /
	// ErrTooManyRooms for the HTTP layer to map to a status. Called BEFORE
	// the websocket handshake.
	Admit(key relay.RoomKey) (*relay.Hub, uint64, error)
	// Serve runs the accepted connection's lifecycle (snapshot, stream,
	// heartbeat) and blocks until it ends.
	Serve(ctx context.Context, key relay.RoomKey, id uint64, ws *websocket.Conn)
	// Release frees a slot reserved by Admit but never handed to Serve.
	Release(key relay.RoomKey, id uint64)
	// CommitAndMirror runs a durable write's KV commit with no relay lock
	// held, so a slow commit never stalls the room, then broadcasts the live
	// mirror locally and publishes it to the peer pods. The commit callback
	// must build the mirror frame AFTER the write so it carries the per-room
	// sequence the commit assigned, which is what orders and de-duplicates it
	// at every subscriber.
	CommitAndMirror(key relay.RoomKey, commit func() (relay.Frame, error)) error
}

// handleRoomWS performs the WebSocket upgrade for a room's relay, reached for
// the reserved /<uuid>/ws path with id already parsed. Every check refuses
// with a normal HTTP status BEFORE any 101, so a client's open fails cleanly
// and no hub is reached by an invalid request:
//
//   - the app slug names a LIVE app, else 404
//   - the room EXISTS, else 404: a relay to a never-created room has nothing
//     to back its late-join snapshot
//   - the connection caps admit it, else 429 (room/app full) or 503 (too many
//     active relay rooms)
//   - the request is a real Upgrade with an allowed Origin, enforced by
//     websocket.Accept, which writes its own 400/426
func (s *Server) handleRoomWS(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID) {
	if s.Relay == nil || s.Rooms == nil {
		http.NotFound(w, r)
		return
	}

	// The same existence gate room creation rides, so a relay cannot be opened
	// under one of the well-formed-but-empty slugs.
	if !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}

	// Any error here, not-found OR a backend read error, refuses the upgrade
	// as a 404: no relay stands up for a room whose existence is unconfirmed,
	// and the two cases stay indistinguishable.
	if _, err := s.Rooms.Scan(appSlug, id); err != nil {
		http.NotFound(w, r)
		return
	}

	key := relay.RoomKey{App: appSlug, ID: id}

	// Caps are enforced BEFORE the handshake, so an over-limit upgrade is
	// refused with a normal HTTP status and no socket is accepted for it.
	_, connID, err := s.Relay.Admit(key)
	if err != nil {
		writeRelayAdmitError(w, err)
		return
	}

	// Accept rejects a non-Upgrade request and enforces the same-origin
	// policy, writing its own response. The reserved slot must be released on
	// that path or the connection count leaks.
	conn, err := websocket.Accept(w, r, wsAcceptOptions(s.ApexDomain))
	if err != nil {
		s.Relay.Release(key, connID)
		return
	}

	// Serve blocks for the connection's lifetime. The websocket is hijacked
	// out from under the http.Server's ReadTimeout / WriteTimeout, which must
	// NOT reap a live relay connection; the relay keeps its own per-connection
	// deadlines via the heartbeat. r.Context() is cancelled when the
	// underlying connection drops, bounding the connection's life.
	s.Relay.Serve(r.Context(), key, connID, conn)
}

// wsAcceptOptions builds the coder/websocket Accept options. The relay is a
// same-origin app API: the only authorized Origins are the app's own subdomain
// under the apex and the apex itself, for path mode. An empty apex disables
// origin verification so a test harness's localhost origin is accepted.
func wsAcceptOptions(apexDomain string) *websocket.AcceptOptions {
	if apexDomain == "" {
		return &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	return &websocket.AcceptOptions{
		OriginPatterns: []string{"*." + apexDomain, apexDomain},
	}
}

// writeRelayAdmitError maps the relay admission sentinels to status codes: a
// room / app connection cap is 429; the service-wide live-room cap is 503,
// which only refuses an upgrade that would create a NEW hub, so joins to live
// rooms still succeed.
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
