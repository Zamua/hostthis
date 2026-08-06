// ShaleRepo's KeyGateRepo implementation (the Sybil rate limit): admit and
// snapshot over the keygate/<subnet>/<identity> rows plus their identity-leading
// view, each a single-{subnet}- or {identity}-shard op. The key layout and
// transaction model are documented in shale_repo.go.
//
// Out-of-window rows are dropped by the scans that already walk them rather
// than by a background pass, so the family stays bounded with no cross-shard
// fan-out and no schedule (docs/SPEC.md "Sybil rate limit").

//go:build slatedb

package storage

import (
	"errors"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"
)

// --- KeyGateRepo (Sybil rate limit) ----------------------------------------

// AdmitNewKey checks the subnet's in-window budget and admits a fresh
// (identity, subnet) pair on the {subnet} shard.
//
// The cap is APPROXIMATE under concurrency, not exact. The in-window count is a
// prefix scan, which shale does not permit inside a CAS transaction, and the
// transaction's read-set covers only the candidate's OWN row. Two first-sight
// identities in one subnet therefore write DIFFERENT keys, raise no conflict,
// and can both commit having each observed a count one below the limit. The
// overshoot is bounded by how many first-sight keys race in one subnet at once;
// a serial attacker sees the exact cap, and every admitted row counts against
// every later admit, so the gate does not decay. Making it exact needs a
// per-subnet counter in the transaction's read-set, which is a key-layout
// change (a new family plus its shaleShardKey routing), not a local edit.
//
// The candidate key IS read inside the tx, which is what makes a racing admit
// of the SAME pair resolve as known-already instead of writing the row twice.
func (r *ShaleRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, errors.New("identity + subnet required")
	}
	rowKey := shaleKeyKeygate(subnet, identity)

	// Fast path: already known (single-shard read, no accounting).
	if raw, err := r.getRaw(rowKey); err != nil {
		return false, err
	} else if raw != nil {
		return true, nil
	}

	// Count fresh in-window rows from a pre-scan (outside the tx). This count is
	// NOT fenced by the transaction below: see the cap-is-approximate note on
	// the doc comment.
	items, err := r.scanPrefix(shalePrefixKeygateSubnet(subnet))
	if err != nil {
		return false, err
	}
	cutoff := now.Add(-window)
	freshCount := 0
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			// An undecodable row cannot be shown to be out of window, so it is
			// left alone rather than deleted: skipping under-counts (the gate
			// admits one too many), deleting would destroy a row that may still
			// be inside the window.
			continue
		}
		if t.After(cutoff) {
			freshCount++
			continue
		}
		r.dropExpiredKeygateRow(item.Key)
	}
	if freshCount >= limitPerSubnet {
		return false, ErrTooManyNewKeys
	}

	known := false
	txErr := r.cluster.Transact(rowKey, func(tx backend.Transaction) error {
		// Re-read inside the tx so a concurrent admit of the same PAIR resolves
		// as known rather than writing the row twice. It does not fence the
		// budget: an admit of a DIFFERENT identity touches a different key and
		// conflicts with nothing here, and no retry recomputes freshCount.
		if _, gerr := tx.Get(rowKey); gerr == nil {
			known = true
			return nil
		} else if !errors.Is(gerr, backend.ErrNotFound) {
			return gerr
		}
		return tx.Put(rowKey, []byte(now.UTC().Format(time.RFC3339Nano)))
	})
	if txErr != nil {
		return false, txErr
	}
	if !known {
		// The IDENTITY-leading view of the row just written, deliberately
		// OUTSIDE the transaction above: it hashes to a different shard than
		// the authoritative row and shale transactions are single-shard, so
		// including it aborts with ErrCrossShard. Best-effort is safe because
		// this is a derived index the reconciler repairs, and the gate reads
		// the subnet-leading row already committed above: a failure costs a
		// stale display value, never a lost admission or a weakened gate.
		idKey := shaleKeyKeygateIdentity(identity, subnet)
		if err := r.cluster.Transact(idKey, func(tx backend.Transaction) error {
			return tx.Put(idKey, []byte(now.UTC().Format(time.RFC3339Nano)))
		}); err != nil {
			r.repoLog().Printf("keygate: identity index write for %s failed: %v (whoami's subnet count may under-report until the next reconcile)", idKey, err)
		}
	}
	return known, nil
}

// SubnetSnapshot counts in-window rows for a subnet and finds the oldest
// first_seen among them. Single-shard scan on the {subnet} shard.
func (r *ShaleRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	items, err := r.scanPrefix(shalePrefixKeygateSubnet(subnet))
	if err != nil {
		return 0, time.Time{}, err
	}
	cutoff := now.Add(-window)
	count := 0
	var oldest time.Time
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if !t.After(cutoff) {
			r.dropExpiredKeygateRow(item.Key)
			continue
		}
		count++
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return count, oldest, nil
}

// SubnetsForIdentity counts distinct in-window subnets for an identity.
//
// Reads the IDENTITY-leading index, so the cost is the number of subnets THIS
// key has been seen in rather than the number of keygate rows in the whole
// cluster, which is what a filtered scan of the global keygate/ prefix costs.
//
// DISPLAY ONLY (whoami via KeyGate.Inspect, whose error is discarded): an entry
// not yet projected under-reports the count rather than weakening the Sybil
// control, which reads the transactionally-written subnet-leading rows.
func (r *ShaleRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	// SINGLE-SHARD: keygate_id/ shards on the identity (see shaleShardKey), so
	// every entry for this key lives on one unit and this is a local prefix
	// scan. Without that co-location even a narrow prefix costs a full
	// fan-out, which is the whole point of the index.
	items, err := r.scanPrefix(shalePrefixKeygateIdentity(identity))
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	seen := make(map[string]struct{})
	for _, item := range items {
		subnet := strings.TrimPrefix(string(item.Key), "keygate_id/"+identity+"/")
		if subnet == "" {
			continue
		}
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if !t.After(cutoff) {
			// The identity-leading view is derived, so dropping an expired entry
			// here needs no coordination with the authoritative row: both age out
			// of the same window.
			if err := r.cluster.Delete(item.Key); err != nil {
				r.repoLog().Printf("keygate: drop expired identity entry %s: %v (it stops counting regardless)", item.Key, err)
			}
			continue
		}
		seen[subnet] = struct{}{}
	}
	return len(seen), nil
}

// dropExpiredKeygateRow removes one out-of-window authoritative row and its
// identity-leading view. Best-effort by design: the row is already outside the
// window, so it counts toward nothing whether or not the delete lands, and a
// failure must never turn an admission into an error.
func (r *ShaleRepo) dropExpiredKeygateRow(key []byte) {
	if err := r.cluster.Delete(key); err != nil {
		r.repoLog().Printf("keygate: drop expired row %s: %v (it stops counting regardless)", key, err)
		return
	}
	rest := strings.TrimPrefix(string(key), "keygate/")
	idx := strings.LastIndex(rest, "/")
	if idx < 0 {
		return
	}
	idKey := shaleKeyKeygateIdentity(rest[idx+1:], rest[:idx])
	if err := r.cluster.Delete(idKey); err != nil {
		r.repoLog().Printf("keygate: drop expired identity entry %s: %v (it stops counting regardless)", idKey, err)
	}
}
