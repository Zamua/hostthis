package main

import (
	"io"
	"log"
	"testing"
	"time"
)

// Defaults apply when unset, overrides are honored, and out-of-range values
// stop startup.
func TestBuildUploadAdmission(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	gate, err := buildUploadAdmission(logger)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if gate.Limit() != 8 || gate.Wait() != 30*time.Second {
		t.Fatalf("defaults = %d, %s; want 8, 30s", gate.Limit(), gate.Wait())
	}

	t.Setenv("HOSTTHIS_UPLOAD_CONCURRENCY", "3")
	t.Setenv("HOSTTHIS_UPLOAD_WAIT", "5s")
	gate, err = buildUploadAdmission(logger)
	if err != nil {
		t.Fatalf("overrides: %v", err)
	}
	if gate.Limit() != 3 || gate.Wait() != 5*time.Second {
		t.Fatalf("overrides = %d, %s; want 3, 5s", gate.Limit(), gate.Wait())
	}

	for _, tc := range []struct{ limit, wait string }{{"0", "30s"}, {"-2", "30s"}, {"4", "-1s"}, {"4", "0s"}} {
		t.Setenv("HOSTTHIS_UPLOAD_CONCURRENCY", tc.limit)
		t.Setenv("HOSTTHIS_UPLOAD_WAIT", tc.wait)
		if _, err := buildUploadAdmission(logger); err == nil {
			t.Fatalf("limit %q wait %q accepted", tc.limit, tc.wait)
		}
	}
}
