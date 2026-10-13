package celld_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
)

// An admission whose connection drops after the cell recorded it is retried,
// and the replay's knownAlready still leads to the identity's subnet note.
func TestAdmitNewKeyRetriesADroppedConnection(t *testing.T) {
	var (
		mu     sync.Mutex
		admits int
		noted  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/subnet/admit":
			admits++
			if admits == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"knownAlready":true,"admitted":true}`))
		case "/identity/notesubnet":
			noted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	repo := celld.NewKeyGateRepo(srv.URL, nil)
	if _, err := repo.AdmitNewKey("key:SHA256:abc", "10.0.0.0/24", time.Now(), 3, time.Hour); err != nil {
		t.Fatalf("AdmitNewKey: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if admits != 2 || noted != 1 {
		t.Fatalf("admits=%d noted=%d, want 2 and 1", admits, noted)
	}
}
