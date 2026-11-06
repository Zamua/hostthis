package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// RoomICEService hands a room participant its WebRTC ICE servers (SPEC.md
// "TURN relay credentials"). internal/service.RoomICE satisfies it.
type RoomICEService interface {
	Servers(appSlug domain.Slug, id domain.RoomID) (json.RawMessage, error)
}

// handleRoomICE serves GET /api/rooms/<uuid>/ice with id already parsed: an
// unknown app 404s first, then a method other than GET is 405, then an unknown
// room 404s. The body is the TURN cell's document, as the Worker serves it.
func (s *Server) handleRoomICE(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID) {
	if s.RoomICE == nil || !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		roomMethodNotAllowed(w, http.MethodGet)
		return
	}
	doc, err := s.RoomICE.Servers(appSlug, id)
	switch {
	case errors.Is(err, service.ErrRoomNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logf("warn: room ice: app=%s: %v", appSlug, err)
		http.Error(w, "internal error\n", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(doc, '\n'))
}
