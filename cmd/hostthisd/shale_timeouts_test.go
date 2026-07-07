package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// unsetEnv removes key for the duration of the test. t.Setenv registers the
// restore-on-cleanup; the explicit Unsetenv makes the variable truly absent
// (not just empty) for the case under test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

func TestShaleTimeoutsFromEnv(t *testing.T) {
	const (
		readKey  = "HOSTTHIS_SHALE_READ_TIMEOUT"
		writeKey = "HOSTTHIS_SHALE_WRITE_TIMEOUT"
	)

	t.Run("absent leaves both zero (shale defaults apply)", func(t *testing.T) {
		unsetEnv(t, readKey)
		unsetEnv(t, writeKey)
		r, w, err := shaleTimeoutsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != 0 || w != 0 {
			t.Fatalf("got read=%v write=%v, want both zero", r, w)
		}
	})

	t.Run("empty and whitespace values leave zero", func(t *testing.T) {
		t.Setenv(readKey, "")
		t.Setenv(writeKey, "   ")
		r, w, err := shaleTimeoutsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != 0 || w != 0 {
			t.Fatalf("got read=%v write=%v, want both zero", r, w)
		}
	})

	t.Run("present values populate both fields", func(t *testing.T) {
		t.Setenv(readKey, "8s")
		t.Setenv(writeKey, "12s")
		r, w, err := shaleTimeoutsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != 8*time.Second {
			t.Fatalf("read timeout = %v, want 8s", r)
		}
		if w != 12*time.Second {
			t.Fatalf("write timeout = %v, want 12s", w)
		}
	})

	t.Run("one set alone leaves the other zero", func(t *testing.T) {
		t.Setenv(readKey, "8s")
		unsetEnv(t, writeKey)
		r, w, err := shaleTimeoutsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != 8*time.Second || w != 0 {
			t.Fatalf("got read=%v write=%v, want read=8s write=0", r, w)
		}
	})

	t.Run("malformed read timeout fails loudly", func(t *testing.T) {
		t.Setenv(readKey, "8seconds")
		unsetEnv(t, writeKey)
		_, _, err := shaleTimeoutsFromEnv()
		if err == nil {
			t.Fatal("want error for malformed read timeout, got nil")
		}
		if !strings.Contains(err.Error(), readKey) {
			t.Fatalf("error %q does not name %s", err, readKey)
		}
	})

	t.Run("malformed write timeout fails loudly", func(t *testing.T) {
		unsetEnv(t, readKey)
		t.Setenv(writeKey, "banana")
		_, _, err := shaleTimeoutsFromEnv()
		if err == nil {
			t.Fatal("want error for malformed write timeout, got nil")
		}
		if !strings.Contains(err.Error(), writeKey) {
			t.Fatalf("error %q does not name %s", err, writeKey)
		}
	})

	t.Run("bare number without a unit fails (Go duration required)", func(t *testing.T) {
		t.Setenv(readKey, "8")
		unsetEnv(t, writeKey)
		if _, _, err := shaleTimeoutsFromEnv(); err == nil {
			t.Fatal("want error for unit-less duration, got nil")
		}
	})

	t.Run("negative duration fails", func(t *testing.T) {
		unsetEnv(t, readKey)
		t.Setenv(writeKey, "-1s")
		if _, _, err := shaleTimeoutsFromEnv(); err == nil {
			t.Fatal("want error for negative duration, got nil")
		}
	})
}
