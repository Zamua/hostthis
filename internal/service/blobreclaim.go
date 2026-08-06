package service

import (
	"context"
	"log"
	"time"
)

// BlobReclaimer is how the blob plane frees bytes nothing references. It is a
// seam rather than a direct call so the sweep loop does not have to know how
// reachability is decided.
type BlobReclaimer interface {
	// ReclaimBlobs frees what it can prove is unreachable and reports how many
	// blobs it freed - or in dry-run, how many it would have freed.
	ReclaimBlobs(ctx context.Context, req ReclaimRequest) (freed int, err error)
}

// ReclaimRequest is everything one pass needs.
type ReclaimRequest struct {
	Now    time.Time
	DryRun bool
	Logger *log.Logger
}

// CollocatedReclaimer is the blob plane's GC: a blob carries a pointer
// co-committed with the record that owns it, so a record delete unbinds it in
// the same transaction. Only STAGED-but-never-bound bytes can be orphaned, and
// only once they are too old to belong to an upload still in flight.
//
// Nothing here consults a global keep-set. That is the property worth naming:
// reachability is a fact about each blob's own pointer, so no scan can return a
// partial answer and no delete can act on ABSENCE from a set.
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
