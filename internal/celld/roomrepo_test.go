package celld

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestRoomRepoCreateMapsFullApp(t *testing.T) {
	f := fixedCell(http.StatusInsufficientStorage, `{"error":"app-full"}`)
	now := time.Unix(1, 0).UTC()
	err := NewRoomRepo("https://cell", f.client()).CreateRoom(domain.Room{
		AppSlug:   domain.Slug("abcdefgh"),
		ID:        domain.RoomID("123e4567-e89b-42d3-a456-426614174000"),
		CreatedAt: now,
		UpdatedAt: now,
	}, "192.0.2.0/24", 123, now)
	if !errors.Is(err, domain.ErrAppRoomsFull) {
		t.Fatalf("CreateRoom error = %v", err)
	}
	if req := f.last(); req.Path != "/room/create" || req.fields(t)["appCap"] != float64(123) {
		t.Fatalf("request = %s appCap %#v, want /room/create with 123", req.Path, req.fields(t)["appCap"])
	}
}
