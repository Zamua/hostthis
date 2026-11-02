package http

import (
	"net/http/httptest"
	"testing"
)

// The agent notes answer at both paths on the apex, never on a slug's host.
func TestAgentNotesServeOnTheApexOnly(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test", AgentNotes: []byte("# hostthis\n")}
	for _, path := range []string{"/llms.txt", "/AGENTS.md"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://paste.test"+path, nil))
		if w.Code != 200 || w.Body.String() != "# hostthis\n" || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Fatalf("%s: %d %q %q", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://abcd2345.paste.test/llms.txt", nil))
	if w.Body.String() == "# hostthis\n" {
		t.Fatal("a slug host served the apex notes")
	}
}

// Without notes the paths are unknown.
func TestAgentNotesUnsetIsNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{ApexDomain: "paste.test"}).Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://paste.test/llms.txt", nil))
	if w.Code != 404 {
		t.Fatalf("status %d, want 404", w.Code)
	}
}
