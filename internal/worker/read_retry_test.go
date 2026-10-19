package worker_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Zamua/hostthis/internal/worker"
)

// A metadata read whose connection drops before any answer is sent once more.
func TestMetadataReadRetriesADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	repo := worker.NewPasteRepo(srv.URL, srv.Client())
	if _, err := repo.ListVersions("slugone1"); err != nil {
		t.Fatalf("read after one dropped connection: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}
