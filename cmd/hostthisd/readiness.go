// readiness.go - env parsing for the /readyz mount floor (docs/SPEC.md
// "Readiness vs liveness"). Deliberately OUTSIDE the slatedb build tag,
// mirroring shale_timeouts.go: the consumer (buildMetadataShale) only
// compiles with -tags slatedb, but the parse contract is pure env+float
// logic, so keeping it untagged lets the default test suite pin it
// without the slatedb toolchain or MinIO.
package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// defaultReadyMinMountedFraction is the mount floor when the env var is
// absent: half the desired units must be mounted. Catches the
// uniform-failure class (0 mounted never passes a floor above 0) while
// tolerating a pod briefly below full mounts mid-handoff.
const defaultReadyMinMountedFraction = 0.5

// readyMinMountedFractionFromEnv reads the optional /readyz mount-floor
// override:
//
//	HOSTTHIS_READY_MIN_MOUNTED_FRACTION  (fraction in [0, 1], e.g. "0.5")
//
// Absent or empty returns the 0.5 default. "0" disables the floor - the
// value is passed straight to the shale predicate, whose f <= 0 contract
// is "no floor requested: always ready", so the disable semantic lives in
// shale, not in a hostthis special case. A value that does not parse as a
// finite number, or falls outside [0, 1], is a configuration error: the
// caller must fail startup loudly (the same fail-loud posture as
// HOSTTHIS_RETENTION and the shale dispatch timeouts), never run with a
// silently substituted floor.
func readyMinMountedFractionFromEnv() (float64, error) {
	const key = "HOSTTHIS_READY_MIN_MOUNTED_FRACTION"
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultReadyMinMountedFraction, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s=%q is not a valid fraction (use a number in [0, 1], e.g. 0.5)", key, raw)
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("%s=%q must be in [0, 1] (0 disables the mount floor, 1 requires every desired unit mounted)", key, raw)
	}
	return f, nil
}
