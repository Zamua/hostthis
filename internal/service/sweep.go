package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// SweepRepo is the persistence interface the Sweep service needs.
//
// ReferencedBlobSHAs is the blob-GC keep-set: every content hash a live paste
// or version still points at. It is the ONLY thing standing between a blob and
// deletion, so an implementation must fail rather than return a partial set -
// the sweep acts on ABSENCE from this set.
type SweepRepo interface {
	ReferencedBlobSHAs() ([]string, error)
}

// SweepBlobs is the standalone (content-addressed) blob store's GC surface.
type SweepBlobs interface {
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// BlobOrphanSweeper reclaims staged-but-unbound bytes on the shale-blob path,
// where there is no whole-store content-addressed walk to run.
type BlobOrphanSweeper interface {
	SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error
}

// SweepSites contributes the site family's blob references to the keep-set.
// Without it a site's bytes look unreferenced and are deleted.
type SweepSites interface {
	ReferencedSiteBlobSHAs() ([]string, error)
}

// SweepRooms prunes the room-create ledger. Those markers are a rate limiter's
// sliding window, not content: past the window a marker can never change a
// future decision, so dropping it keeps the family bounded.
type SweepRooms interface {
	PruneOldRoomCreates(cutoff time.Time) (int, error)
}

// Sweep reclaims storage. Nothing here is time-based from the user's point of
// view: pastes, sites and rooms persist indefinitely (see docs/SPEC.md
// "Persistence"). What remains is garbage collection of bytes and of rate-limit
// bookkeeping, neither of which is reachable by any request.
type Sweep struct {
	Repo SweepRepo
	// Blobs is the blob plane's reclaimer. Which plane is in play is decided at
	// composition (see BlobReclaimer); nil disables blob GC entirely.
	Blobs    BlobReclaimer
	Sites    SweepSites // optional; nil omits site refs from the keep-set
	Rooms    SweepRooms // optional; nil disables the room-create prune
	Interval time.Duration
	Logger   *log.Logger
	Now      func() time.Time
	// DryRun makes the sweep compute and log what it would GC while mutating
	// nothing, so operators get visibility before trusting a live sweep. A
	// "disabled" sweep runs in this mode rather than being a silent no-op.
	DryRun bool
}

// NewSweep wires the detached-store plane, the default for dev and for any
// deploy whose blobs are not shale-collocated.
func NewSweep(repo SweepRepo, blobs SweepBlobs, logger *log.Logger) *Sweep {
	s := &Sweep{
		Repo:     repo,
		Interval: time.Hour,
		Logger:   logger,
		Now:      time.Now,
	}
	if blobs != nil {
		s.Blobs = DetachedStoreReclaimer{Blobs: blobs}
	}
	return s
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
		s.Logger.Printf("sweep[dry-run]: WOULD gc %d blob(s); deleted nothing (room-create prune skipped). Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.", blobCount)
		return
	}
	if blobCount > 0 || prunedCreates > 0 {
		s.Logger.Printf("sweep: gc'd %d blob(s), pruned %d room-create row(s)", blobCount, prunedCreates)
	}
}

// Once runs one blob-GC pass and reports how many blobs it reclaimed.
func (s *Sweep) Once(now time.Time) (blobsGCd int, err error) {
	if s.Blobs == nil {
		return 0, nil
	}
	return s.Blobs.ReclaimBlobs(context.Background(), ReclaimRequest{
		Now:     now,
		DryRun:  s.DryRun,
		Logger:  s.Logger,
		KeepSet: s.keepSet,
	})
}

// keepSet unions the paste-side and site-side references. It is resolved lazily
// by the reclaimer, so a plane that does not need it never triggers the
// cross-shard scan.
func (s *Sweep) keepSet() (map[string]struct{}, error) {
	refs, err := s.Repo.ReferencedBlobSHAs()
	if err != nil {
		return nil, fmt.Errorf("referenced shas: %w", err)
	}
	set := make(map[string]struct{}, len(refs))
	for _, sha := range refs {
		set[sha] = struct{}{}
	}
	if s.Sites != nil {
		siteRefs, err := s.Sites.ReferencedSiteBlobSHAs()
		if err != nil {
			return nil, fmt.Errorf("referenced site shas: %w", err)
		}
		for _, sha := range siteRefs {
			set[sha] = struct{}{}
		}
	}
	return set, nil
}
