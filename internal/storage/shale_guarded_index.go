// Guarded index-entry writes: the conditional {id}-shard CAS the repair-on-read
// paths use so a correction never clobbers a fresher concurrent write.

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
// the next read recomputes from fresher rows (docs/SPEC.md "Window C"). The compare runs inside the {id}-shard CAS with the
// entry in the read-set, so a write landing between the compare and the commit
// conflicts, and the re-run closure re-compares against the new value and skips.
//
// post, when non-nil, runs inside the SAME transaction after the put, so a
// companion write (the owner-doc update) commits with the entry or not at
// all. It runs only when the guard passed.
func (r *ShaleRepo) guardedPutIndexEntry(key, expected []byte, present bool, row any, post func(tx backend.Transaction) error) (bool, error) {
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
				// A truncated envelope on the CURRENT entry: it cannot be shown to
				// still hold `expected`, so treat it as a mismatch and skip, the
				// documented outcome, rather than propagating. Overwriting on a
				// value we cannot read would clobber it from a stale computation;
				// this is a repair/rollback path that must fail open.
				r.repoLog().Printf("shale: guarded index write %s: current entry envelope undecodable (%v); skipping as a mismatch", key, serr)
				return errIndexEntryChanged
			}
			if !bytes.Equal(payload, expected) {
				return errIndexEntryChanged
			}
		}
		if err := shaleTxPutJSON(tx, key, row); err != nil {
			return err
		}
		if post != nil {
			return post(tx)
		}
		return nil
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// guardedDeleteIndexEntry removes the entry at key ONLY IF it still holds
// expected, the payload the intent that owns this cleanup recorded. The guard
// exists because between a crashed insert and the sweep that cleans up after
// it, the owner may have re-uploaded the same slug, and an unguarded delete
// would eat that fresh entry. post, when non-nil, runs inside the same
// transaction after the delete, only when the guard passed.
//
// Reports whether it deleted. A mismatch (or an already-absent entry) is
// (false, nil) and commits nothing: someone else won, which is the correct
// outcome, not an error.
func (r *ShaleRepo) guardedDeleteIndexEntry(key, expected []byte, post func(tx backend.Transaction) error) (bool, error) {
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := tx.Get(key) // records the read-check
		if errors.Is(gerr, backend.ErrNotFound) {
			return errIndexEntryChanged // already gone
		}
		if gerr != nil {
			return gerr
		}
		payload, serr := stripEnvelope(cur)
		if serr != nil {
			// A truncated envelope on the CURRENT entry: it cannot be confirmed to
			// still hold `expected`, so skip rather than delete on a value we
			// cannot read. This is a rollback/cleanup path that must fail open;
			// deleting the wrong entry is far worse than leaving a damaged one for
			// the next reprojection.
			r.repoLog().Printf("shale: guarded index delete %s: current entry envelope undecodable (%v); skipping as a mismatch", key, serr)
			return errIndexEntryChanged
		}
		if !bytes.Equal(payload, expected) {
			return errIndexEntryChanged // a fresher write owns this entry now
		}
		if derr := tx.Delete(key); derr != nil {
			return derr
		}
		if post != nil {
			return post(tx)
		}
		return nil
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// guardedDropOwnerEntry is the rollback paths' guarded delete of an
// enumeration entry, with the owner doc's entry dropped in the same CAS: a
// doc-present owner never consults the legacy rows again, so a rollback that
// removed only the row would leave a doc phantom no read prunes. A guard
// mismatch touches NEITHER representation; the fresher write's own path
// maintains the doc.
func (r *ShaleRepo) guardedDropOwnerEntry(identity, slug string, expected []byte) (bool, error) {
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return false, err
	}
	return r.guardedDeleteIndexEntry(shaleKeyIdentityPaste(identity, slug), expected, func(tx backend.Transaction) error {
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			delete(doc.Pastes, slug)
		})
	})
}
