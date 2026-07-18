// ShaleRepo's KeyGateRepo implementation (the Sybil rate limit): admit /
// snapshot / prune of the keygate/<subnet>/<identity> rows and the
// identity_first_seen family, each a single-{subnet}- or {id}-shard op.
// Split out of shale_repo.go (which keeps the config, lifecycle, and core
// CRUD); see that file's package comment for the key layout and
// transaction model.

//go:build slatedb

package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"
)

// --- KeyGateRepo (Sybil rate limit) ----------------------------------------

// AdmitNewKey atomically checks + admits a fresh (identity, subnet) pair
// on the {subnet} shard. The check (count in-window rows + the per-subnet
// cap) and the admit (write the row) are ONE CAS transaction, so two
// concurrent admissions for the same subnet serialize. The in-window count
// is computed from a pre-scan (scans are not allowed inside the tx); the
// candidate row's key is read ExpectAbsent inside the tx so the admit
// participates in conflict detection, and a racing admit of the same pair
// makes the second a known-already result.
func (r *ShaleRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, errors.New("identity + subnet required")
	}
	rowKey := shaleKeyKeygate(subnet, identity)

	// Fast path: already known? (single-shard read; no accounting.)
	if raw, err := r.getRaw(rowKey); err != nil {
		return false, err
	} else if raw != nil {
		return true, nil
	}

	// Count fresh in-window rows from a pre-scan (outside the tx).
	items, err := r.scanPrefix(shalePrefixKeygateSubnet(subnet))
	if err != nil {
		return false, err
	}
	cutoff := now.Add(-window)
	freshCount := 0
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if t.After(cutoff) {
			freshCount++
		}
	}
	if freshCount >= limitPerSubnet {
		return false, ErrTooManyNewKeys
	}

	known := false
	txErr := r.cluster.Transact(rowKey, func(tx backend.Transaction) error {
		// Re-read inside the tx: a concurrent admit of the same pair makes
		// this a known result; the ExpectAbsent read-check otherwise makes
		// a racing admit conflict + retry (re-checking the count).
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
	return known, nil
}

// SubnetSnapshot counts in-window rows for a subnet + finds the oldest
// first_seen value among them. Single-shard scan on the {subnet} shard.
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
// Cross-shard (a keygate row could live on any {subnet} shard), so it
// fans out via aggregate over the global keygate prefix.
func (r *ShaleRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	items, err := r.aggregatePrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	seen := make(map[string]struct{})
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), "keygate/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		subnet := rest[:idx]
		id := rest[idx+1:]
		if id != identity {
			continue
		}
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if !t.After(cutoff) {
			continue
		}
		seen[subnet] = struct{}{}
	}
	return len(seen), nil
}

// DeleteFirstSeenOlderThan prunes keygate rows whose first-seen is before
// cutoff, across all {subnet} shards. The candidate set is gathered via a
// cross-shard aggregate; each delete is routed to its owning shard.
func (r *ShaleRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	items, err := r.aggregatePrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := r.cluster.Delete(item.Key); err != nil {
				return deleted, fmt.Errorf("delete %s: %w", item.Key, err)
			}
			deleted++
		}
	}
	return deleted, nil
}
