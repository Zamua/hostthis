// audit_default.go - stub for the `audit-counters` subcommand when the binary
// is NOT built with -tags slatedb. The real impl is in audit.go (which carries
// the matching build tag). The counter the audit corrects is a shale-only
// construct, so the audit only exists on the slatedb build.

//go:build !slatedb

package main

import (
	"fmt"
	"log"
)

func runAuditCounters(_ []string, _ *log.Logger) error {
	return fmt.Errorf(
		"audit-counters requires a binary built with -tags slatedb " +
			"(the byte counter it corrects is a shale-only construct); " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}
