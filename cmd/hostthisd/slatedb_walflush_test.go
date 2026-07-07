package main

import (
	"strings"
	"testing"
)

// TestMaxWALFlushesFromEnv pins the HOSTTHIS_SLATEDB_MAX_WAL_FLUSHES parse
// contract (docs/SPEC.md "WAL flush backstop: bounding live WAL
// accumulation"): unset/empty applies the hostthis default backstop (256),
// "0" is the kill-switch that keeps slatedb's own default (the ShaleConfig
// zero value), any other positive integer is used as-is, and a malformed or
// negative value is a configuration error that must fail startup loudly.
func TestMaxWALFlushesFromEnv(t *testing.T) {
	const key = "HOSTTHIS_SLATEDB_MAX_WAL_FLUSHES"

	t.Run("absent applies the 256 default", func(t *testing.T) {
		unsetEnv(t, key)
		n, err := maxWALFlushesFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 256 {
			t.Fatalf("got %d, want 256", n)
		}
	})

	t.Run("empty and whitespace apply the 256 default", func(t *testing.T) {
		for _, v := range []string{"", "   "} {
			t.Setenv(key, v)
			n, err := maxWALFlushesFromEnv()
			if err != nil {
				t.Fatalf("value %q: unexpected error: %v", v, err)
			}
			if n != 256 {
				t.Fatalf("value %q: got %d, want 256", v, n)
			}
		}
	})

	t.Run("zero is the kill-switch (slatedb default)", func(t *testing.T) {
		t.Setenv(key, "0")
		n, err := maxWALFlushesFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Fatalf("got %d, want 0 (leave slatedb default untouched)", n)
		}
	})

	t.Run("positive value is used as-is", func(t *testing.T) {
		t.Setenv(key, "64")
		n, err := maxWALFlushesFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 64 {
			t.Fatalf("got %d, want 64", n)
		}
	})

	t.Run("malformed value is a startup error", func(t *testing.T) {
		t.Setenv(key, "lots")
		if _, err := maxWALFlushesFromEnv(); err == nil {
			t.Fatal("want error for a non-integer value, got nil")
		} else if !strings.Contains(err.Error(), key) {
			t.Fatalf("error should name the variable: %v", err)
		}
	})

	t.Run("negative value is a startup error", func(t *testing.T) {
		t.Setenv(key, "-1")
		if _, err := maxWALFlushesFromEnv(); err == nil {
			t.Fatal("want error for a negative value, got nil")
		}
	})
}
