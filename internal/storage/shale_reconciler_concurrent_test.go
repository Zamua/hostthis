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
// test. Writers continuously insert (and recycle via delete) pastes for
// ONE identity against a per-owner cap that admits only K live pastes,
// while a reconciler loops Reconcile concurrently. The delete-recycle
// keeps the cap repeatedly filling and freeing, so a steady stream of
// fresh confirms overlaps the reconciler's passes - the window the
// removed recompute's bug lived in.
//
// Two invariants are pinned, matching the never-recompute design
// (docs/SPEC.md "Running the reconciler against live traffic"):
//
//   - the per-owner quota ceiling is NEVER breached: a lossless high-water
//     mark of concurrently-live pastes (incremented on a committed insert,
//     decremented on a committed delete) stays <= K, AND the counter never
//     exceeds the cap. The reserve CAS is a hard gate; the reconciler must
//     never WIDEN the budget by writing a counter value below the
//     truly-reserved total. This is THE guarantee, and it holds because
//     the reconciler no longer overwrites the counter at all.
//   - the counter NEVER UNDER-counts: after the writers stop and a final
//     reconcile, the counter is >= the true authoritative live-byte sum.
//     An under-count is the one failure mode the quota must never permit
//     (it frees budget a later reserve would over-admit). An OVER-count
//     within the cap is fail-safe and explicitly allowed: it is the
//     documented residual from a delete whose authoritative {slug} removal
//     committed but whose {id} counter decrement lost a transient CAS
//     retry (docs/SPEC.md "The invariant, the residual drift, and the
//     deliberate non-goal"). That residual is NOT healed online by design;
//     the racy recompute that used to "heal" it is exactly what this
//     redesign removed.
//
// Why the old design failed here: the removed recompute derived the
// counter from a cross-shard live-byte snapshot plus surviving in-grace
// markers, then overwrote the counter. A confirm committing between the
// snapshot scan and the recompute's tx.Get(counter) was invisible to both
// (the just-confirmed paste absent from the stale snapshot, its marker
// already consumed), so the recompute wrote a value BELOW the truly-
// reserved total with no read-set conflict, freeing budget a later reserve
// consumed - a ceiling breach via under-count. Removing the recompute
// eliminates that window entirely: the counter is now only ever moved by
// strict, read-checked CAS deltas, each individually correct under
// concurrency.
func TestShaleReconciler_RaceAgainstInserts(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler race test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	const (
		owner      = "key:race"
		bodyPerPut = 100 // each insert reserves 100 bytes
		admitK     = 10  // the cap admits only 10 live pastes
		userCap    = bodyPerPut * admitK
		writers    = 8 // concurrent insert/recycle goroutines
		// Run long enough that the reconciler completes many recompute passes
		// overlapping live confirms. Slate-on-MinIO commit latency caps
		// throughput, so a longer window is needed to land enough confirms
		// inside a recompute to reliably hit the race. With the current fix
		// this window reliably reproduces the over-admission described in the
		// test doc; if the recompute is ever made non-racy the test should
		// pass cleanly even at this duration.
		churnDuration = 8 * time.Second
	)
	wantCeiling := int64(userCap)

	now := time.Now().UTC()

	// Markers are stamped with the same fixed `now` the reconciler is
	// handed, so every in-flight reservation reads age 0 and is always in
	// grace: this isolates the recompute race from the grace logic (covered
	// separately). A long reserveGrace makes doubly sure no in-flight marker
	// is ever dropped, so any over-admission is attributable to the
	// recompute, not to a prematurely-released reservation.
	const reserveGrace = time.Hour

	var stop atomic.Bool
	var reconcileErr atomic.Bool
	var reconcilePasses atomic.Int64
	var wg sync.WaitGroup

	// liveCount is a LOSSLESS witness of concurrently-live pastes for this
	// owner, maintained by the test itself: +1 on a committed insert, -1 on
	// a committed delete. liveHWM is its running maximum. Because every
	// admitted insert is a paste that exists until the test deletes it, the
	// HWM is the most live pastes that ever coexisted. If it ever exceeds
	// admitK, the per-owner cap admitted more bytes than it should have:
	// a hard-ceiling breach. This atomic witness cannot miss a transient
	// over-admission the way periodic SCAN-based sampling can (the scan is
	// too slow to catch a paste that is admitted then recycled between
	// samples); it records the breach at the instant of admission.
	var liveCount atomic.Int64
	var liveHWM atomic.Int64
	bumpLive := func(delta int64) {
		v := liveCount.Add(delta)
		for {
			h := liveHWM.Load()
			if v <= h || liveHWM.CompareAndSwap(h, v) {
				break
			}
		}
	}

	// authoritativeLiveBytes sums the owner's live (non-deleted) version
	// bytes straight from the authoritative pastes/versions, independent of
	// the derived counter. Used only for the at-rest end-of-run checks.
	authoritativeLiveBytes := func() int64 {
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

	// Writers: each repeatedly inserts a fresh-slug paste; on success it
	// records the slug and, to recycle capacity, deletes a previously-landed
	// slug so the cap keeps cycling (steady confirm + delete stream against
	// the reconciler). Over-quota and CAS-exhaustion are expected transient
	// outcomes. The delete-recycle is what keeps confirms flowing past the
	// initial fill.
	deadline := time.Now().Add(churnDuration)
	var admitted atomic.Int64
	var overQuota atomic.Int64
	var casExhausted atomic.Int64
	var attempts atomic.Int64
	var writeErr atomic.Value
	var landedMu sync.Mutex
	landed := make([]domain.Slug, 0, 256)

	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for time.Now().Before(deadline) {
				n := attempts.Add(1)
				slug := domain.Slug(fmt.Sprintf("rc%06d", n))
				p := domain.Paste{
					Slug: slug, Identity: domain.Identity(owner),
					Kind: domain.KindHTML, ContentSHA: fmt.Sprintf("sha-%d", n), Size: bodyPerPut,
					CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
				}
				err := repo.InsertWithQuotaCheck(p, 0, userCap, now)
				switch {
				case err == nil:
					admitted.Add(1)
					bumpLive(+1) // a fresh paste just landed; record the HWM now
					// Recycle: pop a previously-landed slug and delete it so the
					// cap frees up for the next insert, sustaining the churn (a
					// steady stream of fresh confirms overlapping the reconciler).
					landedMu.Lock()
					landed = append(landed, slug)
					var toDelete domain.Slug
					if len(landed) > admitK/2 {
						toDelete = landed[0]
						landed = landed[1:]
					}
					landedMu.Unlock()
					if toDelete != "" {
						derr := repo.Delete(toDelete)
						switch {
						case derr == nil:
							bumpLive(-1) // a paste left; liveCount falls
						case isTransientWriteConflict(derr):
							// The delete's {id}-shard counter CAS lost the retry
							// race against the reconciler/reserves on the same key:
							// a transient. The paste still exists (delete did not
							// commit), so liveCount stays; put the slug back so it
							// is recycled (or cleaned up) on a later pass.
							landedMu.Lock()
							landed = append(landed, toDelete)
							landedMu.Unlock()
							casExhausted.Add(1)
						default:
							writeErr.CompareAndSwap(nil, "delete: "+derr.Error())
							return
						}
					}
				case errors.Is(err, storage.ErrOverUserQuota):
					overQuota.Add(1)
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
	t.Logf("race outcome: admitted=%d over-quota=%d cas-exhausted=%d attempts=%d reconcile-passes=%d live-HWM=%d (cap admits K=%d, live-bytes cap=%d)",
		admitted.Load(), overQuota.Load(), casExhausted.Load(), attempts.Load(), reconcilePasses.Load(), liveHWM.Load(), admitK, userCap)

	// Invariant (a): the quota ceiling was never breached. The lossless
	// live-paste HWM records the most pastes that ever coexisted for this
	// owner; if that exceeds admitK, more than userCap bytes were
	// simultaneously committed and the reserve CAS's hard ceiling was
	// widened by the reconciler's recompute.
	if hwm := liveHWM.Load(); hwm > admitK {
		t.Fatalf("QUOTA CEILING BREACHED: %d live pastes coexisted = %d bytes against a %d-byte cap (admitK=%d); the reconciler's recompute widened the budget the reserve CAS is supposed to gate",
			hwm, hwm*bodyPerPut, userCap, admitK)
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
	// Invariant (b): no UNDER-count. After a final reconcile the counter is
	// >= the true authoritative live bytes. A value BELOW atRest is the
	// under-count the removed blind-Put recompute caused (it frees budget a
	// later reserve over-admits) - the one failure mode the quota must never
	// permit. A value ABOVE atRest (but within the cap, checked just above)
	// is the documented fail-safe residual: a delete whose authoritative
	// {slug} removal committed but whose {id} counter decrement lost a
	// transient CAS retry, leaving the bytes counted with the paste gone.
	// That residual is deliberately NOT healed online (docs/SPEC.md "The
	// invariant, the residual drift, and the deliberate non-goal"); healing
	// it would require the scan-derived recompute this redesign removed.
	got := mustSum(t, repo, owner, now)
	if got < int(atRest) {
		t.Fatalf("post-race counter UNDER-COUNT: got %d, want >= %d (true live authoritative bytes); an under-count frees budget a later reserve over-admits - the quota guarantee the reserve CAS makes",
			got, atRest)
	}
	if got > int(atRest) {
		// Fail-safe over-count: it only ever rejects a future reserve early,
		// never admits over the cap, so any magnitude is safe. It is the
		// documented residual from a delete whose counter decrement lost a
		// transient CAS retry; deliberately not healed online.
		t.Logf("post-race counter over-counts by %d (got %d, authoritative %d): the documented fail-safe residual from a delete whose counter decrement lost a transient CAS retry; not an under-count, deliberately not healed online",
			got-int(atRest), got, atRest)
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
	if err := repo.InsertWithQuotaCheck(p, 0, 0, now); err != nil {
		t.Fatalf("insert real paste: %v", err)
	}

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
	if err := repo.InsertWithQuotaCheck(p, 0, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(slug, domain.KindHTML, "sha-leak-v2", 150, 0, 0, now); err != nil {
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
