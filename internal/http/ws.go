package http

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
)

// wsKey is the reserved trailing segment that turns a room path into the
// real-time relay upgrade: GET (Upgrade: websocket) /api/rooms/<uuid>/ws. It
// is the one key the KV verbs do not serve as data, so the relay shares the
// room's path space without colliding with a stored value.
const wsKey = "ws"

// RoomRelay is the real-time surface the HTTP layer needs to stand up a
// WebSocket connection for a room. internal/celld.RoomProxy satisfies it.
// Optional: when nil, the /ws path 404s.
type RoomRelay interface {
	// Admit reserves a connection slot under the per-room and per-app caps.
	// Called before the WebSocket handshake.
	Admit(key roomwire.RoomKey) (uint64, error)
	// Serve runs the accepted connection's lifecycle (snapshot, stream,
	// heartbeat) and blocks until it ends.
	Serve(ctx context.Context, key roomwire.RoomKey, id uint64, ws *websocket.Conn)
	// Release frees a slot reserved by Admit but never handed to Serve.
	Release(key roomwire.RoomKey, id uint64)
}

// handleRoomWS performs the WebSocket upgrade for a room's relay, reached for
// the reserved /<uuid>/ws path with id already parsed. Every check refuses
// with a normal HTTP status BEFORE any 101, so a client's open fails cleanly
// and no hub is reached by an invalid request:
//
//   - the app slug names a LIVE app, else 404
//   - the room EXISTS, else 404: a relay to a never-created room has nothing
//     to back its late-join snapshot
//   - the connection caps admit it, else 429
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

	// Not-found OR a backend read error refuses the upgrade as a 404: no relay
	// stands up for a room whose existence is unconfirmed.
	if _, err := s.Rooms.Scan(appSlug, id); err != nil {
		http.NotFound(w, r)
		return
	}

	key := roomwire.RoomKey{App: appSlug, ID: id}

	// Caps are enforced BEFORE the handshake, so an over-limit upgrade is
	// refused with a normal HTTP status and no socket is accepted for it.
	connID, err := s.Relay.Admit(key)
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

	// Serve blocks for the connection's lifetime. The hijacked socket is
	// outside http.Server's Read/WriteTimeout; the relay's heartbeat bounds it,
	// and r.Context() cancels when the underlying connection drops.
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

func writeRelayAdmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, roomwire.ErrRelayDraining):
		http.Error(w, "service restarting\n", http.StatusServiceUnavailable)
	case errors.Is(err, roomwire.ErrRoomFull), errors.Is(err, roomwire.ErrAppFull):
		http.Error(w, "room connection limit reached\n", http.StatusTooManyRequests)
	default:
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}
