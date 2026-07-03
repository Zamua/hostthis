// audit.go - the `hostthisd audit-counters [--apply]` offline op mode.
//
// A one-shot subcommand (not the daemon) that recomputes every identity's
// shale byte counters from authoritative truth and, with --apply, overwrites
// each drifted counter. Active only when the binary is built with -tags
// slatedb (the shale backend + its counter are slatedb-gated); the !slatedb
// stub lives in audit_default.go. Mirrors the retired one-shot migration
// binary's shape (build the shale backend, run, exit) but as a hostthisd
// subcommand rather than a separate binary. See docs/SPEC.md "Offline audit
// that recomputes the counter from authoritative truth".

//go:build slatedb

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// runAuditCounters implements the offline counter audit. It builds the SAME
// shale metadata backend the daemon uses (openShaleRepoFromEnv - deliberately
// WITHOUT the reconcile loop, blob unit, debug server, or any listener), runs
// storage.AuditCounters, prints a per-owner drift report, and exits.
//
// SOUND ONLY WITH WRITES QUIESCED. The operator MUST stop every serving
// hostthisd (scale the Deployment to 0 replicas) before running this: with no
// concurrent upload / delete / reserve in flight, the cross-shard scan and the
// absolute counter overwrite have no conflict window. Run against a LIVE
// cluster it can under-count (the racy recompute the online reconciler
// deliberately never performs). The tool cannot enforce quiescence - it is an
// operator precondition (docs/SPEC.md).
//
// Default is a DRY RUN: report drift, write nothing. --apply performs the
// overwrites (the one place an absolute counter overwrite is permitted, and
// the reason it is gated behind this offline step).
func runAuditCounters(args []string, logger *log.Logger) error {
	fs := flag.NewFlagSet("audit-counters", flag.ExitOnError)
	apply := fs.Bool("apply", false, "overwrite each drifted counter with the recomputed value (default: dry-run, report only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "sqlite"))
	if backend != "shale" {
		return fmt.Errorf("audit-counters requires HOSTTHIS_METADATA_BACKEND=shale "+
			"(the byte counter is a shale-only construct: sqlite/slatedb sum live rows at read time); got %q", backend)
	}

	// Retention is unused by the counter recompute (the counter has no read-time
	// expiry awareness), but openShaleRepoFromEnv takes it; parse it the same way
	// the daemon does so the repo is configured identically.
	retention, err := parseRetention(os.Getenv("HOSTTHIS_RETENTION"), domain.DefaultRetention())
	if err != nil {
		return err
	}

	repo, err := openShaleRepoFromEnv(retention, logger)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := repo.Close(); cerr != nil {
			logger.Printf("close shale: %v", cerr)
		}
	}()

	mode := "DRY-RUN (no writes)"
	if *apply {
		mode = "APPLY (overwriting drifted counters)"
	}
	grace := storage.DefaultReserveGrace()
	logger.Printf("audit-counters starting: mode=%s reserveGrace=%s", mode, grace)
	logger.Printf("PRECONDITION: writes MUST be quiesced (every serving replica scaled to 0); a live cluster can under-count")

	result, err := repo.AuditCounters(time.Now().UTC(), grace, *apply)
	if err != nil {
		return err
	}

	if len(result.Findings) == 0 {
		logger.Printf("audit-counters: %d owner(s) examined, NO drift (every counter matches authoritative truth)", result.Owners)
		return nil
	}
	for _, f := range result.Findings {
		verb := "would set"
		if *apply {
			verb = "set"
		}
		logger.Printf("drift: identity=%s kind=%s stored=%d computed=%d delta=%+d (%s to computed)",
			f.Identity, f.Kind, f.Stored, f.Computed, f.Computed-f.Stored, verb)
	}
	if *apply {
		logger.Printf("audit-counters: %d owner(s) examined, %d drifted counter(s) corrected", result.Owners, result.Applied)
	} else {
		logger.Printf("audit-counters: %d owner(s) examined, %d drifted counter(s) found (DRY-RUN; re-run with --apply to correct)",
			result.Owners, len(result.Findings))
	}
	return nil
}
