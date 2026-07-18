package main

import (
	"strings"
	"testing"
)

func TestReadyMinMountedFractionFromEnv(t *testing.T) {
	const key = "HOSTTHIS_READY_MIN_MOUNTED_FRACTION"

	t.Run("absent defaults to 0.5", func(t *testing.T) {
		unsetEnv(t, key)
		f, err := readyMinMountedFractionFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != 0.5 {
			t.Fatalf("got %v, want default 0.5", f)
		}
	})

	t.Run("empty and whitespace default to 0.5", func(t *testing.T) {
		t.Setenv(key, "   ")
		f, err := readyMinMountedFractionFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != 0.5 {
			t.Fatalf("got %v, want default 0.5", f)
		}
	})

	t.Run("zero disables the floor (passes 0 through to the shale predicate)", func(t *testing.T) {
		t.Setenv(key, "0")
		f, err := readyMinMountedFractionFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != 0 {
			t.Fatalf("got %v, want 0", f)
		}
	})

	t.Run("one is the strict end", func(t *testing.T) {
		t.Setenv(key, "1")
		f, err := readyMinMountedFractionFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != 1 {
			t.Fatalf("got %v, want 1", f)
		}
	})

	t.Run("in-range fraction is passed through", func(t *testing.T) {
		t.Setenv(key, "0.75")
		f, err := readyMinMountedFractionFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != 0.75 {
			t.Fatalf("got %v, want 0.75", f)
		}
	})

	t.Run("malformed fails loudly naming the variable", func(t *testing.T) {
		t.Setenv(key, "half")
		_, err := readyMinMountedFractionFromEnv()
		if err == nil {
			t.Fatal("want error for malformed fraction, got nil")
		}
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error %q does not name %s", err, key)
		}
	})

	t.Run("above one fails (out of range)", func(t *testing.T) {
		t.Setenv(key, "1.5")
		if _, err := readyMinMountedFractionFromEnv(); err == nil {
			t.Fatal("want error for fraction > 1, got nil")
		}
	})

	t.Run("negative fails (out of range)", func(t *testing.T) {
		t.Setenv(key, "-0.1")
		if _, err := readyMinMountedFractionFromEnv(); err == nil {
			t.Fatal("want error for negative fraction, got nil")
		}
	})

	t.Run("NaN fails (garbage must not configure a floor)", func(t *testing.T) {
		t.Setenv(key, "NaN")
		if _, err := readyMinMountedFractionFromEnv(); err == nil {
			t.Fatal("want error for NaN, got nil")
		}
	})
}
