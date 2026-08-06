// ShaleRepo's reconciler: the periodic Reconcile pass, the enumeration-index
// reprojection + orphan prune it drives (shared across the paste and site
// families via reconcileEnumerationIndex), and the guarded index-entry
// write/delete primitives. Key layout and transaction model live in
// shale_repo.go; the design is docs/SPEC.md "Periodic reconcile".

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

// Reconcile is the metadata backend's maintenance pass and the sole
// quota-healing mechanism: the quota is derived by summing the cached values on
// the enumeration indexes, with no stored counter. Two jobs:
//
//   - REPROJECT the per-owner enumeration indexes from the authoritative rows
//     (add missing entries, rebuild cached quota values, drop entries whose
//     record is gone or failed), pruning against a full aggregate of the family
//     so no orphan lingers. Closes both quota gaps of the cached-sum design: a
//     crash between the {slug} row write and the {id} index write (a live row
//     the index omits, under-counting) and a stale or orphaned cached value.
//   - AGE OUT stuck pendings: a pending paste older than PendingPasteTimeout is
//     flipped to failed, dropping it from the quota scan and its loading screen.
//
// Cached values come from the pass's point-in-time snapshot, so a pass can race
// a live write's fresher refresh. Every reprojection write and prune delete is
// GUARDED on the value the pass's index snapshot read (captured strictly before
// the authoritative scans) and skips when a live write moved the entry mid-pass:
// guarded to lose, never to clobber. That is what makes the pass safe under live
// traffic, on any cadence, from every pod concurrently; a skip costs one cycle
// of staleness on one entry.
//
// Not part of the SweepRepo contract. Cross-shard via aggregate. A poisoned row
// or a failed per-entry write is skipped and the pass continues (Policy 1); an
// undecodable row is never silently dropped from the projection.
func (r *ShaleRepo) Reconcile(now time.Time) error {
	// Snapshot the enumeration index FIRST, strictly before the authoritative
	// scans: it is the guard baseline for every reprojection write below. Values
	// computed from rows read AFTER the baseline are at least as fresh as the
	// entry, so "entry unchanged since the baseline" is a meaningful guard, and
	// any live refresh landing mid-pass flips it to skip. Baselined the other
	// way around, a refresh landing between the authoritative scan and the index
	// snapshot would BE the baseline and the stale computed value would sail
	// through the guard.
	pasteIdx, err := r.aggregateForBackground(prefixIdentityPastesAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan identity_pastes: %w", err)
	}

	pasteItems, err := r.aggregateForBackground([]byte("pastes/"))
	if err != nil {
		return fmt.Errorf("reconcile: scan pastes: %w", err)
	}

	// The reprojected entry caches the paste's LIVE byte sum (what the quota
	// scan sums), which lives in the version rows, not the head. A slug with an
	// undecodable version row gets a fail-closed placeholder below: projecting a
	// PARTIAL sum would silently under-count.
	verItems, err := r.aggregateForBackground(prefixVersionsAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan versions: %w", err)
	}
	liveBytes := make(map[string]int64)
	poisonedVersions := make(map[string]bool)
	var corruptVersions corruptTally
	for _, item := range verItems {
		rest := strings.TrimPrefix(string(item.Key), string(prefixVersionsAll))
		slug, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			// Counted, not logged per record: a poisoned row is poisoned on
			// every pass, so a line here repeats forever at a cost proportional
			// to the debris. See corrupt_tally.go.
			poisonedVersions[slug] = true
			corruptVersions.notePlaceholder(slug)
			continue
		}
		if v.Deleted {
			continue
		}
		liveBytes[slug] += int64(v.Size)
	}

	if line, ok := corruptVersions.summary("versions"); ok {
		r.repoLog().Print(line)
	}

	pastesByOwner := make(map[string]map[string]identityPasteRow)
	var corrupt corruptTally
	// A pending older than the timeout is a pod-death casualty: its in-memory
	// bytes are gone and no finalizer will ever run. Aged to failed AFTER the
	// scan + index rebuild so its index entry is not re-projected on the same
	// pass. See docs/SPEC.md "Reconciler: age out stuck pendings".
	var stalePending []domain.Slug
	for _, item := range pasteItems {
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// One poisoned paste must not stall the whole pass, which would
			// freeze index reprojection for every healthy owner; the next tick
			// retries it (Policy 1). But it CANNOT simply be dropped: the quota
			// scan reads THROUGH the enumeration index, so an undecodable row
			// whose identity_pastes entry was ALSO lost would be enumerated by
			// nothing and silently drop out of the owner's sum, a durable
			// UNDER-count that lets the owner exceed the cap permanently. Derive
			// the owner decode-independently from slug_owner/<slug> and project a
			// PLACEHOLDER entry so the next quota scan enumerates the slug,
			// re-reads the authoritative row, and hard-fails (fail-closed
			// reject). See docs/SPEC.md "Decode tolerance of the quota scan".
			owner := r.ownerOfSlug(domain.Slug(slug))
			if owner == "" {
				// No slug_owner: the owner cannot be derived and no enumeration
				// entry can be projected, so the row stays un-enumerated until
				// it is repaired. Counted, not logged per record: such a row is
				// unrepairable, so every pass re-finds the same set and per-row
				// logging burns CPU proportional to the debris, starving the
				// request path. The count is the diagnostic; the samples let a
				// reader go look at one.
				corrupt.noteUnrepairable(slug)
				continue
			}
			if pastesByOwner[owner] == nil {
				pastesByOwner[owner] = make(map[string]identityPasteRow)
			}
			// Fail-closed placeholder: name and size are unreadable and the
			// quota scan sums the CACHED size, so a zero-value entry would
			// silently under-count. The placeholder marker makes the owner's
			// next quota scan HARD-FAIL instead (docs/SPEC.md "Decode tolerance
			// of the quota scan").
			if _, ok := pastesByOwner[owner][slug]; !ok {
				pastesByOwner[owner][slug] = identityPasteRow{Placeholder: true}
			}
			corrupt.notePlaceholder(slug)
			continue
		}
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusFailed {
			// A failed paste is NOT enumerated: MarkFailed drops its entry and
			// the quota scan sums whatever the index lists, so reprojecting it
			// would resurrect its bytes and its ListByOwner presence. A leftover
			// entry is pruned by reconcileIndexes' orphan pass instead.
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
			}
		}
		// Deferred until after the index rebuild (see stalePending).
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusPending &&
			now.Sub(p.CreatedAt) > PendingPasteTimeout {
			stalePending = append(stalePending, domain.Slug(slug))
		}
	}

	if line, ok := corrupt.summary("pastes"); ok {
		r.repoLog().Print(line)
	}

	if r.testHookReconcileBeforeIndexWrites != nil {
		r.testHookReconcileBeforeIndexWrites()
	}

	// The three jobs are independent (Policy 1: one stalled job must not freeze
	// the others), so a failure in one is collected and the pass continues; the
	// joined error makes the whole pass retried next tick.
	var errs []error

	// Job 1: the paste enumeration index.
	if err := r.reconcileIndexes(pastesByOwner, pasteIdx); err != nil {
		errs = append(errs, err)
	}
	// Job 1b: age out stuck pending pastes. Each MarkFailed is independent +
	// idempotent; a failure on one slug is logged and skipped so it cannot
	// stall the rest of the pass.
	for _, slug := range stalePending {
		if err := r.MarkFailed(slug); err != nil {
			// transient-failure-log: fires only when MarkFailed errors, not per record.
			r.repoLog().Printf("reconcile: age-out pending %s: %v", slug, err)
		}
	}
	// Job 2: the SITE enumeration index, the exact parallel of Job 1. Backfills
	// sites with no index entry and closes the crash-between-row-and-index
	// under-count for sites.
	if err := r.reconcileSiteIndexPass(); err != nil {
		errs = append(errs, err)
	}
	// Job 3: the IDENTITY-leading keygate index, rebuilt from the authoritative
	// subnet-leading rows. Backfills entries that were never written and repairs
	// the drift the best-effort co-write at admit time can leave.
	if err := r.reconcileKeygateIdentityIndex(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileKeygateIdentityIndex rebuilds keygate_id/<identity>/<subnet> to
// match the authoritative keygate/<subnet>/<identity> rows.
//
// The identity-leading entry cannot be written atomically with the
// authoritative row (shale transactions are single-shard and the two keys hash
// to different shards), so it is written best-effort at admit and healed here.
//
// Deliberately BACKGROUND budget and O(all keygate rows): that full scan is the
// work kept OFF the interactive path, where it dominated a command whose answer
// is a display value. Nothing waits on this pass.
//
// Policy 1 (skip + continue) applies: one bad row must not stall the pass.
func (r *ShaleRepo) reconcileKeygateIdentityIndex() error {
	rows, err := r.aggregateForBackground([]byte("keygate/"))
	if err != nil {
		return fmt.Errorf("reconcile keygate index: scan authoritative rows: %w", err)
	}
	have, err := r.aggregateForBackground([]byte("keygate_id/"))
	if err != nil {
		return fmt.Errorf("reconcile keygate index: scan identity index: %w", err)
	}

	want := make(map[string][]byte, len(rows))
	for _, item := range rows {
		rest := strings.TrimPrefix(string(item.Key), "keygate/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		subnet, identity := rest[:idx], rest[idx+1:]
		if subnet == "" || identity == "" {
			continue
		}
		want[string(shaleKeyKeygateIdentity(identity, subnet))] = item.Value
	}

	// PRESENCE IS CHECKED THROUGH THE CONSUMER'S LENS, not the aggregate's.
	//
	// keygate_id/ shards on the identity, so SubnetsForIdentity reads it with a
	// SINGLE-SHARD prefix scan. An entry sitting on the wrong unit is still
	// visible to the cross-shard aggregate below but NOT to that read; treating
	// the aggregate's view as presence would mark it "already projected" and
	// strand it forever, invisible to the only thing that reads it. Probing per
	// identity with the same single-shard scan the consumer uses repairs
	// placement instead: the entry reads as absent, gets re-written, and lands
	// on the correct unit. One narrow scan per distinct identity on a background
	// pass, rather than one point read per row.
	visible := make(map[string]struct{}, len(want))
	identities := make(map[string]struct{})
	for key := range want {
		rest := strings.TrimPrefix(key, "keygate_id/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			identities[rest[:idx]] = struct{}{}
		}
	}
	var errs []error
	for identity := range identities {
		items, serr := r.scanPrefix(shalePrefixKeygateIdentity(identity))
		if serr != nil {
			errs = append(errs, fmt.Errorf("scan keygate index for %s: %w", identity, serr))
			continue
		}
		for _, item := range items {
			visible[string(item.Key)] = struct{}{}
		}
	}

	present := make(map[string]struct{}, len(have))
	for _, item := range have {
		key := string(item.Key)
		present[key] = struct{}{}
		if _, ok := want[key]; ok {
			continue
		}
		// ORPHAN: no authoritative row. Left alone it makes whoami over-report
		// subnets the gate has already pruned, and nothing else removes it.
		if derr := r.cluster.Delete(item.Key); derr != nil {
			errs = append(errs, fmt.Errorf("prune orphan keygate index entry %s: %w", item.Key, derr))
		}
	}
	for key, val := range want {
		if _, ok := visible[key]; ok {
			continue
		}
		k := []byte(key)
		if perr := r.cluster.Transact(k, func(tx backend.Transaction) error {
			return tx.Put(k, val)
		}); perr != nil {
			errs = append(errs, fmt.Errorf("project keygate index entry %s: %w", key, perr))
		}
	}
	return errors.Join(errs...)
}

// reconcileIndexes rebuilds identity_pastes projections to match the
// authoritative paste set per owner. have is the pass's index snapshot,
// captured by Reconcile BEFORE the authoritative scans (see the guard-baseline
// reasoning there). The mechanics live in reconcileEnumerationIndex; this
// wrapper supplies the paste family's prefix, key builder, and prune step.
func (r *ShaleRepo) reconcileIndexes(pastesByOwner map[string]map[string]identityPasteRow, have []scanItem) error {
	return reconcileEnumerationIndex(r, pastesByOwner, have, prefixIdentityPastesAll,
		shaleKeyIdentityPaste, r.pruneOrphanPasteEntry, "reconcile", "identity_pastes")
}

// pruneOrphanPasteEntry classifies one identity_pastes entry whose (owner,
// slug) is missing from the reprojection set: an orphan (the authoritative
// paste is gone), a failed paste's leftover entry, or a FRESH insert that raced
// the pass's pastes/ snapshot. It confirms against the authoritative row so the
// racing insert is kept; only the first two verdicts prune.
func (r *ShaleRepo) pruneOrphanPasteEntry(slug string) bool {
	var p pasteRow
	switch gerr := r.getJSON(shaleKeyPaste(domain.Slug(slug)), &p); {
	case errors.Is(gerr, ErrNotFound):
		// Orphan: the paste is gone.
		return true
	case gerr != nil:
		// Undecodable row: keep the entry; the fail-closed placeholder
		// projection handles it.
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
// (identity_pastes / identity_sites) to match the authoritative rows: prune
// entries with no wanted row, refresh every wanted entry (idempotent; covers
// add + update), one {id}-shard CAS per entry.
//
// have is the pass's index snapshot, captured BEFORE the authoritative scans
// (the guard-baseline ordering argument in Reconcile), serving double duty as
// the orphan-prune candidate set and as the expected-value baseline for the
// guarded writes. The prune runs against that FULL aggregate of the family, not
// just the owners with authoritative rows, so an entry lingering under an owner
// whose records are ALL gone is still dropped: the quota scan sums cached
// values without resolving the rows, so an unpruned orphan over-counts that
// owner forever.
//
// pruneOrphan is the per-family confirm-then-classify step; the drop itself is
// the VALUE-COMPARED guardedDeleteIndexEntry, because the confirm and the
// delete are not one step: a same-slug delete-then-redeploy can land a fresh
// row + entry between the confirm's NotFound and the delete, and the fresh
// entry must survive (docs/SPEC.md, the reconciler's prune). Reprojection
// writes are likewise guarded on the snapshot value, so a live write that
// landed mid-pass flips the guard and the write skips; the next pass recomputes
// from fresher rows. A failed write follows Policy 1: log, count, continue, and
// the aggregated error makes the whole pass retried next tick.
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
			if _, err := r.guardedDeleteIndexEntry(item.Key, item.Value); err != nil {
				// transient-failure-log: fires only on a failed prune, and the next pass retries.
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
				// transient-failure-log: fires only on a failed write, and the next pass retries.
				r.repoLog().Printf("%s: index write %s failed: %v (skipped; next pass retries)", logPrefix, key, err)
				continue
			}
			if !written {
				// transient-failure-log: a concurrent live write, self-resolving next pass.
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
// (<prefix><owner>/<slug>) into its owner + slug. The slug is the LAST segment:
// slugs are slash-free, but an owner identity may contain '/' (e.g. a base64
// key fingerprint), so the last '/' is the only safe split.
func splitIdentityIndexKey(key, prefix []byte) (owner, slug string, ok bool) {
	rest := string(key[len(prefix):])
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

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

// guardedDeleteIndexEntry drops the enumeration entry at key ONLY IF it still
// holds expected, the payload the pass's snapshot read. It is the delete
// sibling of guardedPutIndexEntry, closing the orphan prune's TOCTOU: the prune
// confirms the authoritative row is gone/failed and THEN deletes, and a
// same-slug delete-then-redeploy landing its fresh row + entry in that window
// must not have the FRESH entry dropped. A changed value skips, returning
// (false, nil), so the redeploy's entry survives and a stale entry lingers at
// most one more pass; an already-gone entry is an idempotent no-op. The compare
// runs inside the {id}-shard CAS with the entry in the read-set, so a write
// landing between the compare and the commit conflicts and the re-run closure
// re-compares and skips.
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
