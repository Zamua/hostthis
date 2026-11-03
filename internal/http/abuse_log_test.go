package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/abuselog"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
	"github.com/Zamua/hostthis/internal/service"
)

const abuseRoomID = "123e4567-e89b-42d3-a456-426614174000"

// abuseLines collects every line the server's abuse logger prints.
type abuseLines struct {
	mu    sync.Mutex
	lines []string
}

func (a *abuseLines) printf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, fmt.Sprintf(format, args...))
}

func (a *abuseLines) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.lines...)
}

func withAbuseLog(srv *Server) *abuseLines {
	got := &abuseLines{}
	srv.Abuse = abuselog.New(got.printf)
	return got
}

// one asserts exactly one line was logged and returns its decoded fields.
func (a *abuseLines) one(t *testing.T) map[string]any {
	t.Helper()
	lines := a.all()
	if len(lines) != 1 {
		t.Fatalf("want one abuse line, got %d: %q", len(lines), lines)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
		t.Fatalf("line is not JSON: %q", lines[0])
	}
	return out
}

func wantLine(t *testing.T, got map[string]any, event, route string, status int) {
	t.Helper()
	if got["event"] != event || got["route"] != route || got["status"] != float64(status) || got["subnet"] != "203.0.113.0/24" || len(got) != 4 {
		t.Fatalf("line %v, want event=%s route=%s status=%d subnet=203.0.113.0/24", got, event, route, status)
	}
}

func TestAbuseLog_RoomCreationAndItsLimit(t *testing.T) {
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	lines := withAbuseLog(srv)

	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusCreated {
		t.Fatalf("create: code %d", w.Code)
	}
	wantLine(t, lines.one(t), "created", "collection", http.StatusCreated)

	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second create: code %d, want 429", w.Code)
	}
	all := lines.all()
	if len(all) != 2 {
		t.Fatalf("want two lines, got %q", all)
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(all[1]), &second)
	wantLine(t, second, "rate-limited", "collection", http.StatusTooManyRequests)
	for _, l := range all {
		if strings.Contains(l, "203.0.113.5") {
			t.Fatalf("line carries the full address: %q", l)
		}
	}
}

func TestAbuseLog_NormalTrafficLogsNothing(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	lines := withAbuseLog(srv)

	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/api/rooms/" + id + "/name", []byte("alice")},
		{http.MethodGet, "/api/rooms/" + id + "/name", nil},
		{http.MethodGet, "/api/rooms/" + id, nil},
		{http.MethodDelete, "/api/rooms/" + id + "/name", nil},
		{http.MethodGet, "/api/rooms/" + id + "/missing", nil},
	} {
		req(t, srv, c.method, slug, c.path, c.body)
	}
	if got := lines.all(); len(got) != 0 {
		t.Fatalf("normal traffic logged: %q", got)
	}
}

// failingRooms answers every value write with err.
type failingRooms struct {
	RoomService
	err error
}

func (f failingRooms) Put(domain.Slug, domain.RoomID, string, []byte) (uint64, error) {
	return 0, f.err
}

func TestAbuseLog_RefusedWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		event  string
		status int
	}{
		{"not text", domain.ErrRoomValueNotText, "rejected", http.StatusUnsupportedMediaType},
		{"content rule", &service.RoomValueRefused{Rule: "r"}, "rejected", http.StatusForbidden},
		{"room data cap", service.ErrRoomDataCap, "oversize", http.StatusRequestEntityTooLarge},
		{"app capacity", service.ErrAppRoomsCap, "capacity", http.StatusInsufficientStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{ApexDomain: "hostthis.test", Rooms: failingRooms{err: tc.err}}
			lines := withAbuseLog(srv)
			w := req(t, srv, http.MethodPut, "appz2345", "/api/rooms/"+abuseRoomID+"/k", []byte("v"))
			if w.Code != tc.status {
				t.Fatalf("code %d, want %d", w.Code, tc.status)
			}
			wantLine(t, lines.one(t), tc.event, "value", tc.status)
		})
	}
}

func TestAbuseLog_OtherRoomErrorsLogNothing(t *testing.T) {
	for _, err := range []error{service.ErrRoomNotFound, errors.New("backend down")} {
		srv := &Server{ApexDomain: "hostthis.test", Rooms: failingRooms{err: err}}
		lines := withAbuseLog(srv)
		req(t, srv, http.MethodPut, "appz2345", "/api/rooms/"+abuseRoomID+"/k", []byte("v"))
		if got := lines.all(); len(got) != 0 {
			t.Fatalf("%v logged: %q", err, got)
		}
	}
}

func TestAbuseLog_PrivateRoomsAPIRefusal(t *testing.T) {
	srv := &Server{
		ApexDomain:  "hostthis.test",
		Rooms:       stubRoomsForAccess{},
		RoomsAccess: RoomsAccess{Token: "s3cret"},
	}
	lines := withAbuseLog(srv)
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusForbidden {
		t.Fatalf("code %d, want 403", w.Code)
	}
	wantLine(t, lines.one(t), "auth-failure", "rooms", http.StatusForbidden)
}

// fullRelay refuses every connection as over the cap.
type fullRelay struct{ RoomRelay }

func (fullRelay) Admit(roomwire.RoomKey) (uint64, error) { return 0, roomwire.ErrRoomFull }

func TestAbuseLog_SocketOverTheCap(t *testing.T) {
	srv := buildRoomServer(t)
	srv.Relay = fullRelay{}
	id := createRoomID(t, srv, "appz2345")
	lines := withAbuseLog(srv)

	w := req(t, srv, http.MethodGet, "appz2345", "/api/rooms/"+id+"/ws", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code %d, want 429", w.Code)
	}
	wantLine(t, lines.one(t), "rate-limited", "ws", http.StatusTooManyRequests)
}

// The subnet is the one the creation limit counts: behind a trusted proxy the
// client hop, never the proxy and never the full address.
func TestAbuseLog_SubnetFollowsTheClientAddressSource(t *testing.T) {
	srv := buildRoomServer(t)
	srv.ClientIP = ClientIPForwardedFor
	lines := withAbuseLog(srv)
	req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil, from("10.0.0.1:4000"), xff("spoofed, 2001:db8:abcd:1234::77"))
	got := lines.one(t)
	if got["subnet"] != "2001:db8:ab00::/40" {
		t.Fatalf("subnet %v", got["subnet"])
	}
}

// Property: whatever the client address, no line spells it.
func TestAbuseLog_FullAddressNeverReachesALine(t *testing.T) {
	srv := buildRoomServer(t)
	srv.ClientIP = ClientIPForwardedFor
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	lines := withAbuseLog(srv)
	rng := rand.New(rand.NewSource(7))

	var addrs []string
	for range 300 {
		if rng.Intn(2) == 0 {
			addrs = append(addrs, fmt.Sprintf("%d.%d.%d.%d", 1+rng.Intn(222), rng.Intn(256), rng.Intn(256), 1+rng.Intn(254)))
		} else {
			addrs = append(addrs, fmt.Sprintf("2a%02x:%x:%x:%x::%x", rng.Intn(256), 1+rng.Intn(0xffff), 1+rng.Intn(0xffff), 1+rng.Intn(0xffff), 1+rng.Intn(0xffff)))
		}
	}
	for _, a := range addrs {
		// Two creates per address: one 201, one 429, so both event kinds run.
		req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil, from("10.0.0.1:4000"), xff(a))
		req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil, from("10.0.0.1:4000"), xff(a))
	}
	all := lines.all()
	if len(all) < len(addrs) {
		t.Fatalf("only %d lines for %d addresses", len(all), len(addrs))
	}
	for _, a := range addrs {
		for _, l := range all {
			if strings.Contains(l, a) {
				t.Fatalf("line %q carries the address %s", l, a)
			}
		}
	}
}
