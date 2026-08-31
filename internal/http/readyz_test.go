package http

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestReadyzReportsProcessReady(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Ready bool `json:"ready"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Ready {
		t.Fatal("ready = false, want true")
	}
}
