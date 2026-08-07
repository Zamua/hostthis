package service

import (
	"context"
	"log"
	"time"
)

// BlobOrphanSweeper reclaims staged-but-unbound bytes: the only way a blob can
// end up unreferenced, since every bound blob's pointer is co-committed with
// the record that owns it and unbound by that record's delete.
type BlobOrphanSweeper interface {
	SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error
}

// Sweep reclaims storage. Nothing here is time-based from the user's point of
// view: pastes, sites and rooms persist indefinitely (see docs/SPEC.md
// "Persistence"). What remains is reclaiming orphaned bytes and bounding
// rate-limit bookkeeping, neither of which is reachable by any request.
//
// No pass here deletes on ABSENCE from a scanned set, so no partial answer can
// destroy live data - a property the old content-addressed keep-set could only
// approximate with a guard.
// LegacySiteSweeper drains directories that predate the artifact model.
type LegacySiteSweeper interface {
	SweepLegacySites(ctx context.Context, now time.Time) (int, error)
}

type Sweep struct {
	// Blobs reclaims orphaned bytes; nil disables blob reclamation.
	Blobs BlobReclaimer

	// LegacySites drains a pre-unification family onto the current one. nil
	// when nothing is left to drain, which is the steady state - this is a
	// MIGRATION riding the loop until it converges, not a standing job, and it
	// goes when the family it drains does.
	//
	// It belongs on a schedule rather than at boot alone because which units a
	// node owns is still settling while a rollout is in flight: a sweep that
	// runs then can legitimately see nothing, and nothing would run again.
	LegacySites LegacySiteSweeper
	// legacyReported gates the zero-result report to the first pass. Touched
	// only from the loop goroutine.
	legacyReported bool

	Interval time.Duration
	Logger   *log.Logger
	Now      func() time.Time
	// DryRun makes the sweep run without mutating anything, so a risky change
	// can be deployed and observed before it is trusted. A "disabled" sweep
	// runs in this mode rather than being a silent no-op.
	DryRun bool
}

func NewSweep(logger *log.Logger) *Sweep {
	return &Sweep{
		Interval: time.Hour,
		Logger:   logger,
		Now:      time.Now,
	}
}

func (s *Sweep) Run(ctx context.Context) {
	s.tick()
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Sweep) tick() {
	now := s.Now().UTC()
	blobCount, err := s.Once(now)
	if err != nil {
		s.Logger.Printf("sweep: %v", err)
	}
	if s.DryRun {
		s.Logger.Printf("sweep[dry-run]: reclaimed nothing. Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.")
		return
	}
	if blobCount > 0 {
		s.Logger.Printf("sweep: reclaimed %d blob(s)", blobCount)
	}
	s.sweepLegacySites()
}

// sweepLegacySites drains what it can see this pass. Errors are logged, never
// propagated: a migration that cannot finish must not stop the reclamation the
// rest of this tick performs.
func (s *Sweep) sweepLegacySites() {
	if s.LegacySites == nil {
		return
	}
	moved, err := s.LegacySites.SweepLegacySites(context.Background(), s.Now().UTC())
	if err != nil {
		s.Logger.Printf("sweep: legacy sites: %v (retried next pass)", err)
	}
	// The first pass reports even a zero result. A drain that speaks only when
	// it moves something is indistinguishable from one that was never wired,
	// and those call for opposite responses: wait, or go fix the wiring.
	if moved > 0 || !s.legacyReported {
		s.legacyReported = true
		s.Logger.Printf("sweep: migrated %d legacy directory(s)", moved)
	}
}

// Once runs one blob-reclaim pass and reports how many blobs it freed.
func (s *Sweep) Once(now time.Time) (blobsGCd int, err error) {
	if s.Blobs == nil {
		return 0, nil
	}
	return s.Blobs.ReclaimBlobs(context.Background(), ReclaimRequest{
		Now:    now,
		DryRun: s.DryRun,
		Logger: s.Logger,
	})
}
