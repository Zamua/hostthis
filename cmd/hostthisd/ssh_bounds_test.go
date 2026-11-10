package main

import (
	"io"
	"log"
	"testing"
	"time"
)

// Defaults apply when unset, overrides are honored, and a value that is not
// positive or does not exceed the upload wait stops startup.
func TestSSHConnectionBounds(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	const uploadWait = 30 * time.Second

	idle, max, err := sshConnectionBounds(logger, uploadWait)
	if err != nil || idle != 5*time.Minute || max != 2*time.Hour {
		t.Fatalf("defaults = %s, %s, %v; want 5m, 2h", idle, max, err)
	}

	t.Setenv("HOSTTHIS_SSH_IDLE_TIMEOUT", "90s")
	t.Setenv("HOSTTHIS_SSH_MAX_SESSION", "10m")
	idle, max, err = sshConnectionBounds(logger, uploadWait)
	if err != nil || idle != 90*time.Second || max != 10*time.Minute {
		t.Fatalf("overrides = %s, %s, %v; want 90s, 10m", idle, max, err)
	}

	for _, tc := range []struct{ idle, max string }{{"0s", "1h"}, {"-1s", "1h"}, {"5m", "0s"}, {"30s", "1h"}, {"10s", "1h"}} {
		t.Setenv("HOSTTHIS_SSH_IDLE_TIMEOUT", tc.idle)
		t.Setenv("HOSTTHIS_SSH_MAX_SESSION", tc.max)
		if _, _, err := sshConnectionBounds(logger, uploadWait); err == nil {
			t.Fatalf("idle %q max %q accepted", tc.idle, tc.max)
		}
	}
}
