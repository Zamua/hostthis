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
	// ExpiredPastes returns one reference per paste whose expires_at is at
	// or before now (inclusive boundary): the slug plus, on index-backed
	// backends, an opaque reference to the exact expiry-index entry that
	// surfaced it (round-tripped into DeleteExpired).
	ExpiredPastes(now time.Time) ([]domain.ExpiredPaste, error)
	// DeleteExpired processes one expired reference: it deletes the paste
	// record when it still exists (the same full cascade as Delete) and
	// removes the expiry-index entry REGARDLESS, so an orphaned entry (its
	// paste already gone) cannot resurface on the next scan. Returns
	// whether a paste record was actually deleted - the sweep counts only
	// those in its deleted total. See docs/SPEC.md "The storage contract"
	// (Expiry).
	DeleteExpired(ref domain.ExpiredPaste) (bool, error)
	// ReferencedBlobSHAs returns the set of blob content-SHAs still
	// referenced by a LIVE (non-deleted) version or paste head. The
	// sweep GCs any blob NOT in this set. Returning an empty/nil set
	// while pastes exist would delete every blob, so the sweep has an
	// abort-on-zero-refs guard (see Once).
	ReferencedBlobSHAs() ([]string, error)
}

// SweepBlobs is the blob-side interface for the global content-addressed GC
// (the standalone disk/S3 path). On the transactional shale-blob path the blobs
// live in the cluster and a delete unbinds the pointer in the metadata-delete
// transaction, so this global GC is NOT wired there (Blobs is nil); orphan
// BYTES are reclaimed by BlobOrphanSweeper instead.
type SweepBlobs interface {
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// BlobOrphanSweeper reclaims staged-but-unbound blob objects (the shale-blob
// path's SweepOrphans). A blob that was staged but whose binding transaction
// never committed (a crash between StageBlob and the metadata commit) leaves a
// unit-local orphan object; this age-gated, mounted-unit-local pass reclaims it.
// storage.ShaleRepo.SweepBlobOrphans satisfies it. Nil on every path except the
// shale-blob one, where the metadata-delete already unbinds bound blobs and this
// only mops up the never-bound stragglers. The grace MUST exceed the longest
// stage->commit window so an in-flight upload's object is never swept.
type BlobOrphanSweeper interface {
	SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error
}

// SweepSites is the optional site-side interface. When wired, the sweep
// also expires sites and unions the blob SHAs their manifests reference
// into the keep-alive set, so a blob shared between a site and a paste
// (or two sites) survives as long as ANY live record references it.
// internal/storage.SiteRepo satisfies it. Nil disables site sweeping.
// The expiry pair mirrors SweepRepo's contract exactly (see docs/SPEC.md
// "Static-site storage", sweep path).
type SweepSites interface {
	// ExpiredSites returns one reference per site whose expiry is at or
	// before now (inclusive boundary): the slug plus, on index-backed
	// backends, an opaque reference to the exact expiry_sites/ entry that
	// surfaced it (round-tripped into DeleteExpiredSite).
	ExpiredSites(now time.Time) ([]domain.ExpiredSite, error)
	// DeleteExpiredSite processes one expired reference: it deletes the
	// site record when it still exists (the same full cascade as the
	// owner-facing Delete) and removes the expiry-index entry REGARDLESS,
	// so an orphaned entry (its record already gone) cannot resurface on
	// the next scan. Returns whether a site record was actually deleted -
	// the sweep counts only those in its deleted total.
	DeleteExpiredSite(ref domain.ExpiredSite) (bool, error)
	ReferencedSiteBlobSHAs() ([]string, error)
}

// SweepRooms is the optional room-side interface. When wired, the sweep
// also expires rooms (whose 30-day inactivity window has passed),
// deleting each room and - via the storage FK cascade - every value in
// its namespace, and prunes the bounded room-creation rate-limit table.
// Rooms hold no blobs (their values live in the metadata store, not the
// content-addressed BlobStore), so they contribute nothing to the
// blob-GC keep-alive set. internal/storage.RoomKVRepo satisfies it. Nil
// disables room sweeping. The expiry pair mirrors SweepRepo's contract
// exactly (see docs/SPEC.md "Room storage on the slatedb (and shale)
// backend", sweep path).
type SweepRooms interface {
	// ExpiredRooms returns one reference per room whose expiry is at or
	// before now (inclusive boundary): the (app-slug, room-id) pair plus,
	// on index-backed backends, an opaque reference to the exact
	// roomexpiry/ entry that surfaced it (round-tripped into
	// DeleteExpiredRoom).
	ExpiredRooms(now time.Time) ([]domain.ExpiredRoom, error)
	// DeleteExpiredRoom processes one expired reference: it deletes the
	// room record (cascading to every value in its namespace) when it
	// still exists and removes the expiry-index entry REGARDLESS, so an
	// orphaned entry (its record already gone) cannot resurface on the
	// next scan. Returns whether a room record was actually deleted -
	// the sweep counts only those in its deleted total.
	DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error)
	PruneOldRoomCreates(cutoff time.Time) (int, error)
}

// Sweep runs the periodic expiry job: deletes pastes (and sites) whose
// expires_at has passed, garbage-collects any blob files no longer
// referenced, and prunes stale rate-limit rows from the key gate.
type Sweep struct {
	Repo  SweepRepo
	Blobs SweepBlobs // optional; nil on the shale-blob path (the cluster owns the blobs; see BlobOrphans)
	Sites SweepSites // optional; nil disables site expiry + site-ref GC protection
	Rooms SweepRooms // optional; nil disables room expiry + room-create prune
	// BlobOrphans is the shale-blob path's orphan-bytes reclaimer (SweepOrphans).
	// Optional + nil on the standalone path (the global content-addressed GC via
	// Blobs is the GC there). When set, each tick runs an age-gated, mounted-
	// unit-local orphan sweep; Blobs is left nil on that path (the metadata
	// delete already unbinds bound blobs, so there is no whole-store content-
	// addressed GC to run).
	BlobOrphans BlobOrphanSweeper
	// OrphanGrace is the age a staged-but-unbound blob object must exceed before
	// the orphan sweep reclaims it. It MUST exceed the longest stage->commit
	// window so an in-flight upload's object is never swept. Zero falls back to
	// DefaultOrphanGrace.
	OrphanGrace time.Duration
	KeyGate     *KeyGate // optional; nil disables the key_first_seen prune
	Interval    time.Duration
	Logger      *log.Logger
	Now         func() time.Time
	// DryRun, when true, makes the sweep COMPUTE + LOG what it would expire
	// and GC but mutate nothing (no Delete, no Remove, no prune). It gives
	// operators visibility before trusting a live sweep. A "disabled" sweep
	// runs in this mode - it is never a silent no-op. See docs/SPEC.md
	// "Dry-run (observability)".
	DryRun bool

	// guard is the cross-pass convergence guard: it classifies refs that
	// RESURFACE after being processed (the delete reported success but did
	// not persist - e.g. legacy data physically placed where the routed
	// delete cannot reach it, or a diverged replica resurrecting a record)
	// as UNREACHABLE, so the sweep skips them instead of re-processing the
	// same refs every pass forever. Live mode only; see docs/SPEC.md
	// "Sweep convergence guard (unreachable refs)".
	guard refGuard
}

// maxGuardRefs bounds the convergence guard's memory (ids tracked across
// passes). Above the cap the guard fails OPEN: new refs are processed as if
// unguarded and the overflow is logged, so a pathological keyspace can cost
// repeat work but never unbounded memory.
const maxGuardRefs = 200_000

// refGuard is the sweep's cross-pass convergence-guard state. One pass
// records the ref ids it processed; the next pass classifies any of those
// ids that RESURFACE in its scan as unreachable and skips them. Ids that
// stop appearing are forgotten (externally purged / store converged), and a
// process restart clears everything, so each boot re-attempts once.
// Not safe for concurrent use; the sweep runs passes sequentially.
type refGuard struct {
	lastAttempted map[string]struct{} // ids processed in the previous pass
	unreachable   map[string]struct{} // ids that resurfaced after processing
}

// guardPass is one pass's view of the guard: skip() consults + records,
// finish() folds the pass back into the cross-pass state.
type guardPass struct {
	g         *refGuard
	attempted map[string]struct{} // ids processed THIS pass
	seen      map[string]struct{} // every id surfaced THIS pass
	skipped   int                 // refs skipped as unreachable this pass
	overflow  bool                // cap hit; guard failing open for new ids
}

func (g *refGuard) beginPass() *guardPass {
	if g.lastAttempted == nil {
		g.lastAttempted = make(map[string]struct{})
	}
	if g.unreachable == nil {
		g.unreachable = make(map[string]struct{})
	}
	return &guardPass{
		g:         g,
		attempted: make(map[string]struct{}),
		seen:      make(map[string]struct{}),
	}
}

// skip reports whether the ref with this id must be skipped this pass
// (known or newly-classified unreachable). A false return means the caller
// should process the ref; the id is recorded so a resurfacing next pass
// classifies it.
func (p *guardPass) skip(id string) bool {
	p.seen[id] = struct{}{}
	if _, ok := p.g.unreachable[id]; ok {
		p.skipped++
		return true
	}
	if _, ok := p.g.lastAttempted[id]; ok {
		// Processed last pass and back again: the mutation did not persist.
		if len(p.g.unreachable) < maxGuardRefs {
			p.g.unreachable[id] = struct{}{}
		} else {
			p.overflow = true
		}
		p.skipped++
		return true
	}
	if len(p.attempted) < maxGuardRefs {
		p.attempted[id] = struct{}{}
	} else {
		p.overflow = true // fail open: processed but not tracked
	}
	return false
}

// finish folds the pass into the cross-pass state. A COMPLETED pass
// replaces lastAttempted and prunes unreachable ids that stopped appearing
// (externally drained). An ABORTED pass only merges what it attempted (its
// scan view is partial, so pruning against it would wrongly forget ids).
func (p *guardPass) finish(completed bool) {
	if completed {
		p.g.lastAttempted = p.attempted
		for id := range p.g.unreachable {
			if _, ok := p.seen[id]; !ok {
				delete(p.g.unreachable, id)
			}
		}
		return
	}
	for id := range p.attempted {
		if len(p.g.lastAttempted) >= maxGuardRefs {
			break
		}
		p.g.lastAttempted[id] = struct{}{}
	}
}

// Guard ids are kind-prefixed so a paste, site, and room ref can never
// collide. Index-backed refs key on the exact index entry (the thing that
// resurfaces); indexless backends key on the slug.
func pasteGuardID(ref domain.ExpiredPaste) string {
	if ref.IndexRef != "" {
		return "p\x00" + ref.IndexRef
	}
	return "p\x00slug\x00" + ref.Slug.String()
}

func siteGuardID(ref domain.ExpiredSite) string {
	if ref.IndexRef != "" {
		return "s\x00" + ref.IndexRef
	}
	return "s\x00slug\x00" + ref.Slug.String()
}

func roomGuardID(ref domain.ExpiredRoom) string {
	if ref.IndexRef != "" {
		return "r\x00" + ref.IndexRef
	}
	return fmt.Sprintf("r\x00%s\x00%s", ref.AppSlug, ref.ID)
}

// DefaultOrphanGrace is the fallback age a staged-but-unbound shale blob object
// must exceed before the orphan sweep reclaims it (used when Sweep.OrphanGrace
// is zero). One hour comfortably exceeds the longest stage->commit window (a
// multi-second blob upload), so an in-flight upload's object is never swept.
const DefaultOrphanGrace = time.Hour

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
		// In dry-run this surfaces what WOULD block a live sweep (e.g. the
		// fail-closed blob GC aborting on an undecodable record) without
		// having touched anything - exactly the visibility dry-run is for.
		s.Logger.Printf("sweep: %v", err)
	}
	// The rate-limit prunes mutate, so they're skipped in dry-run.
	var prunedKeys, prunedCreates int
	if !s.DryRun {
		if s.KeyGate != nil {
			n, err := s.KeyGate.PruneOldRows(now)
			if err != nil {
				s.Logger.Printf("sweep: prune key_first_seen: %v", err)
			}
			prunedKeys = n
		}
		// Prune the bounded room-creation rate-limit table. Rows past the
		// creation window can never affect a future decision, so they're
		// safe to drop on every tick (the same discipline as the key gate).
		if s.Rooms != nil {
			n, err := s.Rooms.PruneOldRoomCreates(now.Add(-domain.RoomCreateWindow))
			if err != nil {
				s.Logger.Printf("sweep: prune room_creates: %v", err)
			}
			prunedCreates = n
		}
		// Reclaim orphan blob BYTES on the shale-blob path (staged-but-never-
		// bound objects from a crash between StageBlob and the metadata commit).
		// Age-gated + mounted-unit-local, so it slots into the per-node sweep.
		// nil on every other path (the global content-addressed GC in Once is
		// the reclaimer there).
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
		// Always log in dry-run so the operator sees the sweep ran + what it
		// found, even on a clean (zero) tick.
		s.Logger.Printf("sweep[dry-run]: WOULD delete %d expired record(s) + gc %d blob(s); deleted nothing (key-gate/room-create prune skipped). Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.",
			pasteCount, blobCount)
		return
	}
	if pasteCount > 0 || blobCount > 0 || prunedKeys > 0 || prunedCreates > 0 {
		s.Logger.Printf("sweep: deleted %d expired record(s), gc'd %d blob(s), pruned %d key-gate row(s), %d room-create row(s)",
			pasteCount, blobCount, prunedKeys, prunedCreates)
	}
}

// Once runs a single sweep pass and returns the counts. Exposed so
// tests can drive it deterministically. The pastesDeleted count
// includes expired sites - both are "records expired this tick" for
// the purpose of the data-loss guard below.
func (s *Sweep) Once(now time.Time) (pastesDeleted, blobsGCd int, err error) {
	// The convergence guard runs in live mode only (dry-run mutates
	// nothing, so nothing can fail to persist). It is folded back into the
	// cross-pass state whether the pass completes or aborts; an aborted
	// pass merges without pruning (its scan view is partial).
	var guard *guardPass
	if !s.DryRun {
		guard = s.guard.beginPass()
		defer func() { guard.finish(err == nil) }()
	}

	expired, err := s.Repo.ExpiredPastes(now)
	if err != nil {
		return 0, 0, fmt.Errorf("expired pastes: %w", err)
	}
	// Orphaned expiry-index entries cleaned this pass: entries whose record
	// was already gone. They are index maintenance, not deletions, so they
	// don't count into pastesDeleted (which feeds the summary log AND the
	// abort-on-zero-refs guard below); reported separately.
	orphanEntries := 0
	for _, ref := range expired {
		if s.DryRun {
			// Dry-run mutates nothing, so it can't distinguish a live paste
			// from an orphaned entry; the would-expire count is the entry
			// count, an upper bound on real deletions.
			s.Logger.Printf("sweep[dry-run]: would expire paste %q", ref.Slug)
			pastesDeleted++
			continue
		}
		if guard.skip(pasteGuardID(ref)) {
			continue
		}
		deleted, err := s.Repo.DeleteExpired(ref)
		if err != nil {
			return pastesDeleted, blobsGCd, fmt.Errorf("delete %q: %w", ref.Slug, err)
		}
		if deleted {
			pastesDeleted++
		} else {
			orphanEntries++
		}
	}

	// Expire sites too, when the site sweeper is wired. Same contract as
	// the paste loop: only real record deletions count into pastesDeleted;
	// an orphaned-entry cleanup joins the cleaned count.
	if s.Sites != nil {
		expiredSites, err := s.Sites.ExpiredSites(now)
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("expired sites: %w", err)
		}
		for _, ref := range expiredSites {
			if s.DryRun {
				s.Logger.Printf("sweep[dry-run]: would expire site %q", ref.Slug)
				pastesDeleted++
				continue
			}
			if guard.skip(siteGuardID(ref)) {
				continue
			}
			deleted, err := s.Sites.DeleteExpiredSite(ref)
			if err != nil {
				return pastesDeleted, blobsGCd, fmt.Errorf("delete site %q: %w", ref.Slug, err)
			}
			if deleted {
				pastesDeleted++
			} else {
				orphanEntries++
			}
		}
	}

	// Expire rooms too, when the room sweeper is wired. Same contract as
	// the paste and site loops: only real record deletions count into
	// pastesDeleted; an orphaned-entry cleanup joins the cleaned count.
	// Deleting a room cascades (in storage) to every value in its
	// namespace. Rooms hold no blobs, so this does not touch the
	// keep-alive set - but a real room expiry still counts as a "record
	// expired this tick" so the data-loss guard below doesn't misfire on
	// a tick where only rooms expired.
	if s.Rooms != nil {
		expiredRooms, err := s.Rooms.ExpiredRooms(now)
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("expired rooms: %w", err)
		}
		for _, ref := range expiredRooms {
			if s.DryRun {
				s.Logger.Printf("sweep[dry-run]: would expire room %s/%s", ref.AppSlug, ref.ID)
				pastesDeleted++
				continue
			}
			if guard.skip(roomGuardID(ref)) {
				continue
			}
			deleted, err := s.Rooms.DeleteExpiredRoom(ref)
			if err != nil {
				return pastesDeleted, blobsGCd, fmt.Errorf("delete room %s/%s: %w", ref.AppSlug, ref.ID, err)
			}
			if deleted {
				pastesDeleted++
			} else {
				orphanEntries++
			}
		}
	}

	if orphanEntries > 0 {
		s.Logger.Printf("sweep: cleaned %d orphaned expiry-index entries (record already gone)", orphanEntries)
	}
	if guard != nil && guard.skipped > 0 {
		s.Logger.Printf("sweep: skipped %d unreachable expired ref(s) - processing reported success but did not persist (misplaced legacy data or replica divergence); external cleanup required", guard.skipped)
	}
	if guard != nil && guard.overflow {
		s.Logger.Printf("sweep: convergence-guard capacity (%d refs) exceeded; excess refs are re-processed unguarded", maxGuardRefs)
	}

	// GC unreferenced blobs. On the transactional shale-blob path the cluster
	// owns the blobs and a delete unbinds the pointer in the metadata-delete
	// transaction, so there is NO whole-store content-addressed GC to run here
	// (Blobs is nil); orphan BYTES (staged-but-never-bound) are reclaimed by the
	// BlobOrphans sweep in tick(). Skip the global GC entirely on that path.
	if s.Blobs == nil {
		return pastesDeleted, 0, nil
	}

	// The repo gives us the set of SHAs we DO reference; we walk disk and remove
	// any blob whose sha isn't in it. The keep-alive set is the UNION of paste-
	// referenced and site-referenced SHAs so a blob shared across record kinds
	// survives as long as ANY live record references it.
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
		if s.DryRun {
			s.Logger.Printf("sweep[dry-run]: would gc orphan blob %q", sha)
			blobsGCd++
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
