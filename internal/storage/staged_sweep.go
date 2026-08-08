// The sweep that reclaims bytes an abandoned upload staged and never bound.

package storage

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// SweepStagedBytes reclaims the staged objects of uploads that stopped making
// progress, and reports how many uploads it settled.
//
// Driven by the staged records rather than by durable intents, because the two
// answer different questions. An intent tracks a paste CREATION - it exists to
// decide whether half-written metadata should be finished or undone. Staged
// bytes outlive that: an append and a redeploy stage bytes without creating
// anything, and an upload that dies while staging never reaches the insert that
// would open an intent at all. That last case is the likeliest abandonment there
// is - a long multi-file deploy interrupted partway - so routing byte
// reclamation through intents would miss its main case.
//
// The scan is node-LOCAL, for the same reason the intent sweep's is: every unit
// is mounted by someone, so the fleet covers the keyspace without any node
// fanning out.
//
// Call it after the node is serving. Reclaiming reads and writes the slug's
// shard, which may not be mounted anywhere yet during a cold start.
func (r *ShaleRepo) SweepStagedBytes(ctx context.Context, now time.Time) (int, error) {
	if r.kv == nil {
		return 0, nil // no blob plane: nothing was ever staged through it
	}
	slugs, err := r.stagedUploadsLocal()
	if err != nil {
		return 0, err
	}
	if len(slugs) == 0 {
		return 0, nil
	}
	var settled int
	var firstErr error
	for _, slug := range slugs {
		// reclaimStagedBytes applies the grace itself, so an upload still in
		// flight is left alone here without this loop knowing how to decide it.
		//
		// One bad upload does not stop the sweep: this runs at boot on a
		// serving node, and a single unreclaimable record must not deny the
		// sweep to every other.
		if rerr := r.reclaimStagedBytes(ctx, slug, now); rerr != nil {
			r.repoLog().Printf("shale: reclaiming staged bytes for %s: %v", slug, rerr)
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		settled++
	}
	r.repoLog().Printf("shale: staged sweep: %d upload(s) with staged bytes on mounted units, %d past the %s grace and reclaimed",
		len(slugs), settled, ResolveGrace)
	return settled, firstErr
}

// stagedUploadsLocal returns every slug with staged records on this node's units.
//
// It reports candidates only. Whether an upload is old enough to touch is
// reclaimStagedBytes' decision, made from a fresh read of the records - so a
// tombstone or an unreadable record here costs nothing, and this scan never
// has to be right about anything except which slugs exist.
func (r *ShaleRepo) stagedUploadsLocal() ([]domain.Slug, error) {
	seen := make(map[domain.Slug]struct{})
	err := retryAcquiring(bootRetry, r.repoLog(), "staged-sweep", func() error {
		clear(seen)
		it, err := r.cluster.LocalScanPrefix(prefixStagedAll)
		if err != nil {
			return err
		}
		defer it.Close() //nolint:errcheck
		for {
			k, v, err := it.Next()
			if err != nil {
				return err
			}
			if k == nil && v == nil {
				return nil
			}
			if slug, ok := slugFromStagedKey(k); ok {
				seen[slug] = struct{}{}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.Slug, 0, len(seen))
	for slug := range seen {
		out = append(out, slug)
	}
	slices.Sort(out) // deterministic order, so a failing sweep logs the same way twice
	return out, nil
}

// slugFromStagedKey pulls the slug back out of staged/<slug>/<blobid>. A slug
// contains no slash, so one cut is enough.
func slugFromStagedKey(k []byte) (domain.Slug, bool) {
	rest, found := bytes.CutPrefix(k, prefixStagedAll)
	if !found {
		return "", false
	}
	s, id, found := bytes.Cut(rest, []byte("/"))
	if !found || len(s) == 0 || len(id) == 0 {
		return "", false
	}
	return domain.Slug(s), true
}
