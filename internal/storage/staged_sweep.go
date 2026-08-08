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
// progress, and reports how many uploads it actually reclaimed - never how many
// it looked at.
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
	var reclaimed int
	var firstErr error
	for _, slug := range slugs {
		// reclaimStagedBytes applies the grace itself, so an upload still in
		// flight is left alone here without this loop knowing how to decide it.
		//
		// One bad upload does not stop the sweep: this runs at boot on a
		// serving node, and a single unreclaimable record must not deny the
		// sweep to every other.
		did, rerr := r.reclaimStagedBytes(ctx, slug, now)
		if rerr != nil {
			r.repoLog().Printf("shale: reclaiming staged bytes for %s: %v", slug, rerr)
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		if did {
			reclaimed++
		}
	}
	r.repoLog().Printf("shale: staged sweep: %d upload(s) with staged bytes on mounted units, %d past the %s grace and reclaimed",
		len(slugs), reclaimed, ResolveGrace)
	return reclaimed, firstErr
}

// stagedUploadsLocal returns every slug with LIVE staged records on this node's
// units.
//
// Tombstones are skipped. A cleared record is a deleted key, and a deleted key
// keeps returning from a scan until compaction - so counting them would report
// every upload that ever committed as an upload with staged bytes, which is the
// opposite of what the number is read as.
//
// An UNREADABLE record still counts. Skipping it would hide bytes nothing can
// name; counting it hands the slug to reclaimStagedBytes, which refuses the
// whole upload and logs, so the failure is visible instead of silent.
//
// Everything else is left to reclaimStagedBytes, which re-reads the records: how
// old they are, and whether they are worth acting on at all.
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
			if isTombstone(v) {
				continue
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

// StagedUploadsLocalForTest exposes the candidate scan so the blob-plane suite
// can assert on WHICH uploads it reports, which the reclaimed count cannot show.
func (r *ShaleRepo) StagedUploadsLocalForTest() ([]domain.Slug, error) {
	return r.stagedUploadsLocal()
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

// isTombstone reports whether a scanned record is a deleted key. LocalScanPrefix
// returns the stored bytes, so the envelope comes off here; a value that will
// not decode is NOT a tombstone, and deliberately stays a candidate.
func isTombstone(v []byte) bool {
	payload, err := stripEnvelope(v)
	return err == nil && len(payload) == 0
}
