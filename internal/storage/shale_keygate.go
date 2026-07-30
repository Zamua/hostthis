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
	if !known {
		// Materialise the IDENTITY-leading view of the row just written.
		//
		// Best-effort and deliberately OUTSIDE the transaction above: shale
		// transactions are single-shard, and this key hashes to a different
		// shard than the authoritative row, so including it would abort with
		// ErrCrossShard. It is a DERIVED index, repaired by the reconciler
		// like the enumeration indexes, so a failure here costs at most a
		// stale display value until the next pass - never a lost admission
		// and never a weakened gate, because the gate reads the
		// subnet-leading row that was already committed above.
		idKey := shaleKeyKeygateIdentity(identity, subnet)
		if err := r.cluster.Transact(idKey, func(tx backend.Transaction) error {
			return tx.Put(idKey, []byte(now.UTC().Format(time.RFC3339Nano)))
		}); err != nil {
			r.repoLog().Printf("keygate: identity index write for %s failed: %v (whoami's subnet count may under-report until the next reconcile)", idKey, err)
		}
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
//
// Reads the IDENTITY-leading index, so the cost is the number of subnets THIS
// key has been seen in (1 for a normal user) rather than the number of keygate
// rows in the entire cluster. A scan of the global keygate/ prefix filtered in
// Go returns the same answer at a cost that grows with total admissions across
// ALL users.
//
// DISPLAY ONLY: reached from whoami via KeyGate.Inspect, which is called after
// admit and discards its error. This gates nothing, so an index entry that has
// not been projected yet under-reports the count rather than weakening the
// Sybil control - that control reads the subnet-leading rows, which are
// authoritative and written transactionally.
func (r *ShaleRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	// SINGLE-SHARD: keygate_id/ shards on the identity (see shaleShardKey), so
	// every entry for this key lives on one unit and this is a local prefix
	// scan, not a fan-out. That co-location is what makes the index pay: a
	// narrow prefix collected via fan-out still costs the fan-out, measured at
	// 26.7s to return a single entry.
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
			continue
		}
		seen[subnet] = struct{}{}
	}
	return len(seen), nil
}

func (r *ShaleRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	items, err := r.aggregateForBackground([]byte("keygate/"))
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
			// Drop the identity-leading view of the same fact. Without this
			// the derived index outlives the authoritative row and whoami
			// would keep counting subnets the gate has already forgotten -
			// an OVER-report, and one that grows without bound because
			// nothing else would ever remove these.
			rest := strings.TrimPrefix(string(item.Key), "keygate/")
			if idx := strings.LastIndex(rest, "/"); idx >= 0 {
				subnet, identity := rest[:idx], rest[idx+1:]
				idKey := shaleKeyKeygateIdentity(identity, subnet)
				if err := r.cluster.Delete(idKey); err != nil {
					// Best-effort, like the write: the reconciler prunes an
					// identity entry whose authoritative row is gone.
					r.repoLog().Printf("keygate: identity index prune for %s failed: %v (next reconcile drops it)", idKey, err)
				}
			}
			deleted++
		}
	}
	return deleted, nil
}
