package http

import (
	"encoding/json"
	"net/http"
	"testing"
)

// docs/SPEC.md "TURN relay credentials", against the TURN cell on the local
// runtime, which has no TURN settings: STUN only.
const stunOnlyICE = `{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]}]}` + "\n"

func TestRoomICE_ServesTheTurnCellsDocument(t *testing.T) {
	srv := buildRoomServer(t)
	id := createRoomID(t, srv, "appz2345")
	w := req(t, srv, http.MethodGet, "appz2345", "/api/rooms/"+id+"/ice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q", cc)
	}
	// The cell's bytes plus the newline the Worker's answer ends in.
	if w.Body.String() != stunOnlyICE {
		t.Fatalf("body %q, want %q", w.Body.String(), stunOnlyICE)
	}
	var doc struct {
		ICEServers []struct {
			URLs []string `json:"urls"`
		} `json:"iceServers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.ICEServers) != 1 {
		t.Fatalf("decode: %v %+v", err, doc)
	}
}

func TestRoomICE_StatusOrder(t *testing.T) {
	srv := buildRoomServer(t)
	const missing = "22222222-2222-4222-8222-222222222222"

	if w := req(t, srv, http.MethodGet, "appz2345", "/api/rooms/nope/ice", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: code %d", w.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := req(t, srv, method, "appz2345", "/api/rooms/"+missing+"/ice", nil)
		if method == http.MethodGet && w.Code != http.StatusNotFound {
			t.Fatalf("GET missing room: code %d", w.Code)
		}
		if method != http.MethodGet && (w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet) {
			t.Fatalf("%s: code %d allow %q", method, w.Code, w.Header().Get("Allow"))
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: cache-control %q", method, cc)
		}
	}

	// An unknown app is a 404 whatever the method.
	srv.Sites = nil
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if w := req(t, srv, method, "appz2345", "/api/rooms/"+missing+"/ice", nil); w.Code != http.StatusNotFound {
			t.Fatalf("unknown app %s: code %d", method, w.Code)
		}
	}
}

// ice is carved out like ws, so no value lands under it; ice/x and ice:x are
// ordinary keys.
func TestRoomICE_KeyIsReserved(t *testing.T) {
	srv := buildRoomServer(t)
	id := createRoomID(t, srv, "appz2345")
	if w := req(t, srv, http.MethodPut, "appz2345", "/api/rooms/"+id+"/ice", []byte("v")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT ice: code %d", w.Code)
	}
	for _, key := range []string{"ice/x", "ice:x"} {
		if w := req(t, srv, http.MethodPut, "appz2345", "/api/rooms/"+id+"/"+key, []byte("v")); w.Code != http.StatusNoContent {
			t.Fatalf("PUT %s: code %d body %q", key, w.Code, w.Body.String())
		}
	}
}
