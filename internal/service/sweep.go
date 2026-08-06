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
	Repo  SweepRepo
	Blobs SweepBlobs // optional; nil on the shale-blob path (see BlobOrphans)
	Sites SweepSites // optional; nil omits site refs from the keep-set
	Rooms SweepRooms // optional; nil disables the room-create prune
	// BlobOrphans is the shale-blob path's orphan-bytes reclaimer, running an
	// age-gated, mounted-unit-local pass per tick. Exactly one of BlobOrphans
	// and Blobs is set: the shale-blob path has no whole-store
	// content-addressed GC to run, the standalone path has no staging step.
	BlobOrphans BlobOrphanSweeper
	// OrphanGrace is the age a staged-but-unbound blob object must exceed before
	// the orphan sweep reclaims it. It MUST exceed the longest stage-to-commit
	// window, or an in-flight upload's object is swept. Zero falls back to
	// DefaultOrphanGrace.
	OrphanGrace time.Duration
	KeyGate     *KeyGate // optional; nil disables the key_first_seen prune
	Interval    time.Duration
	Logger      *log.Logger
	Now         func() time.Time
	// DryRun makes the sweep compute and log what it would GC while mutating
	// nothing, so operators get visibility before trusting a live sweep. A
	// "disabled" sweep runs in this mode rather than being a silent no-op.
	DryRun bool
}

// DefaultOrphanGrace must exceed the longest stage-to-commit window.
const DefaultOrphanGrace = time.Hour

func NewSweep(repo SweepRepo, blobs SweepBlobs, logger *log.Logger) *Sweep {
	return &Sweep{
		Repo:     repo,
		Blobs:    blobs,
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
	var prunedKeys, prunedCreates int
	if !s.DryRun {
		if s.KeyGate != nil {
			n, err := s.KeyGate.PruneOldRows(now)
			if err != nil {
				s.Logger.Printf("sweep: prune key_first_seen: %v", err)
			}
			prunedKeys = n
		}
		if s.Rooms != nil {
			n, err := s.Rooms.PruneOldRoomCreates(now.Add(-domain.RoomCreateWindow))
			if err != nil {
				s.Logger.Printf("sweep: prune room_creates: %v", err)
			}
			prunedCreates = n
		}
		if s.BlobOrphans != nil {
			grace := s.OrphanGrace
			if grace <= 0 {
				grace = DefaultOrphanGrace
			}
			if err := s.BlobOrphans.SweepBlobOrphans(context.Background(), now, grace); err != nil {
				s.Logger.Printf("sweep: orphan blob sweep: %v", err)
			}
		}
	}
	if s.DryRun {
		s.Logger.Printf("sweep[dry-run]: WOULD gc %d blob(s); deleted nothing (key-gate/room-create prune skipped). Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.", blobCount)
		return
	}
	if blobCount > 0 || prunedKeys > 0 || prunedCreates > 0 {
		s.Logger.Printf("sweep: gc'd %d blob(s), pruned %d key-gate row(s), %d room-create row(s)",
			blobCount, prunedKeys, prunedCreates)
	}
}

// Once runs one blob-GC pass and reports how many blobs it reclaimed.
func (s *Sweep) Once(now time.Time) (blobsGCd int, err error) {
	if s.Blobs == nil {
		return 0, nil
	}

	refs, err := s.Repo.ReferencedBlobSHAs()
	if err != nil {
		return 0, fmt.Errorf("referenced shas: %w", err)
	}
	refSet := make(map[string]struct{}, len(refs))
	for _, sha := range refs {
		refSet[sha] = struct{}{}
	}
	if s.Sites != nil {
		siteRefs, err := s.Sites.ReferencedSiteBlobSHAs()
		if err != nil {
			return 0, fmt.Errorf("referenced site shas: %w", err)
		}
		for _, sha := range siteRefs {
			refSet[sha] = struct{}{}
		}
	}

	// An empty keep-set alongside a non-empty store is a repo bug, not a store
	// full of garbage: this pass deletes on ABSENCE, so believing an empty set
	// deletes everything. Nothing is ever removed on the strength of a set that
	// came back empty.
	//
	// The check used to be conditional on no records having been swept this
	// tick. With expiry gone nothing is ever swept, so it is now unconditional
	// - which is both simpler and strictly safer than what it replaced.
	if len(refSet) == 0 {
		blobCount := 0
		if walkErr := s.Blobs.WalkBlobs(func(sha string) error { blobCount++; return nil }); walkErr != nil {
			return 0, fmt.Errorf("walk blobs (guard): %w", walkErr)
		}
		if blobCount > 0 {
			s.Logger.Printf("sweep: ABORTING blob GC - repo reports 0 referenced shas but blob store has %d objects; suspected repo bug. No blobs deleted.", blobCount)
			return 0, nil
		}
	}

	walkErr := s.Blobs.WalkBlobs(func(sha string) error {
		if _, ok := refSet[sha]; ok {
			return nil
		}
		if s.DryRun {
			s.Logger.Printf("sweep[dry-run]: would gc orphan blob %q", sha)
			blobsGCd++
			return nil
		}
		if err := s.Blobs.Remove(sha); err != nil {
			s.Logger.Printf("sweep: remove blob %q: %v", sha, err)
			return nil // one failed file must not abort the walk
		}
		blobsGCd++
		return nil
	})
	if walkErr != nil {
		return blobsGCd, fmt.Errorf("walk blobs: %w", walkErr)
	}
	return blobsGCd, nil
}
