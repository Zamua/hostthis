// shale_tombstone.go - env parsing for the shale tombstone-purge grace window
// (docs/SPEC.md "Tombstone purge: reclaiming replicated deletes"). Untagged for
// the same reason as shale_timeouts.go: the consumer only compiles with -tags
// slatedb, but the parse contract is pure env+time logic the default test
// suite can pin.
package main

import "time"

// tombstoneGraceFromEnv reads the optional purge grace window:
//
//	HOSTTHIS_METADATA_TOMBSTONE_GRACE  (Go duration, e.g. "168h")
//
// Absent or empty returns zero, which shale treats as purge-disabled. A value
// that does not parse, or a negative one, is a configuration error the caller
// must fail startup on rather than silently substituting a default.
func tombstoneGraceFromEnv() (time.Duration, error) {
	return optionalDurationEnv("HOSTTHIS_METADATA_TOMBSTONE_GRACE")
}
