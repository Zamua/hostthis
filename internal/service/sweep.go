package service

import (
	"context"
	"log"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// BlobOrphanSweeper reclaims staged-but-unbound bytes: the only way a blob can
// end up unreferenced, since every bound blob's pointer is co-committed with
// the record that owns it and unbound by that record's delete.
type BlobOrphanSweeper interface {
	SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error
}

// SweepRooms prunes the room-create ledger. Those markers are a rate limiter's
// sliding window, not content: past the window a marker can never change a
// future decision, so dropping it keeps the family bounded.
type SweepRooms interface {
	PruneOldRoomCreates(cutoff time.Time) (int, error)
}

// Sweep reclaims storage. Nothing here is time-based from the user's point of
// view: pastes, sites and rooms persist indefinitely (see docs/SPEC.md
// "Persistence"). What remains is reclaiming orphaned bytes and bounding
// rate-limit bookkeeping, neither of which is reachable by any request.
//
// No pass here deletes on ABSENCE from a scanned set, so no partial answer can
// destroy live data - a property the old content-addressed keep-set could only
// approximate with a guard.
type Sweep struct {
	// Blobs reclaims orphaned bytes; nil disables blob reclamation.
	Blobs BlobReclaimer
	Rooms SweepRooms // optional; nil disables the room-create prune

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
	var prunedCreates int
	if !s.DryRun && s.Rooms != nil {
		n, err := s.Rooms.PruneOldRoomCreates(now.Add(-domain.RoomCreateWindow))
		if err != nil {
			s.Logger.Printf("sweep: prune room_creates: %v", err)
		}
		prunedCreates = n
	}
	if s.DryRun {
		s.Logger.Printf("sweep[dry-run]: reclaimed nothing (room-create prune skipped). Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.")
		return
	}
	if blobCount > 0 || prunedCreates > 0 {
		s.Logger.Printf("sweep: reclaimed %d blob(s), pruned %d room-create row(s)", blobCount, prunedCreates)
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
