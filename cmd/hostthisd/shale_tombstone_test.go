package main

import (
	"strings"
	"testing"
	"time"
)

func TestTombstoneGraceFromEnv(t *testing.T) {
	const key = "HOSTTHIS_METADATA_TOMBSTONE_GRACE"

	t.Run("absent leaves zero (purge disabled)", func(t *testing.T) {
		unsetEnv(t, key)
		g, err := tombstoneGraceFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if g != 0 {
			t.Fatalf("got %v, want zero", g)
		}
	})

	t.Run("empty and whitespace leave zero", func(t *testing.T) {
		t.Setenv(key, "   ")
		g, err := tombstoneGraceFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if g != 0 {
			t.Fatalf("got %v, want zero", g)
		}
	})

	t.Run("present value parses", func(t *testing.T) {
		t.Setenv(key, "168h")
		g, err := tombstoneGraceFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if g != 168*time.Hour {
			t.Fatalf("got %v, want 168h", g)
		}
	})

	t.Run("malformed value errors naming the variable", func(t *testing.T) {
		t.Setenv(key, "7 days")
		_, err := tombstoneGraceFromEnv()
		if err == nil {
			t.Fatal("want error for malformed duration, got nil")
		}
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error must name %s, got: %v", key, err)
		}
	})

	t.Run("negative value errors", func(t *testing.T) {
		t.Setenv(key, "-1h")
		if _, err := tombstoneGraceFromEnv(); err == nil {
			t.Fatal("want error for negative duration, got nil")
		}
	})
}
