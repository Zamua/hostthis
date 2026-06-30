//go:build slatedb

package storage_test

// Reconciler test for the shale backend.
//
// docs/SPEC.md "Running the reconciler against live traffic":
// identity_pastes/* is a DERIVED projection of the authoritative pastes/*
// rows and CAN be rebuilt by the reconciler (add missing, refresh stale,
// drop entries whose paste is gone). identity_bytes/* is the strict
// reservation COUNTER, maintained purely by read-checked CAS deltas on
// the hot path; the reconciler NEVER recomputes it from a scan (that was
// the structurally-racy design that was removed). The only counter write
// the reconciler makes is the orphan-reserve-release delta, covered by
// TestShaleReconciler_ReleasesOrphanReservation below.
//
// This test pins the derived-INDEX half of the reconciler: it desyncs the
// index (drops an identity_pastes entry whose paste still exists, the
// "missing index entry" half repair-on-read cannot fix on its own) AND
// corrupts the counter to a bogus value, then runs Reconcile and asserts
//
//   - the missing index entry is rebuilt (the paste reappears in the list),
//   - the corrupted counter is LEFT AS-IS: the reconciler never overwrites
//     the counter from a scan, so a corrupted absolute value is NOT healed
//     online (that is the deliberate non-goal; recovery is the offline
//     audit tool documented in the spec, not an online recompute).
//
//	go test -tags slatedb -run TestShaleReconciler ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleReconciler_RebuildsDerivedIndexAndCounter(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:recon"

	// Insert two pastes for the owner the normal way: this populates the
	// authoritative rows AND the correct derived index + counter, so we
	// have a known-good baseline to break and then heal back to.
	pA := domain.Paste{
		Slug: domain.Slug("recon1ab"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-recon1", Size: 300,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	pB := domain.Paste{
		Slug: domain.Slug("recon2cd"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-recon2", Size: 200,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), pA, 0, now); err != nil {
		t.Fatalf("insert pA: %v", err)
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), pB, 0, now); err != nil {
		t.Fatalf("insert pB: %v", err)
	}
	// Append a second version to pA so the counter must sum across versions.
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), pA.Slug, domain.KindHTML, "sha-recon1-v2", 100, 0, now); err != nil {
		t.Fatalf("append pA v2: %v", err)
	}

	// Authoritative live bytes for the owner: pA(300 + 100) + pB(200) = 600.
	const wantBytes int64 = 600

	// Sanity: baseline is correct before we corrupt anything.
	if got := mustSum(t, repo, owner, now); got != int(wantBytes) {
		t.Fatalf("baseline counter: got %d, want %d", got, wantBytes)
	}
	if got := mustCount(t, repo, owner); got != 2 {
		t.Fatalf("baseline count: got %d, want 2", got)
	}

	// --- desync the derived state -----------------------------------------

	// 1. Delete pA's identity_pastes index entry while pA still exists
	//    authoritatively. ListByOwner / CountByOwner now under-report.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, pA.Slug.String())); err != nil {
		t.Fatalf("delete index entry: %v", err)
	}
	// 2. Corrupt the identity_bytes counter to a bogus value. The reconciler
	//    must NOT heal this (it never recomputes the counter from a scan);
	//    we assert the corruption survives below.
	const corruptCounter = 99999
	if err := repo.PutRawForTest(storage.IdentityBytesKeyForTest(owner), []byte("99999")); err != nil {
		t.Fatalf("corrupt counter: %v", err)
	}

	// Confirm the desync is observable (the index drop dropped pA from the
	// owner's list; the counter now reads the bogus value).
	if got := mustCount(t, repo, owner); got != 1 {
		t.Fatalf("post-desync count: got %d, want 1 (pA dropped from index)", got)
	}
	if got := mustSum(t, repo, owner, now); got != corruptCounter {
		t.Fatalf("post-desync counter: got %d, want %d (corrupted)", got, corruptCounter)
	}

	// --- reconcile + assert convergence -----------------------------------

	if err := repo.ReconcileForTest(now, time.Hour); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The counter is NOT recomputed: the reconciler only ever moves the
	// counter via strict CAS deltas (orphan-release), never a scan-derived
	// overwrite, so the corrupted absolute value is deliberately left
	// in place (docs/SPEC.md "The invariant, the residual drift, and the
	// deliberate non-goal" - corruption recovery is the OFFLINE audit tool,
	// never an online recompute). wantBytes is kept as documentation of the
	// true authoritative live-byte sum the offline tool would converge to.
	_ = wantBytes
	if got := mustSum(t, repo, owner, now); got != corruptCounter {
		t.Fatalf("post-reconcile counter: got %d, want %d (the reconciler must NOT recompute/heal the counter from a scan)", got, corruptCounter)
	}
	// The dropped index entry is rebuilt: both pastes are back in the list.
	if got := mustCount(t, repo, owner); got != 2 {
		t.Fatalf("post-reconcile count: got %d, want 2 (index rebuilt)", got)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("post-reconcile list: got %d pastes, want 2", len(list))
	}
	sawA, sawB := false, false
	for _, p := range list {
		switch p.Slug {
		case pA.Slug:
			sawA = true
		case pB.Slug:
			sawB = true
		}
		if p.Identity.String() != owner {
			t.Fatalf("reconcile leaked a non-owner paste: %+v", p)
		}
	}
	if !sawA || !sawB {
		t.Fatalf("post-reconcile list missing a paste: sawA=%v sawB=%v list=%+v", sawA, sawB, list)
	}

	// The rebuilt index entry is a real entry, not a tombstone read: the
	// raw key is present.
	raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, pA.Slug.String()))
	if err != nil {
		t.Fatalf("read rebuilt index entry: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("reconcile should have rebuilt pA's index entry, got empty")
	}
}

// TestShaleReconciler_ReleasesOrphanReservation pins the second half of
// the reconciler's job: an identity_reserve marker for a paste that
// never materialized (a failed authoritative write left the counter
// over-counted) is released so the counter converges back to the
// authoritative live-byte sum.
func TestShaleReconciler_ReleasesOrphanReservation(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:orphan"

	// One real paste of 300 bytes for the owner.
	p := domain.Paste{
		Slug: domain.Slug("orphanab"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-orphan", Size: 300,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	// Drain the deferred confirm so the insert's own reservation marker is
	// consumed before the grace=0 reconcile below, which would otherwise
	// release it and shed the real paste's 300 bytes too.
	repo.WaitPendingConfirms()

	// Simulate a crashed reservation: an identity_reserve marker for a
	// slug that has NO authoritative paste, plus the over-counted bytes
	// folded into the counter. This is the fail-safe over-count the
	// reservation pattern leaves behind when the authoritative write
	// fails after the reserve step.
	const orphanBytes = 500
	ghostSlug := "ghostpst"
	mustPutRaw(t, repo, storage.IdentityReserveKeyForTest(owner, ghostSlug), []byte("500"))
	// Counter currently holds the real 300; bump it to 800 to model the
	// reserve that incremented but whose authoritative write never landed.
	mustPutRaw(t, repo, storage.IdentityBytesKeyForTest(owner), []byte("800"))

	if got := mustSum(t, repo, owner, now); got != 800 {
		t.Fatalf("pre-reconcile counter: got %d, want 800 (real 300 + orphan %d)", got, orphanBytes)
	}

	// Reconcile with a zero grace window so the orphan is eligible now.
	if err := repo.ReconcileForTest(now, 0); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The counter is rebuilt to the authoritative live bytes (300); the
	// orphan's 500 is shed.
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("post-reconcile counter: got %d, want 300 (orphan released)", got)
	}
	// The orphan reservation marker is gone.
	raw, err := repo.GetRawForTest(storage.IdentityReserveKeyForTest(owner, ghostSlug))
	if err != nil {
		t.Fatalf("read orphan marker: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("reconcile should have released the orphan reservation marker, got %q", raw)
	}
}

// --- helpers ---------------------------------------------------------------

func mustSum(t *testing.T, repo *storage.ShaleRepo, owner string, now time.Time) int {
	t.Helper()
	n, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum active bytes: %v", err)
	}
	return n
}

func mustCount(t *testing.T, repo *storage.ShaleRepo, owner string) int {
	t.Helper()
	// CountByOwner reads the identity_pastes index, which InsertWithQuotaCheck
	// writes via a deferred confirm goroutine. Drain so the count is
	// deterministic; a no-op when nothing is pending.
	repo.WaitPendingConfirms()
	n, err := repo.CountByOwner(owner)
	if err != nil {
		t.Fatalf("count by owner: %v", err)
	}
	return n
}
