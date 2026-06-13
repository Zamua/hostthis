//go:build slatedb

package storage_test

// Concurrency tests for the shale reconciler running against live insert
// traffic, plus targeted coverage for the grace window and the
// leaked-marker drop (docs/SPEC.md "Running the reconciler against live
// traffic").
//
// The headline test (TestShaleReconciler_RaceAgainstInserts) is the one
// that would have caught the P1s: a reconciler looping in a tight loop
// while N goroutines concurrently insert for the SAME identity against a
// cap that admits only K. It pins two invariants the pre-fix blind-Put
// recompute violated:
//
//   - the per-owner quota ceiling is NEVER breached (the reserve CAS is a
//     hard gate; the reconciler must not be able to widen it), and
//   - after the writers stop and a final reconcile, the counter equals the
//     true authoritative live-byte sum with NO under-count (the pre-fix
//     blind recompute raced concurrent reserves and clobbered them low).
//
// All tests are //go:build slatedb and need a live MinIO; they skip
// cleanly unless MINIO_TEST_ENDPOINT is set.
//
//	go test -tags slatedb -run TestShaleReconciler_Race ./internal/storage
//	go test -tags slatedb -run TestShaleReconciler_Grace ./internal/storage
//	go test -tags slatedb -run TestShaleReconciler_LeakedAppendMarker ./internal/storage

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// isTransientWriteConflict reports whether err is a benign,
// retry-or-skip-able write conflict that heavy same-shard contention
// produces under the race harness: the cluster-level CAS-budget exhaustion
// (backend.ErrCASConflict) or a lower-level slate transaction conflict that
// surfaces verbatim. Neither leaves committed partial state (the
// transaction never applied), so neither threatens the quota invariants -
// they only mean a particular insert/delete attempt did not land. The race
// test classifies these as transient so it fails ONLY on a genuine
// invariant violation (a ceiling breach or an end-state under/over-count),
// never on contention noise.
func isTransientWriteConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, backend.ErrCASConflict) {
		return true
	}
	return strings.Contains(err.Error(), "transaction conflict") ||
		strings.Contains(err.Error(), "CAS conflict")
}

// TestShaleReconciler_RaceAgainstInserts is the headline concurrency
// test and the durable oracle for the never-recompute reconciler. Writers
// continuously insert TWO-version pastes for ONE identity against a tight
// per-owner cap, and recycle capacity by BOTH a whole-paste Delete and a
// single-version DeleteVersion (the per-version tombstone). All three
// authoritative mutations (insert/confirm, Delete, DeleteVersion) move the
// {id} counter on the same shard the reconciler churns over, so a steady
// stream of confirms AND decrements overlaps every reconcile pass - the
// exact window the removed recompute's bug lived in.
//
// Two invariants are pinned, matching the never-recompute design
// (docs/SPEC.md "Running the reconciler against live traffic"):
//
//   - INVARIANT (a) - the per-owner quota ceiling is NEVER breached. A
//     LOSSLESS high-water mark of concurrently-live BYTES (incremented by a
//     committed insert/append, decremented by a committed Delete /
//     DeleteVersion) stays <= the cap throughout the run. The reserve CAS
//     is a hard gate; the reconciler must never WIDEN the budget by writing
//     a counter value below the truly-reserved total. This is THE
//     guarantee, and it holds because the reconciler no longer overwrites
//     the counter at all. The byte-precise HWM cannot miss a transient
//     over-admission the way a periodic SCAN can (a paste admitted then
//     recycled between samples is invisible to a scan); it records the
//     breach at the instant of admission.
//   - INVARIANT (b) - after writers stop and a final reconcile, with NO
//     in-flight ops, the counter is EXACTLY the true authoritative
//     live-byte sum. No under-count (the one failure the quota must never
//     permit: it frees budget a later reserve over-admits) AND no residual
//     over-count. The documented fail-safe over-count (a Delete whose
//     authoritative {slug} removal committed but whose {id} counter
//     decrement lost a transient CAS retry, which the reconciler does NOT
//     heal online by design - docs/SPEC.md "The invariant, the residual
//     drift, and the deliberate non-goal") is detected by the test and
//     DRAINED deterministically before the assertion, since the END state
//     "with no in-flight ops" is required to be exact. Draining only ever
//     sheds bytes the authoritative truth no longer holds; it never papers
//     over an under-count.
//
// Why the old design failed here: the removed recompute derived the
// counter from a cross-shard live-byte snapshot plus surviving in-grace
// markers, then overwrote the counter. A confirm committing between the
// snapshot scan and the recompute's tx.Get(counter) was invisible to both
// (the just-confirmed paste absent from the stale snapshot, its marker
// already consumed), so the recompute wrote a value BELOW the truly-
// reserved total with no read-set conflict, freeing budget a later reserve
// consumed - a ceiling breach via under-count. The concurrent
// DeleteVersion path widens the adversary: every tombstone is another
// authoritative-truth shrink that a recompute snapshot could race. Removing
// the recompute eliminates that window entirely: the counter is now only
// ever moved by strict, read-checked CAS deltas, each individually correct
// under concurrency.
func TestShaleReconciler_RaceAgainstInserts(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler race test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	const (
		owner   = "key:race"
		v1Bytes = 100 // first version (insert) reserves 100 bytes
		v2Bytes = 60  // second version (append) reserves 60 bytes
		pasteSz = v1Bytes + v2Bytes
		admitK  = 8 // the cap admits only 8 live two-version pastes
		userCap = pasteSz * admitK
		writers = 8 // concurrent insert/recycle goroutines
		// Run long enough that the reconciler completes many passes
		// overlapping live confirms + decrements. Slate-on-MinIO commit
		// latency caps throughput, so a longer window is needed to land
		// enough committed mutations inside a reconcile pass to reliably hit
		// the race. With the recompute removed this passes cleanly at this
		// duration; if the recompute were ever reintroduced, this window
		// reliably reproduces the under-count it caused.
		churnDuration = 8 * time.Second
	)
	wantCeiling := int64(userCap)

	now := time.Now().UTC()

	// Markers are stamped with the same fixed `now` the reconciler is
	// handed, so every in-flight reservation reads age 0 and is always in
	// grace: this isolates the counter race from the grace logic (covered
	// separately). A long reserveGrace makes doubly sure no in-flight marker
	// is ever dropped, so any over-admission is attributable to a counter
	// bug, not to a prematurely-released reservation.
	const reserveGrace = time.Hour

	var stop atomic.Bool
	var reconcileErr atomic.Bool
	var reconcilePasses atomic.Int64
	var wg sync.WaitGroup

	// liveBytes is a LOSSLESS witness of the owner's concurrently-live bytes,
	// maintained by the test itself: +N on a committed insert/append, -N on
	// a committed Delete / DeleteVersion. liveBytesHWM is its running
	// maximum: the most bytes that ever coexisted for this owner. If that
	// HWM ever exceeds the cap, the per-owner ceiling admitted more bytes
	// than it should have - a hard-ceiling breach. Tracking BYTES (not just
	// paste count) is required now that pastes carry two differently-sized
	// versions and DeleteVersion frees only one of them.
	var liveBytes atomic.Int64
	var liveBytesHWM atomic.Int64
	bumpLive := func(delta int64) {
		v := liveBytes.Add(delta)
		for {
			h := liveBytesHWM.Load()
			if v <= h || liveBytesHWM.CompareAndSwap(h, v) {
				break
			}
		}
	}

	// authoritativeLiveBytes sums the owner's live (non-deleted) version
	// bytes straight from the authoritative pastes/versions, independent of
	// the derived counter. Used only for the at-rest end-of-run checks.
	authoritativeLiveBytes := func() int64 {
		// Inserts confirm their identity_pastes index entry in a deferred
		// goroutine; drain so ListByOwner sees every just-inserted paste and
		// atRest does not under-report.
		repo.WaitPendingConfirms()
		list, err := repo.ListByOwner(owner)
		if err != nil {
			return -1
		}
		var total int64
		for _, p := range list {
			vers, verr := repo.ListVersions(p.Slug)
			if verr != nil {
				return -1
			}
			for _, v := range vers {
				if !v.Deleted {
					total += int64(v.Size)
				}
			}
		}
		return total
	}

	// Reconciler loop: the live-traffic adversary.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if err := repo.ReconcileForTest(now, reserveGrace); err != nil {
				reconcileErr.Store(true)
				return
			}
			reconcilePasses.Add(1)
		}
	}()

	// pasteState tracks, for a landed paste, how many bytes the test still
	// believes are live for it: pasteSz when both versions are live, v1Bytes
	// after its v2 has been DeleteVersion'd. This lets a whole-paste Delete
	// decrement the liveBytes witness by exactly what remained.
	type pasteState struct {
		slug      domain.Slug
		liveBytes int64 // pasteSz, or v1Bytes after a v2 tombstone
	}

	// residualBytes accumulates the documented fail-safe over-count: a
	// Delete / DeleteVersion whose authoritative {slug} mutation committed
	// but whose {id} counter decrement lost a transient CAS retry. The
	// reconciler does NOT heal this online (by design). The test drains it
	// after the run so the no-in-flight-ops END state can be asserted EXACT.
	var residualBytes atomic.Int64

	deadline := time.Now().Add(churnDuration)
	var admitted atomic.Int64
	var appended atomic.Int64
	var deleted atomic.Int64
	var versionDeleted atomic.Int64
	var overQuota atomic.Int64
	var casExhausted atomic.Int64
	var attempts atomic.Int64
	var writeErr atomic.Value
	var landedMu sync.Mutex
	landed := make([]*pasteState, 0, 256)

	pushLanded := func(st *pasteState) {
		landedMu.Lock()
		landed = append(landed, st)
		landedMu.Unlock()
	}
	popVictim := func() *pasteState {
		landedMu.Lock()
		defer landedMu.Unlock()
		if len(landed) <= admitK/2 {
			return nil
		}
		v := landed[0]
		landed = landed[1:]
		return v
	}

	// pasteGone reports whether the authoritative {slug} paste row is gone.
	// After a transient on a whole-paste Delete, this tells the test whether
	// the {slug} removal committed (so the lost {id} decrement is a residual)
	// or not (so the paste is intact and should be requeued).
	pasteGone := func(slug domain.Slug) bool {
		_, err := repo.Get(slug)
		return errors.Is(err, storage.ErrNotFound)
	}
	// versionTombstoned reports whether version `ver` of slug is tombstoned
	// in authoritative truth. After a transient on DeleteVersion, this tells
	// the test whether the {slug} tombstone committed (lost {id} decrement =
	// residual) or not (requeue, the version is still live).
	versionTombstoned := func(slug domain.Slug, ver int) bool {
		vers, err := repo.ListVersions(slug)
		if err != nil {
			return false
		}
		for _, v := range vers {
			if v.VerNum == ver {
				return v.Deleted
			}
		}
		// The version row is gone entirely (whole paste deleted under us):
		// treat as tombstoned - its bytes are not in authoritative truth.
		return true
	}

	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for time.Now().Before(deadline) {
				n := attempts.Add(1)

				// --- recycle step (runs FIRST so capacity keeps cycling) -----
				// Decoupling the delete from a successful insert is what keeps
				// the cap genuinely filling AND freeing: gating deletes behind
				// inserts stalls the churn the moment the cap saturates (every
				// insert then over-quotas and nothing is ever freed). Half the
				// recycles tombstone just the v2 (DeleteVersion); the other half
				// delete the whole paste (Delete). This interleaves BOTH
				// authoritative-shrink paths against the reconciler.
				if victim := popVictim(); victim != nil {
					if victim.liveBytes > v1Bytes && n%2 == 0 {
						// DeleteVersion the v2 (verNum 2): frees v2Bytes, v1 stays.
						derr := repo.DeleteVersion(victim.slug, 2)
						switch {
						case derr == nil:
							versionDeleted.Add(1)
							bumpLive(-v2Bytes)
							victim.liveBytes -= v2Bytes
							pushLanded(victim) // keep it for a later whole-paste Delete
						case isTransientWriteConflict(derr):
							// The {id} decrement lost the CAS retry. DeleteVersion
							// tombstones the version on {slug} FIRST. If that
							// committed, the bytes are gone from truth but still
							// counted: the documented residual. Otherwise the
							// version is intact - requeue it.
							if versionTombstoned(victim.slug, 2) {
								residualBytes.Add(v2Bytes)
								bumpLive(-v2Bytes)
								victim.liveBytes -= v2Bytes
							}
							pushLanded(victim)
							casExhausted.Add(1)
						default:
							writeErr.CompareAndSwap(nil, "deleteversion: "+derr.Error())
							return
						}
					} else {
						derr := repo.Delete(victim.slug)
						switch {
						case derr == nil:
							deleted.Add(1)
							bumpLive(-victim.liveBytes) // all remaining live bytes left
						case isTransientWriteConflict(derr):
							// Delete removes the paste on {slug} FIRST, then
							// decrements {id}. If the {slug} removal committed, the
							// lost decrement is the documented residual; re-Delete
							// cannot heal it (the row is gone). Otherwise the paste
							// is intact - requeue it.
							if pasteGone(victim.slug) {
								residualBytes.Add(victim.liveBytes)
								bumpLive(-victim.liveBytes)
							} else {
								pushLanded(victim)
							}
							casExhausted.Add(1)
						default:
							writeErr.CompareAndSwap(nil, "delete: "+derr.Error())
							return
						}
					}
				}

				// --- insert step: a fresh two-version paste ------------------
				slug := domain.Slug(fmt.Sprintf("rc%06d", n))
				p := domain.Paste{
					Slug: slug, Identity: domain.Identity(owner),
					Kind: domain.KindHTML, ContentSHA: fmt.Sprintf("sha-%d", n), Size: v1Bytes,
					CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
				}
				err := repo.InsertWithQuotaCheck(p, userCap, now)
				switch {
				case err == nil:
					admitted.Add(1)
					bumpLive(+v1Bytes) // v1 just landed; record the HWM now
					st := &pasteState{slug: slug, liveBytes: v1Bytes}
					// Append a second version. Counts toward the SAME cap, so
					// it may be over-quota'd or CAS-conflicted - both benign.
					_, aerr := repo.AppendVersionWithQuotaCheck(slug, domain.KindHTML, fmt.Sprintf("sha-%d-v2", n), v2Bytes, userCap, now)
					switch {
					case aerr == nil:
						appended.Add(1)
						bumpLive(+v2Bytes)
						st.liveBytes += v2Bytes
					case errors.Is(aerr, storage.ErrOverUserQuota):
						overQuota.Add(1)
					case isTransientWriteConflict(aerr):
						casExhausted.Add(1)
					default:
						writeErr.CompareAndSwap(nil, "append: "+aerr.Error())
						return
					}
					pushLanded(st)
				case errors.Is(err, storage.ErrOverUserQuota):
					// Cap is full: back off briefly so writers do not burn the
					// cluster on doomed reserves, starving the real churn.
					overQuota.Add(1)
					time.Sleep(time.Millisecond)
				case isTransientWriteConflict(err):
					casExhausted.Add(1)
				default:
					writeErr.CompareAndSwap(nil, "insert: "+err.Error())
					return
				}
			}
		}(w)
	}
	writersWG.Wait()

	// Let the reconciler run a couple more passes against the now-quiescent
	// state, then stop it.
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if v := writeErr.Load(); v != nil {
		t.Fatalf("a writer failed with an unexpected (non-transient) error: %v", v)
	}
	if reconcileErr.Load() {
		t.Fatalf("reconcile returned an error under concurrent insert/delete load")
	}
	if reconcilePasses.Load() == 0 {
		t.Fatalf("reconciler never completed a pass; the race window was not exercised")
	}
	if deleted.Load() == 0 && versionDeleted.Load() == 0 {
		t.Fatalf("no deletes landed; the concurrent-delete adversary was not exercised")
	}
	t.Logf("race outcome: admitted=%d appended=%d deleted=%d version-deleted=%d over-quota=%d cas-exhausted=%d attempts=%d reconcile-passes=%d live-bytes-HWM=%d residual-drained=%d (cap=%d, admitK=%d two-version pastes)",
		admitted.Load(), appended.Load(), deleted.Load(), versionDeleted.Load(), overQuota.Load(), casExhausted.Load(),
		attempts.Load(), reconcilePasses.Load(), liveBytesHWM.Load(), residualBytes.Load(), userCap, admitK)

	// INVARIANT (a): the quota ceiling was never breached. The lossless
	// byte-precise HWM records the most bytes that ever coexisted for this
	// owner; if that exceeds the cap, the reserve CAS's hard ceiling was
	// widened (the failure a reconciler recompute would have caused).
	if hwm := liveBytesHWM.Load(); hwm > wantCeiling {
		t.Fatalf("QUOTA CEILING BREACHED: %d live bytes coexisted against a %d-byte cap; the reserve CAS's hard ceiling was widened (only a reconciler recompute could do this)",
			hwm, wantCeiling)
	}

	// Final authoritative reconcile, then the at-rest invariants.
	if err := repo.ReconcileForTest(now, reserveGrace); err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	atRest := authoritativeLiveBytes()
	if atRest < 0 {
		t.Fatalf("final authoritative read failed")
	}
	// Ceiling holds at rest too.
	if atRest > wantCeiling {
		t.Fatalf("QUOTA CEILING BREACHED at rest: %d live bytes > %d cap", atRest, wantCeiling)
	}

	// Drain the documented fail-safe over-count residual: each Delete /
	// DeleteVersion that committed its authoritative {slug} mutation but lost
	// the {id} counter decrement to a transient left that many bytes counted
	// with no live data behind them. The reconciler deliberately does NOT
	// heal this online; re-calling Delete cannot either (the paste row is
	// already gone, so Delete early-returns without decrementing). We settle
	// it here through the same single-CAS {id}-shard path so the
	// no-in-flight-ops END state can be asserted EXACT. This only ever sheds
	// bytes authoritative truth no longer holds - it can never mask an
	// under-count.
	if drain := residualBytes.Load(); drain > 0 {
		if err := repo.DecrementBytesForTest(owner, drain); err != nil {
			t.Fatalf("draining the over-count residual (%d bytes): %v", drain, err)
		}
	}

	// INVARIANT (b): with no in-flight ops, the counter is EXACTLY the true
	// authoritative live bytes. A value BELOW atRest is the under-count the
	// removed blind-Put recompute caused (it frees budget a later reserve
	// over-admits) - the one failure mode the quota must never permit. A
	// value ABOVE atRest after the residual drain would mean a decrement we
	// did NOT account for went missing (a genuine bug, not the documented
	// crash-window residual), so we fail on it too: the end state must be
	// exact.
	got := mustSum(t, repo, owner, now)
	if got != int(atRest) {
		t.Fatalf("post-race counter NOT EXACT: got %d, want %d (true live authoritative bytes) after draining %d residual bytes; under-count frees budget a later reserve over-admits, over-count means an unaccounted decrement was lost",
			got, atRest, residualBytes.Load())
	}
}

// TestShaleReconciler_GraceProtectsInflightReservation pins the grace
// window (P1 #2). A reservation marker stamped `now` (within grace) for a
// paste that does not exist yet is a genuinely in-flight insert: the
// reconciler must NOT delete the marker and the counter must still
// include its bytes. Advancing the clock past grace turns the same marker
// into a stale orphan that IS released and shed from the counter.
func TestShaleReconciler_GraceProtectsInflightReservation(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler grace test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:grace"
	const reserveGrace = 10 * time.Minute

	// One real paste of 300 bytes (authoritative).
	p := domain.Paste{
		Slug: domain.Slug("gracerea"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-grace", Size: 300,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(p, 0, now); err != nil {
		t.Fatalf("insert real paste: %v", err)
	}
	// Drain the deferred confirm so the insert's OWN reservation marker is
	// consumed before we seed + reconcile: otherwise a not-yet-run confirm
	// would leave gracerea's marker behind for the past-grace reconcile to
	// release, decrementing the counter and corrupting this test's premise.
	repo.WaitPendingConfirms()

	// Seed an in-flight reservation: a marker stamped `now` (age 0, well
	// within grace) for a slug with NO authoritative paste, and fold its
	// bytes into the counter exactly as a live reserve would have (real 300
	// + in-flight 500 = 800). This models the window between the reserve
	// step and the authoritative write of a healthy insert.
	const inflightBytes = 500
	inflightSlug := "inflght1"
	marker, err := storage.EncodeReservationMarkerForTest(inflightBytes, now)
	if err != nil {
		t.Fatalf("encode in-flight marker: %v", err)
	}
	mustPutRaw(t, repo, storage.IdentityReserveKeyForTest(owner, inflightSlug), marker)
	mustPutRaw(t, repo, storage.IdentityBytesKeyForTest(owner), []byte("800"))

	// --- reconcile WHILE the marker is in grace: it must survive ----------
	if err := repo.ReconcileForTest(now, reserveGrace); err != nil {
		t.Fatalf("reconcile (in grace): %v", err)
	}
	// The marker is left alone (in-flight).
	raw, err := repo.GetRawForTest(storage.IdentityReserveKeyForTest(owner, inflightSlug))
	if err != nil {
		t.Fatalf("read in-flight marker: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("grace violated: reconcile deleted an in-flight (within-grace) reservation marker")
	}
	// The counter still includes the in-flight bytes (real 300 + 500).
	if got := mustSum(t, repo, owner, now); got != 800 {
		t.Fatalf("in-grace counter: got %d, want 800 (real 300 + in-flight 500 must stay counted)", got)
	}

	// --- advance past grace: the same marker is now a stale orphan --------
	later := now.Add(reserveGrace + time.Minute)
	if err := repo.ReconcileForTest(later, reserveGrace); err != nil {
		t.Fatalf("reconcile (past grace): %v", err)
	}
	// The stale marker is released.
	raw, err = repo.GetRawForTest(storage.IdentityReserveKeyForTest(owner, inflightSlug))
	if err != nil {
		t.Fatalf("read stale marker: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("past grace: reconcile should have released the stale reservation marker, got %q", raw)
	}
	// The counter is back to the authoritative live bytes (300); the
	// abandoned 500 is shed.
	if got := mustSum(t, repo, owner, later); got != 300 {
		t.Fatalf("past-grace counter: got %d, want 300 (stale orphan's 500 must be shed)", got)
	}
}

// TestShaleReconciler_LeakedAppendMarker pins the leaked-marker drop
// (P1 #3). An <slug>#append reservation marker older than grace whose
// paste EXISTS and whose appended version is confirmed (its bytes already
// in the authoritative live-byte sum) is a leaked confirm-failed marker.
// The reconciler must drop it (so the marker family cannot grow unbounded)
// WITHOUT changing the counter (the bytes are already authoritative).
func TestShaleReconciler_LeakedAppendMarker(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler leak test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:leak"
	const reserveGrace = 10 * time.Minute

	// A real paste (v1 = 300) plus a confirmed append (v2 = 150). Both go
	// through the live path, so the counter is authoritatively 450 and the
	// append's own reservation marker was already consumed by confirm.
	slug := domain.Slug("leakpast")
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-leak-v1", Size: 300,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(p, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	// Drain the deferred confirm so the insert's reservation marker is gone
	// before we seed the leaked append marker + reconcile.
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(slug, domain.KindHTML, "sha-leak-v2", 150, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	const wantBytes = 450 // 300 + 150, both live + authoritative
	if got := mustSum(t, repo, owner, now); got != wantBytes {
		t.Fatalf("baseline counter: got %d, want %d", got, wantBytes)
	}

	// Seed a LEAKED append marker: an <slug>#append reservation stamped
	// well in the past (older than grace) for the paste that exists and
	// whose appended bytes are already accounted. This is the residue a
	// confirm that failed after the authoritative write would leave.
	// Crucially the counter is NOT bumped (the bytes are already in it);
	// the marker is pure leak.
	leakSlug := slug.String() + "#append"
	staleAt := now.Add(-(reserveGrace + time.Hour))
	leakMarker, err := storage.EncodeReservationMarkerForTest(150, staleAt)
	if err != nil {
		t.Fatalf("encode leaked marker: %v", err)
	}
	mustPutRaw(t, repo, storage.IdentityReserveKeyForTest(owner, leakSlug), leakMarker)

	// Sanity: the leaked marker is present before reconcile.
	if raw, err := repo.GetRawForTest(storage.IdentityReserveKeyForTest(owner, leakSlug)); err != nil {
		t.Fatalf("read leaked marker pre-reconcile: %v", err)
	} else if len(raw) == 0 {
		t.Fatalf("seed failed: leaked append marker not present before reconcile")
	}

	// --- reconcile: the leaked marker is dropped, counter unchanged -------
	if err := repo.ReconcileForTest(now, reserveGrace); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The leaked marker is gone (no infinite leak).
	raw, err := repo.GetRawForTest(storage.IdentityReserveKeyForTest(owner, leakSlug))
	if err != nil {
		t.Fatalf("read leaked marker post-reconcile: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("leaked append marker not released: reconcile must drop a past-grace marker for a confirmed paste, got %q", raw)
	}
	// The counter is UNCHANGED: the appended bytes were already
	// authoritative, so dropping the marker does not move the counter.
	if got := mustSum(t, repo, owner, now); got != wantBytes {
		t.Fatalf("post-reconcile counter: got %d, want %d (dropping a leaked marker must not change an already-authoritative counter)", got, wantBytes)
	}
	// And the paste + its versions are untouched.
	if got := mustCount(t, repo, owner); got != 1 {
		t.Fatalf("post-reconcile owner paste count: got %d, want 1", got)
	}
}
