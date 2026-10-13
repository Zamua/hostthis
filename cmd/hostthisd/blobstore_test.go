package main

import (
	"io"
	"log"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

// Worker writes queue by weight, and out-of-range queue settings stop startup.
func TestBuildBlobStoreGatesWorkerWrites(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", "http://worker.invalid")

	bs, err := buildBlobStore(logger)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if _, ok := bs.Inner.(*storage.GatedBlobStore); !ok {
		t.Fatalf("worker inner = %T, want *storage.GatedBlobStore", bs.Inner)
	}

	for _, tc := range []struct{ budget, wait string }{{"0", "30s"}, {"-5", "30s"}, {"1024", "-1s"}} {
		t.Setenv("HOSTTHIS_WORKER_PUT_BUDGET_BYTES", tc.budget)
		t.Setenv("HOSTTHIS_WORKER_PUT_WAIT", tc.wait)
		if _, err := buildBlobStore(logger); err == nil {
			t.Fatalf("budget %q wait %q accepted", tc.budget, tc.wait)
		}
	}
}
