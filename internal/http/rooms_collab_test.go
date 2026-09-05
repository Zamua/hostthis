package http

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Holding the room id is the whole access model: a second participant on a
// different IP sees the first's writes on its join scan, and its own writes
// show up in the first's next scan.
func TestRoomsHTTP_TwoParticipantsCollaborate(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	const ipB = "198.51.100.9:50000"
	id := createRoomID(t, srv, slug)

	if w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/slot/mon", []byte("alice")); w.Code != http.StatusNoContent {
		t.Fatalf("A put: code %d", w.Code)
	}
	w := req(t, srv, http.MethodGet, slug, "/api/rooms/"+id, nil, from(ipB))
	var scan map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &scan); err != nil || string(scan["slot/mon"]) != `"alice"` {
		t.Fatalf("B join scan: code %d body %q (%v)", w.Code, w.Body.String(), err)
	}
	if w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/slot/tue", []byte("bob"), from(ipB)); w.Code != http.StatusNoContent {
		t.Fatalf("B put: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, "/api/rooms/"+id, nil)
	scan = nil
	if err := json.Unmarshal(w.Body.Bytes(), &scan); err != nil || len(scan) != 2 || string(scan["slot/tue"]) != `"bob"` {
		t.Fatalf("A re-scan: code %d body %q (%v)", w.Code, w.Body.String(), err)
	}
}
