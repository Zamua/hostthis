package celld

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestRoomRepoCreateMapsFullApp(t *testing.T) {
	var body struct {
		AppCap int64 `json:"appCap"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/room/create" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = w.Write([]byte(`{"error":"app-full"}`))
	}))
	defer srv.Close()

	now := time.Unix(1, 0).UTC()
	err := NewRoomRepo(srv.URL, srv.Client()).CreateRoom(domain.Room{
		AppSlug:   domain.Slug("abcdefgh"),
		ID:        domain.RoomID("123e4567-e89b-42d3-a456-426614174000"),
		CreatedAt: now,
		UpdatedAt: now,
	}, "192.0.2.0/24", 123, now)
	if !errors.Is(err, domain.ErrAppRoomsFull) {
		t.Fatalf("CreateRoom error = %v", err)
	}
	if body.AppCap != 123 {
		t.Fatalf("appCap = %d, want 123", body.AppCap)
	}
}
