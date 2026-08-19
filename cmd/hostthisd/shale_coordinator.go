// shale_coordinator.go - coordinator selection and the retired-knob refusal.
// Untagged for the same reason as shale_timeouts.go: the consumer compiles
// only with -tags slatedb, but the parse contract is pure env logic the
// default suite can pin.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Zamua/hostthis/internal/storage"
)

// retiredCoordinatorEnv are the knobs that configured the removed SWIM mesh.
// Membership now comes from the coordinator the daemon constructs, so nothing
// reads these; a manifest still carrying one describes a cluster shape that no
// longer exists.
var retiredCoordinatorEnv = []string{"HOSTTHIS_SHALE_BIND_ADDR", "HOSTTHIS_SHALE_SEEDS"}

// shaleCoordinatorFromEnv reads HOSTTHIS_SHALE_COORDINATOR: "" (single-node)
// or "cas" (membership through the conditional store). Any other value refuses
// startup naming the variable, rather than coordinating over a silently
// substituted adapter.
func shaleCoordinatorFromEnv() (string, error) {
	raw := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_COORDINATOR"))
	switch strings.ToLower(raw) {
	case "":
		return "", nil
	case storage.CoordinatorCAS:
		return storage.CoordinatorCAS, nil
	default:
		return "", fmt.Errorf(
			"HOSTTHIS_SHALE_COORDINATOR: unrecognized value %q (want \"\" or \"cas\"; the gossip adapter was removed)", raw)
	}
}

// checkRetiredCoordinatorEnv refuses startup when a retired mesh knob is
// PRESENT, empty or not. Rejecting on presence rather than on a non-empty
// value is deliberate and stricter than the equivalent guard in shale's own
// run loop: that one tolerates an empty value so an overlay can neutralise a
// base manifest it cannot edit, which is exactly the trick used during the CAS
// migration here. Nothing sets these any more, so a reappearance - blank
// included - means someone is reconstructing a shape that no longer exists,
// and silently ignoring it is how a node boots believing it joined a mesh.
func checkRetiredCoordinatorEnv() error {
	for _, k := range retiredCoordinatorEnv {
		if v, ok := os.LookupEnv(k); ok {
			return fmt.Errorf(
				"%s is set (%q) but the gossip adapter it configured was removed; "+
					"membership now comes from HOSTTHIS_SHALE_COORDINATOR, so drop the variable", k, v)
		}
	}
	return nil
}

// checkHomogeneousForMode gates the homogeneous bootstrap: it needs the
// sharded multi-backend shape, and it is only meaningful in multi-node mode,
// which now means the cas coordinator.
func checkHomogeneousForMode(homogeneous bool, unitCount int, coordMode string) error {
	if !homogeneous {
		return nil
	}
	if unitCount <= 0 {
		return fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-backend mode (HOSTTHIS_SHALE_UNIT_COUNT > 0)")
	}
	if coordMode != storage.CoordinatorCAS {
		return fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires HOSTTHIS_SHALE_COORDINATOR=cas")
	}
	return nil
}

// checkCoordinatorConfig rejects cas without the homogeneous bootstrap that
// supplies its membership document's store.
func checkCoordinatorConfig(coordMode string, homogeneous bool) error {
	if coordMode != storage.CoordinatorCAS {
		return nil
	}
	if !homogeneous {
		return fmt.Errorf(
			"HOSTTHIS_SHALE_COORDINATOR=cas requires HOSTTHIS_SHALE_HOMOGENEOUS=true: " +
				"the homogeneous bootstrap constructs the conditional store the membership document lives in")
	}
	return nil
}
