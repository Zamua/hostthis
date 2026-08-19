package main

import (
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleCoordinatorFromEnv(t *testing.T) {
	const key = "HOSTTHIS_SHALE_COORDINATOR"

	t.Run("absent selects the default (gossip iff bind addr)", func(t *testing.T) {
		unsetEnv(t, key)
		mode, err := shaleCoordinatorFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "" {
			t.Fatalf("mode = %q, want empty (default path)", mode)
		}
	})

	t.Run("empty and whitespace select the default", func(t *testing.T) {
		t.Setenv(key, "   ")
		mode, err := shaleCoordinatorFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "" {
			t.Fatalf("mode = %q, want empty (default path)", mode)
		}
	})

	t.Run("gossip is the explicit spelling of the default", func(t *testing.T) {
		t.Setenv(key, "gossip")
		// The explicit spelling requires the bind addr that makes it true;
		// TestCoordinatorGossipRequiresBindAddr pins the refusal without it.
		t.Setenv("HOSTTHIS_SHALE_BIND_ADDR", "127.0.0.1:7946")
		mode, err := shaleCoordinatorFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "" {
			t.Fatalf("mode = %q, want empty (default path)", mode)
		}
	})

	t.Run("cas selects the CAS adapter", func(t *testing.T) {
		t.Setenv(key, "cas")
		mode, err := shaleCoordinatorFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != storage.CoordinatorCAS {
			t.Fatalf("mode = %q, want %q", mode, storage.CoordinatorCAS)
		}
	})

	t.Run("value is case-insensitive", func(t *testing.T) {
		t.Setenv(key, "CAS")
		mode, err := shaleCoordinatorFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != storage.CoordinatorCAS {
			t.Fatalf("mode = %q, want %q", mode, storage.CoordinatorCAS)
		}
	})

	t.Run("unrecognized value fails naming the variable", func(t *testing.T) {
		t.Setenv(key, "raft")
		_, err := shaleCoordinatorFromEnv()
		if err == nil {
			t.Fatal("want error for unrecognized coordinator, got nil")
		}
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error %q does not name %s", err, key)
		}
	})
}

func TestCheckCoordinatorConfig(t *testing.T) {
	const (
		coordKey = "HOSTTHIS_SHALE_COORDINATOR"
		bindKey  = "HOSTTHIS_SHALE_BIND_ADDR"
		seedsKey = "HOSTTHIS_SHALE_SEEDS"
		homogKey = "HOSTTHIS_SHALE_HOMOGENEOUS"
	)

	t.Run("default mode accepts any gossip config", func(t *testing.T) {
		if err := checkCoordinatorConfig("", "10.0.0.1:7946", "10.0.0.2:7946", false); err != nil {
			t.Fatalf("default mode must not constrain gossip config, got: %v", err)
		}
	})

	t.Run("cas with bind addr is refused naming both variables", func(t *testing.T) {
		err := checkCoordinatorConfig(storage.CoordinatorCAS, "10.0.0.1:7946", "", true)
		if err == nil {
			t.Fatal("want error for cas + bind addr, got nil")
		}
		for _, want := range []string{coordKey, bindKey} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %s", err, want)
			}
		}
	})

	t.Run("cas with seeds is refused naming both variables", func(t *testing.T) {
		err := checkCoordinatorConfig(storage.CoordinatorCAS, "", "10.0.0.2:7946", true)
		if err == nil {
			t.Fatal("want error for cas + seeds, got nil")
		}
		for _, want := range []string{coordKey, seedsKey} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %s", err, want)
			}
		}
	})

	t.Run("cas without homogeneous is refused naming the variable", func(t *testing.T) {
		err := checkCoordinatorConfig(storage.CoordinatorCAS, "", "", false)
		if err == nil {
			t.Fatal("want error for cas without homogeneous, got nil")
		}
		if !strings.Contains(err.Error(), homogKey) {
			t.Fatalf("error %q does not name %s", err, homogKey)
		}
	})

	t.Run("cas with homogeneous and no gossip config passes", func(t *testing.T) {
		if err := checkCoordinatorConfig(storage.CoordinatorCAS, "", "", true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestCheckHomogeneousForMode(t *testing.T) {
	const (
		unitKey = "HOSTTHIS_SHALE_UNIT_COUNT"
		bindKey = "HOSTTHIS_SHALE_BIND_ADDR"
	)

	t.Run("off passes regardless of the rest", func(t *testing.T) {
		if err := checkHomogeneousForMode(false, 0, "", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("requires a unit count", func(t *testing.T) {
		err := checkHomogeneousForMode(true, 0, "10.0.0.1:7946", "")
		if err == nil {
			t.Fatal("want error for homogeneous without unit count, got nil")
		}
		if !strings.Contains(err.Error(), unitKey) {
			t.Fatalf("error %q does not name %s", err, unitKey)
		}
	})

	t.Run("requires multi-node mode when the coordinator is the default", func(t *testing.T) {
		err := checkHomogeneousForMode(true, 16, "", "")
		if err == nil {
			t.Fatal("want error for homogeneous without multi-node mode, got nil")
		}
		if !strings.Contains(err.Error(), bindKey) {
			t.Fatalf("error %q does not name %s", err, bindKey)
		}
	})

	t.Run("bind addr satisfies multi-node mode", func(t *testing.T) {
		if err := checkHomogeneousForMode(true, 16, "10.0.0.1:7946", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("the cas coordinator satisfies multi-node mode with no bind addr", func(t *testing.T) {
		if err := checkHomogeneousForMode(true, 16, "", storage.CoordinatorCAS); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// The explicit spelling must demand the config that makes it true: gossip
// without a bind address is N single-node clusters over one bucket.
func TestCoordinatorGossipRequiresBindAddr(t *testing.T) {
	t.Setenv("HOSTTHIS_SHALE_COORDINATOR", "gossip")
	unsetEnv(t, "HOSTTHIS_SHALE_BIND_ADDR")
	if _, err := shaleCoordinatorFromEnv(); err == nil ||
		!strings.Contains(err.Error(), "HOSTTHIS_SHALE_BIND_ADDR") {
		t.Fatalf("explicit gossip without a bind addr must refuse naming the var, got %v", err)
	}

	t.Setenv("HOSTTHIS_SHALE_BIND_ADDR", "127.0.0.1:7946")
	if mode, err := shaleCoordinatorFromEnv(); err != nil || mode != "" {
		t.Fatalf("explicit gossip with a bind addr = (%q, %v), want default mode", mode, err)
	}
}
