package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// SweepRepo is the persistence interface the Sweep service needs.
// internal/storage.PasteRepo satisfies it.
type SweepRepo interface {
	ExpiredSlugs(now time.Time) ([]string, error)
	Delete(slug domain.Slug) error
	// ReferencedBlobSHAs returns the set of blob content-SHAs still
	// referenced by a LIVE (non-deleted) version or paste head. The
	// sweep GCs any blob NOT in this set. Returning an empty/nil set
	// while pastes exist would delete every blob, so the sweep has an
	// abort-on-zero-refs guard (see Once).
	ReferencedBlobSHAs() ([]string, error)
}

// SweepBlobs is the blob-side interface for GC.
type SweepBlobs interface {
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// SweepSites is the optional site-side interface. When wired, the sweep
// also expires sites and unions the blob SHAs their manifests reference
// into the keep-alive set, so a blob shared between a site and a paste
// (or two sites) survives as long as ANY live record references it.
// internal/storage.SiteRepo satisfies it. Nil disables site sweeping.
type SweepSites interface {
	ExpiredSiteSlugs(now time.Time) ([]string, error)
	Delete(slug domain.Slug) error
	ReferencedSiteBlobSHAs() ([]string, error)
}

// Sweep runs the periodic expiry job: deletes pastes (and sites) whose
// expires_at has passed, garbage-collects any blob files no longer
// referenced, and prunes stale rate-limit rows from the key gate.
type Sweep struct {
	Repo     SweepRepo
	Blobs    SweepBlobs
	Sites    SweepSites // optional; nil disables site expiry + site-ref GC protection
	KeyGate  *KeyGate   // optional; nil disables the key_first_seen prune
	Interval time.Duration
	Logger   *log.Logger
	Now      func() time.Time
}

// NewSweep wires defaults: 10-minute interval.
func NewSweep(repo SweepRepo, blobs SweepBlobs, logger *log.Logger) *Sweep {
	return &Sweep{
		Repo:     repo,
		Blobs:    blobs,
		Interval: 10 * time.Minute,
		Logger:   logger,
		Now:      time.Now,
	}
}

// Run blocks, sweeping every Interval until ctx is canceled. Called
// from main as a goroutine.
func (s *Sweep) Run(ctx context.Context) {
	// First sweep immediately on boot so a fresh container catches up
	// on anything that expired while it was down.
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

// tick runs one sweep pass. Errors are logged but don't stop the
// loop - the goal is "eventual consistency," not "fail loudly."
func (s *Sweep) tick() {
	now := s.Now().UTC()
	pasteCount, blobCount, err := s.Once(now)
	if err != nil {
		s.Logger.Printf("sweep: %v", err)
	}
	var prunedKeys int
	if s.KeyGate != nil {
		n, err := s.KeyGate.PruneOldRows(now)
		if err != nil {
			s.Logger.Printf("sweep: prune key_first_seen: %v", err)
		}
		prunedKeys = n
	}
	if pasteCount > 0 || blobCount > 0 || prunedKeys > 0 {
		s.Logger.Printf("sweep: deleted %d expired paste(s), gc'd %d blob(s), pruned %d key-gate row(s)",
			pasteCount, blobCount, prunedKeys)
	}
}

// Once runs a single sweep pass and returns the counts. Exposed so
// tests can drive it deterministically. The pastesDeleted count
// includes expired sites - both are "records expired this tick" for
// the purpose of the data-loss guard below.
func (s *Sweep) Once(now time.Time) (pastesDeleted, blobsGCd int, err error) {
	slugs, err := s.Repo.ExpiredSlugs(now)
	if err != nil {
		return 0, 0, fmt.Errorf("expired slugs: %w", err)
	}
	for _, slugStr := range slugs {
		if err := s.Repo.Delete(domain.Slug(slugStr)); err != nil {
			return pastesDeleted, blobsGCd, fmt.Errorf("delete %q: %w", slugStr, err)
		}
		pastesDeleted++
	}

	// Expire sites too, when the site sweeper is wired.
	if s.Sites != nil {
		siteSlugs, err := s.Sites.ExpiredSiteSlugs(now)
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("expired site slugs: %w", err)
		}
		for _, slugStr := range siteSlugs {
			if err := s.Sites.Delete(domain.Slug(slugStr)); err != nil {
				return pastesDeleted, blobsGCd, fmt.Errorf("delete site %q: %w", slugStr, err)
			}
			pastesDeleted++
		}
	}

	// GC unreferenced blobs. The repo gives us the set of SHAs we DO
	// reference; we walk disk and remove any blob whose sha isn't in it.
	// The keep-alive set is the UNION of paste-referenced and site-
	// referenced SHAs so a blob shared across record kinds survives as
	// long as ANY live record references it.
	refs, err := s.Repo.ReferencedBlobSHAs()
	if err != nil {
		return pastesDeleted, 0, fmt.Errorf("referenced shas: %w", err)
	}
	refSet := make(map[string]struct{}, len(refs))
	for _, sha := range refs {
		refSet[sha] = struct{}{}
	}
	if s.Sites != nil {
		siteRefs, err := s.Sites.ReferencedSiteBlobSHAs()
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("referenced site shas: %w", err)
		}
		for _, sha := range siteRefs {
			refSet[sha] = struct{}{}
		}
	}
	// Belt-and-suspenders DATA-LOSS guard: if the repo says zero shas
	// are referenced AND we didn't just delete any pastes (so an
	// empty referenced set isn't the expected after-effect of an
	// expiry pass) AND the blob store has blobs in it, something is
	// broken (buggy repo impl, schema misalignment, partial-restore).
	// Treating every blob as orphan and deleting them all would wipe
	// the bucket on the next tick - refuse instead. The cost in a
	// legitimate "user deleted their last paste, no new uploads"
	// edge case is leaving one orphan blob until the next upload -
	// strictly better than nuking the bucket.
	if len(refSet) == 0 && pastesDeleted == 0 {
		blobCount := 0
		if walkErr := s.Blobs.WalkBlobs(func(sha string) error { blobCount++; return nil }); walkErr != nil {
			return pastesDeleted, 0, fmt.Errorf("walk blobs (guard): %w", walkErr)
		}
		if blobCount > 0 {
			s.Logger.Printf("sweep: ABORTING blob GC - repo reports 0 referenced shas, no pastes were swept this tick, but blob store has %d objects; suspected repo bug. No blobs deleted.", blobCount)
			return pastesDeleted, 0, nil
		}
	}
	walkErr := s.Blobs.WalkBlobs(func(sha string) error {
		if _, ok := refSet[sha]; ok {
			return nil
		}
		if err := s.Blobs.Remove(sha); err != nil {
			s.Logger.Printf("sweep: remove blob %q: %v", sha, err)
			return nil // don't abort the walk on one failed file
		}
		blobsGCd++
		return nil
	})
	if walkErr != nil {
		return pastesDeleted, blobsGCd, fmt.Errorf("walk blobs: %w", walkErr)
	}
	return pastesDeleted, blobsGCd, nil
}
