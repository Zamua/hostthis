package service

import (
	"context"
	"fmt"
	"log"
	"time"
)

// BlobReclaimer is how a blob plane frees bytes nothing references. The two
// planes prove unreachability by completely different means, so the sweep
// selects a reclaimer ONCE at composition instead of branching per tick.
//
// One field, not two nullable ones: "both planes wired" and "neither wired"
// stop being representable, so the invariant is carried by the type rather
// than by a comment nobody re-reads.
type BlobReclaimer interface {
	// ReclaimBlobs frees what it can prove is unreachable and reports how many
	// blobs it freed - or in dry-run, how many it would have freed.
	ReclaimBlobs(ctx context.Context, req ReclaimRequest) (freed int, err error)
}

// ReclaimRequest is everything a plane might need for one pass. A plane uses
// what applies to it and ignores the rest.
type ReclaimRequest struct {
	Now    time.Time
	DryRun bool
	Logger *log.Logger

	// KeepSet resolves the shas a live record still references. It is a
	// FUNCTION rather than a value because resolving it is a cross-shard
	// fan-out: a plane that decides reachability some other way must be able
	// to skip that cost entirely, not receive a set it discards.
	KeepSet func() (map[string]struct{}, error)
}

// DetachedStoreReclaimer is the standalone content-addressed plane: bytes live
// in a store keyed by sha with no per-record pointer, so the only way to know a
// blob is unreachable is that no live record names it.
//
// This is the one reclaimer that acts on ABSENCE, which is why the zero-refs
// guard below is load-bearing rather than defensive.
type DetachedStoreReclaimer struct {
	Blobs SweepBlobs
}

func (d DetachedStoreReclaimer) ReclaimBlobs(_ context.Context, req ReclaimRequest) (int, error) {
	keep, err := req.KeepSet()
	if err != nil {
		return 0, err
	}

	// An empty keep-set alongside a non-empty store is a repo bug, not a store
	// full of garbage: this pass deletes on ABSENCE, so believing an empty set
	// deletes everything. Nothing is ever removed on the strength of a set that
	// came back empty.
	if len(keep) == 0 {
		blobCount := 0
		if walkErr := d.Blobs.WalkBlobs(func(string) error { blobCount++; return nil }); walkErr != nil {
			return 0, fmt.Errorf("walk blobs (guard): %w", walkErr)
		}
		if blobCount > 0 {
			req.Logger.Printf("sweep: ABORTING blob GC - repo reports 0 referenced shas but blob store has %d objects; suspected repo bug. No blobs deleted.", blobCount)
			return 0, nil
		}
	}

	freed := 0
	walkErr := d.Blobs.WalkBlobs(func(sha string) error {
		if _, ok := keep[sha]; ok {
			return nil
		}
		if req.DryRun {
			req.Logger.Printf("sweep[dry-run]: would gc orphan blob %q", sha)
			freed++
			return nil
		}
		if err := d.Blobs.Remove(sha); err != nil {
			req.Logger.Printf("sweep: remove blob %q: %v", sha, err)
			return nil // one failed file must not abort the walk
		}
		freed++
		return nil
	})
	if walkErr != nil {
		return freed, fmt.Errorf("walk blobs: %w", walkErr)
	}
	return freed, nil
}

// CollocatedReclaimer is the shale plane: a blob carries a pointer co-committed
// with the record that owns it, so reachability is decided per blob without
// consulting any global set. Only STAGED-but-never-bound bytes can be orphaned,
// and only after they are too old to belong to an upload still in flight.
//
// It never calls req.KeepSet, which is the point: the expensive cross-shard
// scan the detached plane needs has no counterpart here.
type CollocatedReclaimer struct {
	Sweeper BlobOrphanSweeper
	// Grace is how old a staged-but-unbound object must be before it is
	// reclaimed. It MUST exceed the longest stage-to-commit window, or an
	// in-flight upload's object is swept out from under it. Zero means
	// DefaultOrphanGrace.
	Grace time.Duration
}

func (c CollocatedReclaimer) ReclaimBlobs(ctx context.Context, req ReclaimRequest) (int, error) {
	if req.DryRun {
		// The orphan sweep has no compute-without-mutating mode, so dry-run
		// reports nothing rather than reporting a number it did not derive.
		return 0, nil
	}
	grace := c.Grace
	if grace <= 0 {
		grace = DefaultOrphanGrace
	}
	// The count is not surfaced: SweepBlobOrphans reclaims per mounted unit and
	// reports success, not a total.
	return 0, c.Sweeper.SweepBlobOrphans(ctx, req.Now, grace)
}

// DefaultOrphanGrace must exceed the longest stage-to-commit window.
const DefaultOrphanGrace = time.Hour
