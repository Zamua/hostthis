package http

import (
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestRoomsAccess_AdmitsListedNetworksOrTheToken(t *testing.T) {
	allow := domain.NewDenylist([]domain.DenyEntry{{Entry: "198.51.100.0/24"}})
	a := RoomsAccess{Allow: &allow, Token: "s3cret"}
	req := func(peer, token string) *stdhttp.Request {
		r := httptest.NewRequest(stdhttp.MethodPost, "/api/rooms", nil)
		r.RemoteAddr = peer + ":4000"
		if token != "" {
			r.Header.Set(RoomsTokenHeader, token)
		}
		return r
	}
	cases := []struct {
		peer, token string
		want        bool
	}{
		{"198.51.100.7", "", true},
		{"203.0.113.9", "s3cret", true},
		{"203.0.113.9", "wrong", false},
		{"203.0.113.9", "", false},
	}
	for _, c := range cases {
		if got := a.admits(req(c.peer, c.token), ClientIPPeer); got != c.want {
			t.Errorf("peer %s token %q: admits = %v, want %v", c.peer, c.token, got, c.want)
		}
	}
	if !(RoomsAccess{}).admits(req("203.0.113.9", ""), ClientIPPeer) {
		t.Error("the zero RoomsAccess refused a request")
	}
}

func TestRoomsAccess_RefusesThePrivateAPIWith403(t *testing.T) {
	srv := &Server{Rooms: stubRoomsForAccess{}, RoomsAccess: RoomsAccess{Token: "s3cret"}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(stdhttp.MethodPost, "/api/rooms", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	srv.handleRoomsAPI(w, r, "app12345", "")
	if w.Code != stdhttp.StatusForbidden || w.Body.String() != "the rooms API is private on this instance\n\n" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

// stubRoomsForAccess panics on any call: nothing may get past the gate.
type stubRoomsForAccess struct{ RoomService }

type notTextRooms struct{ RoomService }

func (notTextRooms) Put(domain.Slug, domain.RoomID, string, []byte) (uint64, error) {
	return 0, domain.ErrRoomValueNotText
}

func TestRooms_ANotTextRefusalIs415WithTheActionableMessage(t *testing.T) {
	srv := &Server{Rooms: notTextRooms{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(stdhttp.MethodPut, "/api/rooms/11111111-1111-4111-8111-111111111111/k", strings.NewReader("x"))
	srv.handleRoomsAPI(w, r, "app12345", "/11111111-1111-4111-8111-111111111111/k")
	want := "room values must be small text/JSON; host files as a site (tar czf - dir | ssh hostthis.dev)\n\n"
	if w.Code != stdhttp.StatusUnsupportedMediaType || w.Body.String() != want {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}
