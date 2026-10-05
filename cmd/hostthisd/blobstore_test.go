package main

import (
	"io"
	"log"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

// Only the celld backend queues its writes, and out-of-range queue settings
// stop startup.
func TestBuildBlobStoreGatesCelldOnly(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	t.Setenv("HOSTTHIS_CELLD_ENDPOINT", "http://celld.invalid")

	t.Setenv("HOSTTHIS_BLOB_BACKEND", "celld")
	bs, err := buildBlobStore(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("celld: %v", err)
	}
	if _, ok := bs.Inner.(*storage.GatedBlobStore); !ok {
		t.Fatalf("celld inner = %T, want *storage.GatedBlobStore", bs.Inner)
	}

	t.Setenv("HOSTTHIS_BLOB_BACKEND", "disk")
	bs, err = buildBlobStore(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if _, ok := bs.Inner.(*storage.GatedBlobStore); ok {
		t.Fatal("disk backend is gated")
	}

	t.Setenv("HOSTTHIS_BLOB_BACKEND", "celld")
	for _, tc := range []struct{ budget, wait string }{{"0", "30s"}, {"-5", "30s"}, {"1024", "-1s"}} {
		t.Setenv("HOSTTHIS_CELLD_PUT_BUDGET_BYTES", tc.budget)
		t.Setenv("HOSTTHIS_CELLD_PUT_WAIT", tc.wait)
		if _, err := buildBlobStore(t.TempDir(), logger); err == nil {
			t.Fatalf("budget %q wait %q accepted", tc.budget, tc.wait)
		}
	}
}
