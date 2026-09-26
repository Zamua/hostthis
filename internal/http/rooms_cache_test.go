package http

import (
	"net/http"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// Every rooms API response, error or success, is no-store.
func TestRoomsHTTP_EveryResponseIsNoStore(t *testing.T) {
	const slug = "appz2345"
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 2
	id := createRoomID(t, srv, slug)
	missing := domain.NewRoomID().String()

	for _, s := range []struct {
		name         string
		method, path string
		body         []byte
		code         int
	}{
		{"absent key before it is written", http.MethodGet, "/api/rooms/" + id + "/k", nil, http.StatusNotFound},
		{"write the key", http.MethodPut, "/api/rooms/" + id + "/k", []byte("v"), http.StatusNoContent},
		{"read it back", http.MethodGet, "/api/rooms/" + id + "/k", nil, http.StatusOK},
		{"scan the room", http.MethodGet, "/api/rooms/" + id, nil, http.StatusOK},
		{"delete the key", http.MethodDelete, "/api/rooms/" + id + "/k", nil, http.StatusNoContent},
		{"nonexistent room", http.MethodGet, "/api/rooms/" + missing + "/k", nil, http.StatusNotFound},
		{"malformed room id", http.MethodGet, "/api/rooms/not-a-uuid/k", nil, http.StatusBadRequest},
		{"wrong method", http.MethodPost, "/api/rooms/" + id + "/k", []byte("v"), http.StatusMethodNotAllowed},
		{"create", http.MethodPost, "/api/rooms", nil, http.StatusCreated},
		{"create over the rate limit", http.MethodPost, "/api/rooms", nil, http.StatusTooManyRequests},
	} {
		w := req(t, srv, s.method, slug, s.path, s.body)
		if w.Code != s.code {
			t.Fatalf("%s: code %d, want %d", s.name, w.Code, s.code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s (%d): Cache-Control %q, want no-store", s.name, w.Code, got)
		}
	}
}
