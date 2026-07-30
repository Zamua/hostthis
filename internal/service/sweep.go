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
	// removes the expiry-index entry REGARDLESS, so an orphaned entry whose
	// paste is already gone cannot resurface on the next scan. The bool
	// reports whether a record was actually deleted; only those count into
	// the sweep's deleted total. See docs/SPEC.md "The storage contract"
	// (Expiry).
	DeleteExpired(ref domain.ExpiredPaste) (bool, error)
	// ReferencedBlobSHAs returns the blob content-SHAs still referenced by a
	// LIVE (non-deleted) version or paste head. The sweep GCs any blob NOT in
	// this set, so returning an empty set while pastes exist would delete
	// every blob; Once guards that with an abort-on-zero-refs check.
	ReferencedBlobSHAs() ([]string, error)
}

// SweepBlobs is the blob-side interface for the global content-addressed GC on
// the standalone disk path. On the transactional shale-blob path a delete
// unbinds the pointer inside the metadata-delete transaction, so this global GC
// is NOT wired there (Blobs is nil) and orphan BYTES are reclaimed by
// BlobOrphanSweeper instead.
type SweepBlobs interface {
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// BlobOrphanSweeper reclaims staged-but-unbound blob objects on the shale-blob
// path: a crash between StageBlob and the metadata commit leaves a unit-local
// object no transaction ever bound. Nil on every other path, where the
// metadata-delete unbinds bound blobs and only never-bound stragglers remain.
// The grace MUST exceed the longest stage-to-commit window, or an in-flight
// upload's object is swept out from under it.
type BlobOrphanSweeper interface {
	SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error
}

// SweepSites is the optional site-side interface. When wired, the sweep also
// expires sites and unions the blob SHAs their manifests reference into the
// keep-alive set, so a blob shared between a site and a paste (or two sites)
// survives as long as ANY live record references it. Nil disables site
// sweeping. The expiry pair mirrors SweepRepo's contract exactly (docs/SPEC.md
// "Static-site storage", sweep path).
type SweepSites interface {
	// ExpiredSites returns one reference per site whose expiry is at or
	// before now (inclusive boundary): the slug plus, on index-backed
	// backends, an opaque reference to the exact expiry_sites/ entry that
	// surfaced it (round-tripped into DeleteExpiredSite).
	ExpiredSites(now time.Time) ([]domain.ExpiredSite, error)
	// DeleteExpiredSite mirrors DeleteExpired for sites: delete the record
	// when it still exists, drop the expiry-index entry REGARDLESS, and
	// report whether a record was actually deleted.
	DeleteExpiredSite(ref domain.ExpiredSite) (bool, error)
	ReferencedSiteBlobSHAs() ([]string, error)
}

// SweepRooms is the optional room-side interface. When wired, the sweep also
// expires rooms whose inactivity window has passed (deleting each room and, via
// the storage cascade, every value in its namespace) and prunes the bounded
// room-creation rate-limit table. Room values live in the metadata store rather
// than the content-addressed BlobStore, so rooms contribute nothing to the
// blob-GC keep-alive set. Nil disables room sweeping. The expiry pair mirrors
// SweepRepo's contract exactly (docs/SPEC.md "Room storage on the slatedb (and
// shale) backend", sweep path).
type SweepRooms interface {
	// ExpiredRooms returns one reference per room whose expiry is at or
	// before now (inclusive boundary): the (app-slug, room-id) pair plus,
	// on index-backed backends, an opaque reference to the exact
	// roomexpiry/ entry that surfaced it (round-tripped into
	// DeleteExpiredRoom).
	ExpiredRooms(now time.Time) ([]domain.ExpiredRoom, error)
	// DeleteExpiredRoom mirrors DeleteExpired for rooms: delete the record
	// (cascading to its namespace) when it still exists, drop the
	// expiry-index entry REGARDLESS, and report whether a record was
	// actually deleted.
	DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error)
	PruneOldRoomCreates(cutoff time.Time) (int, error)
}

// Sweep runs the periodic expiry job: deletes records whose expiry has passed,
// garbage-collects blobs no longer referenced, and prunes stale rate-limit rows.
type Sweep struct {
	Repo  SweepRepo
	Blobs SweepBlobs // optional; nil on the shale-blob path (see BlobOrphans)
	Sites SweepSites // optional; nil disables site expiry + site-ref GC protection
	Rooms SweepRooms // optional; nil disables room expiry + room-create prune
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
	// DryRun makes the sweep compute and log what it would expire and GC while
	// mutating nothing (no Delete, no Remove, no prune), so operators get
	// visibility before trusting a live sweep. A "disabled" sweep runs in this
	// mode rather than being a silent no-op. See docs/SPEC.md "Dry-run
	// (observability)".
	DryRun bool

	// guard classifies refs that RESURFACE after being processed (the delete
	// reported success but did not persist: data physically placed where the
	// routed delete cannot reach it, or a diverged replica resurrecting a
	// record) as UNREACHABLE, so the sweep skips them instead of re-processing
	// the same refs every pass forever. Live mode only; see docs/SPEC.md
	// "Sweep convergence guard (unreachable refs)".
	guard refGuard
}

// maxGuardRefs bounds the convergence guard's memory. Above the cap the guard
// fails OPEN: new refs are processed as if unguarded and the overflow is logged,
// so a pathological keyspace costs repeat work but never unbounded memory.
const maxGuardRefs = 200_000

// refGuard is the sweep's cross-pass convergence-guard state. One pass records
// the ref ids it processed; the next classifies any of those that RESURFACE in
// its scan as unreachable and skips them. Ids that stop appearing are forgotten
// (externally purged, or the store converged), and a process restart clears
// everything, so each boot re-attempts once. Not safe for concurrent use; the
// sweep runs passes sequentially.
type refGuard struct {
	lastAttempted map[string]struct{} // ids processed in the previous pass
	unreachable   map[string]struct{} // ids that resurfaced after processing
}

// guardPass is one pass's view of the guard: skip consults and records, finish
// folds the pass back into the cross-pass state.
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

// skip reports whether the ref with this id is unreachable (already classified,
// or classified now because it resurfaced). A false return means process it; the
// id is recorded so that resurfacing next pass classifies it.
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

// finish folds the pass into the cross-pass state. A COMPLETED pass replaces
// lastAttempted and prunes unreachable ids that stopped appearing. An ABORTED
// pass only merges what it attempted: its scan view is partial, so pruning
// against it would wrongly forget ids.
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

// Guard ids are kind-prefixed so paste, site and room refs can never collide.
// Index-backed refs key on the exact index entry (the thing that resurfaces);
// indexless backends key on the slug.
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

// sweepExpired runs one record family's expiry loop (paste, site, or room) over
// the refs its scan surfaced, under the counter semantics all three share:
//
//   - Dry-run logs every ref and counts it into deleted. A mutation-free pass
//     cannot tell a live record from an orphaned entry, so the would-expire
//     count is the entry count, an upper bound on real deletions.
//   - A live pass consults the convergence guard first; a ref skipped as
//     unreachable counts into NEITHER total (guardPass tracks its own count).
//   - A processed ref counts into deleted when a record was actually deleted,
//     into cleaned when the record was already gone (index maintenance, not a
//     deletion).
//
// A processing error aborts the family's loop and returns the counts so far
// with the error wrapped by wrapErr.
func sweepExpired[T any](s *Sweep, guard *guardPass, refs []T, guardID func(T) string, process func(T) (bool, error), dryRunLog func(T), wrapErr func(T, error) error) (deleted, cleaned int, err error) {
	for _, ref := range refs {
		if s.DryRun {
			dryRunLog(ref)
			deleted++
			continue
		}
		if guard.skip(guardID(ref)) {
			continue
		}
		ok, perr := process(ref)
		if perr != nil {
			return deleted, cleaned, wrapErr(ref, perr)
		}
		if ok {
			deleted++
		} else {
			cleaned++
		}
	}
	return deleted, cleaned, nil
}

// DefaultOrphanGrace is the fallback for Sweep.OrphanGrace. An hour comfortably
// exceeds the longest stage-to-commit window, so an in-flight upload's object is
// never swept.
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

// Run blocks, sweeping every Interval until ctx is canceled.
func (s *Sweep) Run(ctx context.Context) {
	// Sweep immediately on boot so a fresh container catches up on anything
	// that expired while it was down.
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

// tick runs one sweep pass. Errors are logged but do not stop the loop: the goal
// is eventual consistency, not failing loudly.
func (s *Sweep) tick() {
	now := s.Now().UTC()
	pasteCount, blobCount, err := s.Once(now)
	if err != nil {
		s.Logger.Printf("sweep: %v", err)
	}
	// The rate-limit prunes mutate, so dry-run skips them.
	var prunedKeys, prunedCreates int
	if !s.DryRun {
		if s.KeyGate != nil {
			n, err := s.KeyGate.PruneOldRows(now)
			if err != nil {
				s.Logger.Printf("sweep: prune key_first_seen: %v", err)
			}
			prunedKeys = n
		}
		// Rows past the creation window can never affect a future decision,
		// so they are safe to drop every tick (as with the key gate).
		if s.Rooms != nil {
			n, err := s.Rooms.PruneOldRoomCreates(now.Add(-domain.RoomCreateWindow))
			if err != nil {
				s.Logger.Printf("sweep: prune room_creates: %v", err)
			}
			prunedCreates = n
		}
		// Reclaim orphan blob BYTES on the shale-blob path. Age-gated and
		// mounted-unit-local, so it slots into the per-node sweep. Nil on
		// every other path, where the global GC in Once reclaims instead.
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
		// Logged unconditionally so the operator sees the sweep ran and what
		// it found, even on a clean tick.
		s.Logger.Printf("sweep[dry-run]: WOULD delete %d expired record(s) + gc %d blob(s); deleted nothing (key-gate/room-create prune skipped). Set HOSTTHIS_SWEEP_DISABLED=false to enable live cleanup.",
			pasteCount, blobCount)
		return
	}
	if pasteCount > 0 || blobCount > 0 || prunedKeys > 0 || prunedCreates > 0 {
		s.Logger.Printf("sweep: deleted %d expired record(s), gc'd %d blob(s), pruned %d key-gate row(s), %d room-create row(s)",
			pasteCount, blobCount, prunedKeys, prunedCreates)
	}
}

// Once runs a single sweep pass and returns the counts. Exposed so tests can
// drive it deterministically. pastesDeleted counts expired sites and rooms too:
// all three are "records expired this tick" for the data-loss guard below.
func (s *Sweep) Once(now time.Time) (pastesDeleted, blobsGCd int, err error) {
	// Live mode only: dry-run mutates nothing, so nothing can fail to persist.
	// The pass folds back into the cross-pass state whether it completes or
	// aborts (an aborted pass merges without pruning).
	var guard *guardPass
	if !s.DryRun {
		guard = s.guard.beginPass()
		defer func() { guard.finish(err == nil) }()
	}

	expired, err := s.Repo.ExpiredPastes(now)
	if err != nil {
		return 0, 0, fmt.Errorf("expired pastes: %w", err)
	}
	// Entries whose record was already gone are index maintenance, not
	// deletions, so they stay out of pastesDeleted (which feeds the summary log
	// AND the abort-on-zero-refs guard below) and are reported separately.
	orphanEntries := 0
	d, c, err := sweepExpired(s, guard, expired, pasteGuardID, s.Repo.DeleteExpired,
		func(ref domain.ExpiredPaste) { s.Logger.Printf("sweep[dry-run]: would expire paste %q", ref.Slug) },
		func(ref domain.ExpiredPaste, err error) error { return fmt.Errorf("delete %q: %w", ref.Slug, err) })
	pastesDeleted += d
	orphanEntries += c
	if err != nil {
		return pastesDeleted, blobsGCd, err
	}

	// Sites, same contract as the paste loop.
	if s.Sites != nil {
		expiredSites, err := s.Sites.ExpiredSites(now)
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("expired sites: %w", err)
		}
		d, c, err := sweepExpired(s, guard, expiredSites, siteGuardID, s.Sites.DeleteExpiredSite,
			func(ref domain.ExpiredSite) { s.Logger.Printf("sweep[dry-run]: would expire site %q", ref.Slug) },
			func(ref domain.ExpiredSite, err error) error { return fmt.Errorf("delete site %q: %w", ref.Slug, err) })
		pastesDeleted += d
		orphanEntries += c
		if err != nil {
			return pastesDeleted, blobsGCd, err
		}
	}

	// Rooms, same contract again. Deleting one cascades in storage to every
	// value in its namespace. Rooms hold no blobs, so this leaves the
	// keep-alive set alone, but a real room expiry still counts as a record
	// expired this tick so the data-loss guard below cannot misfire on a tick
	// where only rooms expired.
	if s.Rooms != nil {
		expiredRooms, err := s.Rooms.ExpiredRooms(now)
		if err != nil {
			return pastesDeleted, 0, fmt.Errorf("expired rooms: %w", err)
		}
		d, c, err := sweepExpired(s, guard, expiredRooms, roomGuardID, s.Rooms.DeleteExpiredRoom,
			func(ref domain.ExpiredRoom) {
				s.Logger.Printf("sweep[dry-run]: would expire room %s/%s", ref.AppSlug, ref.ID)
			},
			func(ref domain.ExpiredRoom, err error) error {
				return fmt.Errorf("delete room %s/%s: %w", ref.AppSlug, ref.ID, err)
			})
		pastesDeleted += d
		orphanEntries += c
		if err != nil {
			return pastesDeleted, blobsGCd, err
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

	// On the shale-blob path a delete unbinds the pointer inside the
	// metadata-delete transaction, so there is no whole-store content-addressed
	// GC to run here (Blobs is nil) and orphan bytes are reclaimed by the
	// BlobOrphans sweep in tick().
	if s.Blobs == nil {
		return pastesDeleted, 0, nil
	}

	// Walk the store and remove any blob whose sha is not referenced. The
	// keep-alive set is the UNION of paste-referenced and site-referenced SHAs,
	// so a blob shared across record kinds survives as long as ANY live record
	// references it.
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
	// DATA-LOSS guard. Zero referenced shas, no records expired this tick (so
	// an empty set is not the expected after-effect of an expiry pass), and a
	// non-empty blob store together mean something is broken: a buggy repo
	// impl, schema misalignment, a partial restore. Proceeding would treat
	// every blob as an orphan and wipe the store, so refuse. The cost in the
	// legitimate "owner deleted their last paste, no new uploads" case is one
	// orphan blob left until the next upload.
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
			return nil // one failed file must not abort the walk
		}
		blobsGCd++
		return nil
	})
	if walkErr != nil {
		return pastesDeleted, blobsGCd, fmt.Errorf("walk blobs: %w", walkErr)
	}
	return pastesDeleted, blobsGCd, nil
}
