//go:build slatedb

package storage_test

// Guarded index writes: reprojection and the write paths' refreshes recompute
// cached quota values OUTSIDE the {id}-shard CAS, so a stale computation can
// race a fresher one. Every such write commits only if the entry still holds
// the value the computation started from, and SKIPS on conflict instead of
// retrying, so a race loses rather than clobbers (docs/SPEC.md "Periodic
// reconcile" / "Window C").
//
// The window is microseconds wide, so the race is driven deterministically
// through the reconciler test seam in shale_export_test.go. Skips unless
// MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestShaleReprojectionLosesToFresherRefresh pins the clobber case: a
// reprojection from a point-in-time snapshot races an append whose refresh
// already landed a FRESHER cached size. The guarded write must skip, leaving
// the fresher value in place.
func TestShaleReprojectionLosesToFresherRefresh(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reprojection-race test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	owner := "key:reprorace"
	slug := domain.Slug("reprorc1")

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-reprorace-v1", Size: 300,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	if got := readCachedIndexSize(t, repo, idxKey); got != 300 {
		t.Fatalf("cached size after insert: got %d, want 300", got)
	}

	// The hook lands the append AFTER the pass snapshotted the authoritative
	// rows (so its reprojection value is the stale 300) and BEFORE the pass's
	// index write; the append's own refresh sets the cached size to 500.
	var once sync.Once
	repo.SetReconcileBeforeIndexWritesHookForTest(func() {
		once.Do(func() {
			if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-reprorace-v2", 200, 0, now); err != nil {
				t.Errorf("racing append: %v", err)
			}
			if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
				t.Errorf("cached size after racing append: got %d, want 500", got)
			}
		})
	})
	defer repo.SetReconcileBeforeIndexWritesHookForTest(nil)

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
		t.Fatalf("reprojection clobbered the fresher refresh: cached size got %d, want 500 (guarded write must skip)", got)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after raced reconcile: got %d, want 500", got)
	}

	// An unraced pass recomputes from the authoritative rows: 500 is the
	// true live sum.
	repo.SetReconcileBeforeIndexWritesHookForTest(nil)
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (heal): %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
		t.Fatalf("cached size after healing pass: got %d, want 500", got)
	}
}
