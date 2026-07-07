// slatedb_walflush.go - env parsing for the slatedb WAL flush backstop
// (docs/SPEC.md "WAL flush backstop: bounding live WAL accumulation").
// Deliberately OUTSIDE the slatedb build tag: the consumer
// (metadata_shale.go's openShaleRepoFromEnv) only compiles with -tags
// slatedb, but the parse contract is pure env+int logic, so keeping it
// untagged lets the default test suite pin it without the slatedb toolchain
// or MinIO (the same shape as shale_timeouts.go).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// defaultMaxWALFlushesBeforeL0 is the daemon's default WAL flush backstop:
// force a slatedb L0 flush at least every 256 WAL flushes so a unit's live
// WAL count stays bounded near 256. slatedb's own default (4096) is the only
// flush trigger that ever fires at hostthis's tiny-value write profile (the
// 64 MiB memtable trigger never does), so without this cap each unit
// accumulates thousands of unreapable live WAL objects at low write rates.
const defaultMaxWALFlushesBeforeL0 = 256

// maxWALFlushesFromEnv reads the optional WAL-flush-backstop override:
//
//	HOSTTHIS_SLATEDB_MAX_WAL_FLUSHES  (non-negative integer)
//
// Absent or empty applies the hostthis default (256). "0" is the
// kill-switch: it returns zero, which storage.ShaleConfig passes through as
// "leave slatedb's own default untouched" (mirroring
// HOSTTHIS_SLATEDB_FENCE_GC=false). Any other positive integer is used
// as-is. A malformed or negative value is a configuration error: the caller
// must fail startup loudly (the same fail-loud posture as the
// dispatch-deadline knobs), never run with a silently substituted default.
func maxWALFlushesFromEnv() (int, error) {
	const key = "HOSTTHIS_SLATEDB_MAX_WAL_FLUSHES"
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultMaxWALFlushesBeforeL0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer (use e.g. 256, or 0 to keep the slatedb default)", key, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%q must not be negative (0 keeps the slatedb default)", key, raw)
	}
	return n, nil
}
