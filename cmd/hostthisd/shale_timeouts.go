// shale_timeouts.go - env parsing for the shale cluster's per-dispatch
// read/write deadlines (docs/SPEC.md "Dispatch deadlines: the read/write
// timeout knobs"). Deliberately OUTSIDE the slatedb build tag: the consumer
// (metadata_shale.go's openShaleRepoFromEnv) only compiles with -tags slatedb,
// but the parse contract is pure env+time logic, so keeping it untagged lets
// the default test suite pin it without the slatedb toolchain or MinIO.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// shaleTimeoutsFromEnv reads the optional shale dispatch-deadline overrides:
//
//	HOSTTHIS_SHALE_READ_TIMEOUT   (Go duration, e.g. "8s")
//	HOSTTHIS_SHALE_WRITE_TIMEOUT  (Go duration, e.g. "8s")
//
// Absent or empty leaves the returned value zero; storage.ShaleConfig passes
// zero through so the shale cluster default (5s each) applies, keeping a
// deployment that sets neither unchanged. A value that does not parse as a
// Go duration, or a negative one, is a configuration error: the caller must
// fail startup loudly (the same fail-loud posture as HOSTTHIS_RETENTION),
// never run with a silently substituted default the way envOr/envOrInt do
// for their softer knobs.
func shaleTimeoutsFromEnv() (readTimeout, writeTimeout time.Duration, err error) {
	if readTimeout, err = optionalDurationEnv("HOSTTHIS_SHALE_READ_TIMEOUT"); err != nil {
		return 0, 0, err
	}
	if writeTimeout, err = optionalDurationEnv("HOSTTHIS_SHALE_WRITE_TIMEOUT"); err != nil {
		return 0, 0, err
	}
	return readTimeout, writeTimeout, nil
}

// optionalDurationEnv parses key as a Go duration. Absent or
// empty/whitespace returns zero with no error (the "keep the downstream
// default" signal); a malformed or negative value returns an error naming
// the variable so startup fails with an actionable message.
func optionalDurationEnv(key string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid Go duration (use e.g. 8s, 500ms)", key, raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s=%q must not be negative", key, raw)
	}
	return d, nil
}
