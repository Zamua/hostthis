// ShaleRepo's reconciler: the periodic Reconcile maintenance pass, the
// enumeration-index reprojection + orphan prune it drives (shared across
// the paste and site families via reconcileEnumerationIndex), and the
// guarded index-entry write/delete primitives that keep every recomputed
// cached-value write from clobbering a fresher concurrent one. Split out
// of shale_repo.go (which keeps the config, lifecycle, and core CRUD);
// see that file's package comment for the key layout and transaction
// model, and docs/SPEC.md "Periodic reconcile" for the design.

//go:build slatedb

package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// --- reconciler ------------------------------------------------------------

// Reconcile is the metadata backend's maintenance pass and the SOLE
// quota-healing mechanism now that the per-identity quota is DERIVED by
// summing the cached values of the enumeration indexes (docs/SPEC.md
// "Scan-derived quota" / "Derived indexes and repair-on-read"). With no
// stored counter to keep correct, it has exactly two jobs:
//
//   - REPROJECT the per-owner enumeration indexes from the authoritative
//     rows: identity_pastes/<id>/<slug> from the pastes/ + versions/ scans
//     (reconcileIndexes) and identity_sites/<id>/<slug> from the sites/ scan
//     (reconcileSiteIndexPass). Each adds a missing entry, rebuilds every
//     projection's CACHED QUOTA VALUES (a paste entry's size is the live
//     version sum from the versions/ scan; a site entry's is the row's
//     deduped size), and drops an entry whose paste/site is gone or failed -
//     pruning against a full aggregate of the index family so an orphan
//     lingers nowhere, not even under an owner with no remaining rows. This
//     closes BOTH quota-relevant gaps the cached-sum design has: a crash
//     between the authoritative {slug} row write and the {id} index write
//     (a live row the index does not list, transiently UNDER-counting the
//     owner) and a stale/orphaned cached value (a lost size refresh or a
//     lost entry drop, transiently mis-counting by that record's bytes).
//   - AGE OUT stuck pending pastes (the pod-death backstop): a status=pending
//     paste older than PendingPasteTimeout is flipped to failed so it drops
//     out of the quota scan (MarkFailed drops its enumeration entry) and its
//     loading screen.
//
// It reprojects the authoritative rows: a SET (add/drop entries, an
// idempotent heal) plus per-record cached VALUES (each paste's live version
// sum), and the values are numbers computed from the pass's point-in-time
// snapshot, so a pass can race a live write's fresher refresh. Every
// reprojection write (and prune delete) is therefore GUARDED on the value
// the pass's index snapshot read - captured strictly BEFORE the
// authoritative scans, see the ordering note in the body - skipping when a
// live write moved the entry mid-pass (guarded to lose, never to clobber).
// That is what makes the pass safe under live traffic on any cadence and
// from every pod concurrently: each converges toward the authoritative
// state, and a skip costs at most one cycle of staleness on one entry. It
// is NOT part of the SweepRepo contract: the sweep's public surface is
// unchanged. Single-node, cross-shard via aggregate; a poisoned row - or a
// failed per-entry index write - is skipped + logged and the pass continues
// (docs/SPEC.md "Decode tolerance is per-scan-semantics", Policy 1) - but
// an undecodable row is never silently dropped from the projection (see the
// fail-closed placeholder below).
func (r *ShaleRepo) Reconcile(now time.Time) error {
	// Snapshot the enumeration index FIRST - strictly before the
	// authoritative scans - because it is the guard baseline for every
	// reprojection write below: a write commits only if its entry still
	// holds this snapshot's value. Snapshotting the index first is what
	// makes a guard pass MEAN something: the value this pass computes comes
	// from authoritative rows read AFTER the baseline, so "entry unchanged
	// since the baseline" proves the computed value is at least as fresh as
	// the entry - while any live refresh that lands mid-pass changes the
	// entry and flips the guard to skip. (Baselined the other way around, a
	// refresh landing between the authoritative scan and a later index
	// snapshot would be adopted as the baseline and the stale computed value
	// would sail through the guard - the exact clobber the guard exists to
	// stop.)
	pasteIdx, err := r.aggregatePrefix(prefixIdentityPastesAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan identity_pastes: %w", err)
	}

	// Gather authoritative paste state across all shards.
	pasteItems, err := r.aggregatePrefix([]byte("pastes/"))
	if err != nil {
		return fmt.Errorf("reconcile: scan pastes: %w", err)
	}

	// Gather the version rows too: the reprojected entry caches the paste's
	// LIVE byte sum (what the quota scan sums), which lives in the version
	// rows, not the head. A slug with an undecodable version row cannot have
	// its live sum computed - projecting a PARTIAL sum would silently
	// under-count - so it is marked and projected as a fail-closed
	// placeholder below (Policy 1 for the pass, Policy 3 for the value).
	verItems, err := r.aggregatePrefix(prefixVersionsAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan versions: %w", err)
	}
	liveBytes := make(map[string]int64)
	poisonedVersions := make(map[string]bool)
	for _, item := range verItems {
		rest := strings.TrimPrefix(string(item.Key), string(prefixVersionsAll))
		slug, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			poisonedVersions[slug] = true
			r.repoLog().Printf("reconcile: undecodable version %s: projecting a fail-closed placeholder for %s: %v", item.Key, slug, err)
			continue
		}
		if v.Deleted {
			continue
		}
		liveBytes[slug] += int64(v.Size)
	}

	pastesByOwner := make(map[string]map[string]identityPasteRow)
	// stalePending collects slugs whose status is pending and whose
	// created_at is older than the pending timeout: the pod-death backstop
	// (the in-memory bytes are gone, no finalizer will ever run). They are
	// aged to failed AFTER the scan + index rebuild so a too-old pending's
	// index entry does not get re-projected by reconcileIndexes on the same
	// pass. See docs/SPEC.md "Reconciler: age out stuck pendings".
	var stalePending []domain.Slug
	for _, item := range pasteItems {
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// Idempotent reconcile: one poisoned paste must not stall the whole
			// pass (which would freeze index reprojection for every healthy
			// owner); the next tick retries it (Policy 1). But we CANNOT simply
			// drop it: the quota scan reads THROUGH the enumeration index, so an
			// undecodable row whose identity_pastes entry was ALSO lost (a crash
			// between the row write and the index write) would be enumerated by
			// nothing and silently drop from the owner's sum - a durable
			// UNDER-count that lets the owner exceed the cap permanently. So
			// derive the owner decode-independently from slug_owner/<slug> and
			// still project a PLACEHOLDER enumeration entry (zero-value row) so
			// the next quota scan enumerates the slug and re-reads the
			// authoritative row - which then hard-fails the scan (fail-closed
			// reject), never a silent under-count. See docs/SPEC.md "Decode
			// tolerance of the quota scan".
			owner := r.ownerOfSlug(domain.Slug(slug))
			if owner == "" {
				// No slug_owner to derive the owner (only reachable on the
				// metadata-only test path; prod always writes slug_owner). Cannot
				// project the entry; log loudly - this is the one residual where
				// the row can stay un-enumerated until it is repaired.
				r.repoLog().Printf("reconcile: undecodable paste %s AND no slug_owner: cannot project enumeration entry; quota may under-count this slug until it is repaired: %v", item.Key, err)
				continue
			}
			if pastesByOwner[owner] == nil {
				pastesByOwner[owner] = make(map[string]identityPasteRow)
			}
			// Fail-closed placeholder: we cannot read name/size/expiry from the
			// corrupt row, and the quota scan sums the CACHED size, so a
			// zero-value entry would silently under-count. The placeholder
			// marker makes the owner's next quota scan HARD-FAIL instead
			// (docs/SPEC.md "Decode tolerance of the quota scan").
			if _, ok := pastesByOwner[owner][slug]; !ok {
				pastesByOwner[owner][slug] = identityPasteRow{Placeholder: true}
			}
			r.repoLog().Printf("reconcile: undecodable paste %s: projected fail-closed placeholder enumeration entry under owner %s: %v", item.Key, owner, err)
			continue
		}
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusFailed {
			// A failed paste is NOT enumerated: MarkFailed drops its entry, and
			// the quota scan sums whatever the index lists, so reprojecting it
			// would resurrect its bytes (and its ListByOwner presence). A
			// leftover entry (crash mid-MarkFailed) is pruned by
			// reconcileIndexes' orphan pass instead.
			continue
		}
		if pastesByOwner[p.Identity] == nil {
			pastesByOwner[p.Identity] = make(map[string]identityPasteRow)
		}
		if poisonedVersions[slug] {
			// The live sum is uncomputable (an undecodable version row): project
			// the fail-closed placeholder rather than a partial number.
			pastesByOwner[p.Identity][slug] = identityPasteRow{Placeholder: true}
		} else {
			pastesByOwner[p.Identity][slug] = identityPasteRow{
				Name:      p.Name,
				Size:      int(liveBytes[slug]),
				CreatedAt: p.CreatedAt,
				ExpiresAt: p.ExpiresAt,
			}
		}
		// Age-out check: a pending paste older than the timeout is a
		// pod-death casualty (its in-memory bytes never reached the blob
		// store). Defer the transition until after the index rebuild.
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusPending &&
			now.Sub(p.CreatedAt) > PendingPasteTimeout {
			stalePending = append(stalePending, domain.Slug(slug))
		}
	}

	if r.testHookReconcileBeforeIndexWrites != nil {
		r.testHookReconcileBeforeIndexWrites()
	}

	// The three jobs are independent (Policy 1: one stalled job must not
	// freeze the others), so a failure in one is collected and the pass
	// continues; the joined error makes the whole pass retried next tick.
	var errs []error

	// Job 1: reproject the paste enumeration index from the authoritative rows.
	if err := r.reconcileIndexes(pastesByOwner, pasteIdx); err != nil {
		errs = append(errs, err)
	}
	// Job 1b: age out stuck pending pastes (pod-death backstop). Each
	// MarkFailed is independent + idempotent; a failure on one slug is
	// logged and skipped so it cannot stall the rest of the pass (the next
	// tick retries it), the same per-row discipline the index reprojection uses.
	for _, slug := range stalePending {
		if err := r.MarkFailed(slug); err != nil {
			r.repoLog().Printf("reconcile: age-out pending %s: %v", slug, err)
		}
	}
	// Job 2: reproject the SITE enumeration index from the authoritative sites/
	// rows (the exact parallel of Job 1, re-homed here from the now-deleted
	// site-reservation pass so the site index still heals every tick). This is
	// what backfills sites deployed before the identity_sites index existed and
	// closes the crash-between-row-and-index under-count for sites.
	if err := r.reconcileSiteIndexPass(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileIndexes rebuilds identity_pastes projections to match the
// authoritative paste set per owner: adds missing entries, refreshes every
// projection's cached values, and drops entries with no live authoritative
// paste. have is the pass's index snapshot, captured by Reconcile BEFORE
// the authoritative scans (see the guard-baseline reasoning there). The
// mechanics - orphan prune, guarded reprojection, Policy 1 error handling -
// live in reconcileEnumerationIndex; this wrapper supplies the paste
// family's prefix, key builder, and confirm-then-classify prune step.
func (r *ShaleRepo) reconcileIndexes(pastesByOwner map[string]map[string]identityPasteRow, have []scanItem) error {
	return reconcileEnumerationIndex(r, pastesByOwner, have, prefixIdentityPastesAll,
		shaleKeyIdentityPaste, r.pruneOrphanPasteEntry, "reconcile", "identity_pastes")
}

// pruneOrphanPasteEntry classifies one identity_pastes entry whose (owner,
// slug) is missing from the reprojection set: an orphan (the authoritative
// paste is gone), a failed paste's leftover entry (crash mid-MarkFailed), or
// a FRESH insert that raced the pass's pastes/ snapshot. It confirms against
// the authoritative row so the racing insert is kept; only the first two
// verdicts prune.
func (r *ShaleRepo) pruneOrphanPasteEntry(slug string) bool {
	var p pasteRow
	switch gerr := r.getJSON(shaleKeyPaste(domain.Slug(slug)), &p); {
	case errors.Is(gerr, ErrNotFound):
		// Orphan: the paste is gone.
		return true
	case gerr != nil:
		// Undecodable row: keep the entry; the fail-closed placeholder
		// projection handles it (or already did via slug_owner).
		return false
	case domain.NormalizeStatus(p.Status) == domain.PasteStatusFailed:
		// Failed pastes are not enumerated. Same guarded drop.
		return true
	default:
		// Live row that raced the snapshot: keep; the next pass reprojects it.
		return false
	}
}

// reconcileEnumerationIndex rebuilds one per-owner enumeration-index family
// (identity_pastes / identity_sites) to match the authoritative rows: it
// prunes entries with no wanted row and refreshes every wanted entry
// (idempotent; covers add + update), one {id}-shard CAS per entry. have is
// the pass's index snapshot, captured BEFORE the authoritative scans (the
// guard-baseline ordering argument in Reconcile); it serves double duty as
// the orphan-prune candidate set and as the expected-value baseline for the
// guarded writes. The orphan prune runs against that FULL aggregate of the
// index family - not just the owners with authoritative rows - so an entry
// lingering under an owner whose records are ALL gone is still dropped (the
// quota scan sums cached values without resolving the rows, so an unpruned
// orphan would over-count that owner forever). pruneOrphan is the per-family
// confirm-then-classify step; the drop itself is the VALUE-COMPARED
// guardedDeleteIndexEntry, because the confirm and the delete are not one
// step: a same-slug delete-then-redeploy can land a fresh row + entry
// between the confirm's NotFound and the delete, and the fresh entry must
// survive (docs/SPEC.md, the reconciler's prune). Reprojection writes are
// guarded on the pass's snapshot value: a live write that landed mid-pass
// (its cached values are fresher than this pass's point-in-time computation)
// flips the guard and the write skips - the next pass recomputes from
// fresher rows. A failed write follows Policy 1 (one entry's failure must
// not stall the reprojection for every healthy owner): log, count, continue;
// the aggregated error makes the whole pass retried next tick. logPrefix +
// familyName keep each family's log/error strings byte-identical to the
// pre-extraction per-family loops.
func reconcileEnumerationIndex[R any](r *ShaleRepo, wanted map[string]map[string]R, have []scanItem, prefix []byte, entryKey func(owner, slug string) []byte, pruneOrphan func(slug string) bool, logPrefix, familyName string) error {
	snapshot := make(map[string][]byte, len(have))
	for _, item := range have {
		snapshot[string(item.Key)] = item.Value
	}
	for _, item := range have {
		owner, slug, ok := splitIdentityIndexKey(item.Key, prefix)
		if !ok {
			continue
		}
		if _, w := wanted[owner][slug]; w {
			continue
		}
		if pruneOrphan(slug) {
			// Best-effort, guarded drop.
			if _, err := r.guardedDeleteIndexEntry(item.Key, item.Value); err != nil {
				r.repoLog().Printf("%s: prune %s: %v (next pass retries)", logPrefix, item.Key, err)
			}
		}
	}
	var writeFailures int
	for owner, want := range wanted {
		for slug, row := range want {
			key := entryKey(owner, slug)
			expected, present := snapshot[string(key)]
			written, err := r.guardedPutIndexEntry(key, expected, present, row)
			if err != nil {
				writeFailures++
				r.repoLog().Printf("%s: index write %s failed: %v (skipped; next pass retries)", logPrefix, key, err)
				continue
			}
			if !written {
				r.repoLog().Printf("%s: index write %s skipped: entry changed since the pass snapshot (a live write landed; next pass reprojects)", logPrefix, key)
			}
		}
	}
	if writeFailures > 0 {
		return fmt.Errorf("%s: %d %s index write(s) failed (skipped; next pass retries them)", logPrefix, writeFailures, familyName)
	}
	return nil
}

// splitIdentityIndexKey splits an enumeration-index key
// (<prefix><owner>/<slug>) into its owner + slug. The slug is the LAST
// segment (slugs are slash-free; an owner identity may itself contain '/',
// e.g. a base64 key fingerprint, so the last '/' is the only safe split).
func splitIdentityIndexKey(key, prefix []byte) (owner, slug string, ok bool) {
	rest := string(key[len(prefix):])
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// errIndexEntryChanged is the internal sentinel a guarded index write's
// closure aborts with when the entry no longer holds the value the writing
// computation started from (a fresher write landed in between). Transact
// returns a non-conflict fn error verbatim, so guardedPutIndexEntry maps it
// to "skipped". It never escapes guardedPutIndexEntry.
var errIndexEntryChanged = errors.New("shale: index entry changed since the computation read it")

// guardedPutIndexEntry writes row (JSON) at key ONLY IF the entry still
// holds expected - the exact payload the writing computation read when it
// started (present=false: the entry must still be absent). This is the
// conditional primitive that keeps every recomputed cached-value write (the
// reconciler's reprojections, the append/tombstone refreshes) from
// clobbering a FRESHER value a concurrent write landed after the
// computation's snapshot: on a mismatch it skips, returning (false, nil) -
// guarded to lose, never to clobber; the next cycle/refresh recomputes from
// fresher rows (docs/SPEC.md "Periodic reconcile" / "Window C"). The
// compare runs inside the {id}-shard CAS with the entry in the read-set, so
// a write landing between the compare and the commit conflicts, and the
// re-run closure re-compares against the new value and skips.
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

// guardedDeleteIndexEntry drops the enumeration entry at key ONLY IF it
// still holds expected - the payload the pass's snapshot read. It is the
// delete sibling of guardedPutIndexEntry, closing the orphan prune's
// TOCTOU: the prune confirms the authoritative row is gone/failed and THEN
// deletes, and a same-slug delete-then-redeploy landing its fresh row +
// entry in that window must not have the FRESH entry dropped. A changed
// value skips, returning (false, nil) - the redeploy's entry survives, and
// the stale entry (if the skip kept one) lingers at most one more pass; an
// already-gone entry is an idempotent no-op (docs/SPEC.md, the
// reconciler's prune). The compare runs inside the {id}-shard CAS with the
// entry in the read-set, so a write landing between the compare and the
// commit conflicts and the re-run closure re-compares and skips.
func (r *ShaleRepo) guardedDeleteIndexEntry(key, expected []byte) (bool, error) {
	if r.testHookBeforeOrphanPruneDelete != nil {
		r.testHookBeforeOrphanPruneDelete(key)
	}
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := tx.Get(key) // records the read-check
		switch {
		case errors.Is(gerr, backend.ErrNotFound):
			return nil // already gone: idempotent no-op
		case gerr != nil:
			return gerr
		}
		payload, serr := stripEnvelope(cur)
		if serr != nil {
			return serr
		}
		if !bytes.Equal(payload, expected) {
			return errIndexEntryChanged
		}
		return tx.Delete(key)
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
