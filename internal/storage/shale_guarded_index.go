// Guarded index-entry writes: the conditional {id}-shard CAS the repair-on-read
// paths use so a correction never clobbers a fresher concurrent write.

//go:build slatedb

package storage

import (
	"bytes"
	"errors"

	"github.com/Zamua/shale/pkg/backend"
)

// errIndexEntryChanged is the internal sentinel a guarded index write's closure
// aborts with when the entry no longer holds the value the writing computation
// started from. Transact returns a non-conflict fn error verbatim, so
// guardedPutIndexEntry maps it to "skipped". It never escapes that function.
var errIndexEntryChanged = errors.New("shale: index entry changed since the computation read it")

// guardedPutIndexEntry writes row (JSON) at key ONLY IF the entry still holds
// expected, the exact payload the writing computation read when it started
// (present=false: the entry must still be absent). This is the conditional
// primitive that keeps every recomputed cached-value write (reprojections,
// append/tombstone refreshes) from clobbering a FRESHER value landed after the
// computation's snapshot: on a mismatch it skips, returning (false, nil), and
// the next cycle recomputes from fresher rows (docs/SPEC.md "Periodic
// reconcile" / "Window C"). The compare runs inside the {id}-shard CAS with the
// entry in the read-set, so a write landing between the compare and the commit
// conflicts, and the re-run closure re-compares against the new value and skips.
func (r *ShaleRepo) guardedPutIndexEntry(key, expected []byte, present bool, row any) (bool, error) {
	if r.testHookGuardedIndexWrite != nil {
		if err := r.testHookGuardedIndexWrite(key); err != nil {
			return false, err
		}
	}
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := tx.Get(key) // records the read-check
		switch {
		case errors.Is(gerr, backend.ErrNotFound):
			if present {
				return errIndexEntryChanged // entry deleted since the snapshot
			}
		case gerr != nil:
			return gerr
		default:
			if !present {
				return errIndexEntryChanged // entry appeared since the snapshot
			}
			payload, serr := stripEnvelope(cur)
			if serr != nil {
				return serr
			}
			if !bytes.Equal(payload, expected) {
				return errIndexEntryChanged
			}
		}
		return shaleTxPutJSON(tx, key, row)
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
