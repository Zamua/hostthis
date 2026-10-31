package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// Worker writes queue by weight, and out-of-range queue settings stop startup.
func TestBuildBlobStoreGatesWorkerWrites(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/held") {
			arrived <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(worker.Close)
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", worker.URL)
	t.Setenv("HOSTTHIS_WORKER_PUT_BUDGET_BYTES", "1")
	t.Setenv("HOSTTHIS_WORKER_PUT_WAIT", "50ms")

	bs, err := buildBlobStore(logger)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	ctx := context.Background()
	held := make(chan error, 1)
	go func() { held <- bs.StagePrecompressed(ctx, "uploads/u/held", strings.NewReader("ab"), 2) }()
	<-arrived
	if err := bs.StagePrecompressed(ctx, "uploads/u/queued", strings.NewReader("cd"), 2); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("put while the budget is spent = %v, want ErrStoreBusy", err)
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("held put: %v", err)
	}
	if err := bs.StagePrecompressed(ctx, "uploads/u/after", strings.NewReader("ef"), 2); err != nil {
		t.Fatalf("put after the budget frees: %v", err)
	}

	for _, tc := range []struct{ budget, wait string }{{"0", "30s"}, {"-5", "30s"}, {"1024", "-1s"}} {
		t.Setenv("HOSTTHIS_WORKER_PUT_BUDGET_BYTES", tc.budget)
		t.Setenv("HOSTTHIS_WORKER_PUT_WAIT", tc.wait)
		if _, err := buildBlobStore(logger); err == nil {
			t.Fatalf("budget %q wait %q accepted", tc.budget, tc.wait)
		}
	}
}
