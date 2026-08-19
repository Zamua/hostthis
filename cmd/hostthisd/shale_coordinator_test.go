package main

import (
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleCoordinatorFromEnv(t *testing.T) {
	const key = "HOSTTHIS_SHALE_COORDINATOR"

	t.Run("absent selects single-node", func(t *testing.T) {
		unsetEnv(t, key)
		mode, err := shaleCoordinatorFromEnv()
		if err != nil || mode != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", mode, err)
		}
	})

	t.Run("cas selects the document coordinator", func(t *testing.T) {
		t.Setenv(key, "cas")
		mode, err := shaleCoordinatorFromEnv()
		if err != nil || mode != storage.CoordinatorCAS {
			t.Fatalf("got (%q, %v), want (%q, nil)", mode, err, storage.CoordinatorCAS)
		}
	})

	t.Run("case and whitespace tolerated", func(t *testing.T) {
		t.Setenv(key, "  CAS ")
		if mode, err := shaleCoordinatorFromEnv(); err != nil || mode != storage.CoordinatorCAS {
			t.Fatalf("got (%q, %v), want cas", mode, err)
		}
	})

	// The removed adapter's own name must not silently select single-node: an
	// operator writing it expects a cluster.
	t.Run("gossip is refused, naming the variable", func(t *testing.T) {
		t.Setenv(key, "gossip")
		_, err := shaleCoordinatorFromEnv()
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("want a refusal naming %s, got %v", key, err)
		}
	})

	t.Run("unknown value refused", func(t *testing.T) {
		t.Setenv(key, "swim")
		if _, err := shaleCoordinatorFromEnv(); err == nil {
			t.Fatal("want a refusal for an unknown coordinator")
		}
	})
}

// The retired mesh knobs are refused on PRESENCE, blank included: nothing reads
// them, so a manifest still carrying one describes a cluster shape that does
// not exist, and ignoring it is how a node boots believing it joined a mesh.
func TestRetiredCoordinatorEnvRefused(t *testing.T) {
	for _, k := range []string{"HOSTTHIS_SHALE_BIND_ADDR", "HOSTTHIS_SHALE_SEEDS"} {
		t.Run(k+" non-empty", func(t *testing.T) {
			unsetEnv(t, "HOSTTHIS_SHALE_BIND_ADDR")
			unsetEnv(t, "HOSTTHIS_SHALE_SEEDS")
			t.Setenv(k, "10.0.0.1:7946")
			err := checkRetiredCoordinatorEnv()
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("want a refusal naming %s, got %v", k, err)
			}
		})

		t.Run(k+" BLANK is still refused", func(t *testing.T) {
			unsetEnv(t, "HOSTTHIS_SHALE_BIND_ADDR")
			unsetEnv(t, "HOSTTHIS_SHALE_SEEDS")
			t.Setenv(k, "")
			if err := checkRetiredCoordinatorEnv(); err == nil {
				t.Fatalf("%s set to blank must still refuse: a blanked retired knob means "+
					"someone is reconstructing a shape that no longer exists", k)
			}
		})
	}

	t.Run("both absent passes", func(t *testing.T) {
		unsetEnv(t, "HOSTTHIS_SHALE_BIND_ADDR")
		unsetEnv(t, "HOSTTHIS_SHALE_SEEDS")
		if err := checkRetiredCoordinatorEnv(); err != nil {
			t.Fatalf("clean env must pass, got %v", err)
		}
	})
}

func TestCoordinatorConfigRequiresHomogeneous(t *testing.T) {
	if err := checkCoordinatorConfig(storage.CoordinatorCAS, false); err == nil ||
		!strings.Contains(err.Error(), "HOSTTHIS_SHALE_HOMOGENEOUS") {
		t.Fatalf("cas without homogeneous must refuse naming the var, got %v", err)
	}
	if err := checkCoordinatorConfig(storage.CoordinatorCAS, true); err != nil {
		t.Fatalf("cas + homogeneous must pass, got %v", err)
	}
	if err := checkCoordinatorConfig("", false); err != nil {
		t.Fatalf("single-node constrains nothing, got %v", err)
	}
}

func TestHomogeneousForMode(t *testing.T) {
	if err := checkHomogeneousForMode(true, 0, storage.CoordinatorCAS); err == nil {
		t.Fatal("homogeneous with unit count 0 must refuse")
	}
	if err := checkHomogeneousForMode(true, 16, ""); err == nil {
		t.Fatal("homogeneous single-node must refuse")
	}
	if err := checkHomogeneousForMode(true, 16, storage.CoordinatorCAS); err != nil {
		t.Fatalf("homogeneous + sharded + cas must pass, got %v", err)
	}
	if err := checkHomogeneousForMode(false, 0, ""); err != nil {
		t.Fatalf("homogeneous off constrains nothing, got %v", err)
	}
}
