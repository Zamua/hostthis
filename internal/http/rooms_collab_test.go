package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// reqFrom is like req but lets the caller choose the source address, so a
// test can simulate two distinct participants (different IPs) hitting the
// same room. The two stand in for two browsers sharing a room link.
func reqFrom(t *testing.T, srv *Server, method, slug, path, remoteAddr string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, "http://"+slug+".hostthis.test"+path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "http://"+slug+".hostthis.test"+path, nil)
	}
	r.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestRoomsHTTP_TwoParticipantsCollaborate exercises the headline
// collaborative-on-refresh flow end to end through the HTTP surface, with
// two simulated participants on different source IPs sharing one room
// link. Participant A creates the room and shares its id; A writes, B
// reads it back via a full-room scan (its "join"), B writes, and A sees
// B's write on its next scan. No per-user auth anywhere - holding the room
// id is the whole access model, so both participants address the one
// shared namespace.
func TestRoomsHTTP_TwoParticipantsCollaborate(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"

	// Participant A (one IP) creates the room and shares the id.
	const ipA = "203.0.113.5:40000"
	wc := reqFrom(t, srv, http.MethodPost, slug, "/api/rooms", ipA, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("A create: code %d body %q", wc.Code, wc.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(wc.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := created.ID
	if _, err := domain.ParseRoomID(id); err != nil {
		t.Fatalf("created id not a valid v4: %q", id)
	}

	// A writes its availability.
	if w := reqFrom(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/slot/mon", ipA, []byte("alice")); w.Code != http.StatusNoContent {
		t.Fatalf("A put: code %d", w.Code)
	}

	// Participant B (a DIFFERENT IP) holds the same link. On join it scans
	// the room and must see A's write.
	const ipB = "198.51.100.9:50000"
	w := reqFrom(t, srv, http.MethodGet, slug, "/api/rooms/"+id, ipB, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("B scan: code %d", w.Code)
	}
	var bScan map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &bScan); err != nil {
		t.Fatalf("B scan decode: %v (%q)", err, w.Body.String())
	}
	if string(bScan["slot/mon"]) != `"alice"` {
		t.Fatalf("B did not see A's write on join: %s", bScan["slot/mon"])
	}

	// B writes its own value into the same shared namespace.
	if w := reqFrom(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/slot/tue", ipB, []byte("bob")); w.Code != http.StatusNoContent {
		t.Fatalf("B put: code %d", w.Code)
	}

	// A refreshes (re-scans) and now sees BOTH writes.
	w = reqFrom(t, srv, http.MethodGet, slug, "/api/rooms/"+id, ipA, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("A re-scan: code %d", w.Code)
	}
	var aScan map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &aScan); err != nil {
		t.Fatalf("A re-scan decode: %v", err)
	}
	if len(aScan) != 2 {
		t.Fatalf("A re-scan key count = %d, want 2", len(aScan))
	}
	if string(aScan["slot/tue"]) != `"bob"` {
		t.Fatalf("A did not see B's write on refresh: %s", aScan["slot/tue"])
	}
	if string(aScan["slot/mon"]) != `"alice"` {
		t.Fatalf("A lost its own write: %s", aScan["slot/mon"])
	}

	// A point-reads B's key directly (not just via scan).
	w = reqFrom(t, srv, http.MethodGet, slug, "/api/rooms/"+id+"/slot/tue", ipA, nil)
	if w.Code != http.StatusOK || w.Body.String() != "bob" {
		t.Fatalf("A point-read of B's key: code %d body %q", w.Code, w.Body.String())
	}
}
