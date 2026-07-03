//go:build slatedb

package storage_test

// Lifecycle-status tests for the shale backend: MarkReady / MarkFailed
// transitions and the reconciler's age-out of a stuck pending paste.
// See docs/SPEC.md "Paste lifecycle status (async blob write)".
//
//	go test -tags slatedb -run TestShaleLifecycle ./internal/storage
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

func insertPending(t *testing.T, repo *storage.ShaleRepo, owner, slug string, size int, now time.Time) domain.Paste {
	t.Helper()
	p := domain.Paste{
		Slug:       domain.Slug(slug),
		Identity:   domain.Identity(owner),
		Status:     domain.PasteStatusPending,
		Kind:       domain.KindHTML,
		ContentSHA: "sha-" + slug,
		Size:       size,
		CreatedAt:  now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert pending %s: %v", slug, err)
	}
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get after insert %s: %v", slug, err)
	}
	if got.Status != domain.PasteStatusPending {
		t.Fatalf("inserted status: got %q, want pending", got.Status)
	}
	return p
}

func TestShaleLifecycle_MarkReady(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale lifecycle test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:life-ready"

	p := insertPending(t, repo, owner, "readyaaa", 300, now)
	if err := repo.MarkReady(p.Slug); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status: got %q, want ready", got.Status)
	}
	// Bytes still counted (a ready paste is live).
	if n := mustSum(t, repo, owner, now); n != 300 {
		t.Fatalf("bytes after ready: got %d, want 300", n)
	}
	// MarkReady on an already-ready paste is a no-op (guarded).
	if err := repo.MarkReady(p.Slug); err != nil {
		t.Fatalf("mark ready idempotent: %v", err)
	}
}

func TestShaleLifecycle_MarkFailedReleasesQuota(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale lifecycle test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:life-fail"

	p := insertPending(t, repo, owner, "failaaaa", 400, now)
	if n := mustSum(t, repo, owner, now); n != 400 {
		t.Fatalf("bytes while pending: got %d, want 400 (pending counts toward quota)", n)
	}
	if c := mustCount(t, repo, owner); c != 1 {
		t.Fatalf("count while pending: got %d, want 1", c)
	}

	if err := repo.MarkFailed(p.Slug); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusFailed {
		t.Fatalf("status: got %q, want failed", got.Status)
	}
	// Reservation released: bytes back to 0, index entry dropped.
	if n := mustSum(t, repo, owner, now); n != 0 {
		t.Fatalf("bytes after failed: got %d, want 0 (reservation released)", n)
	}
	if c := mustCount(t, repo, owner); c != 0 {
		t.Fatalf("count after failed: got %d, want 0 (index entry dropped)", c)
	}
	// MarkFailed again is a no-op: must NOT double-decrement the counter.
	if err := repo.MarkFailed(p.Slug); err != nil {
		t.Fatalf("mark failed idempotent: %v", err)
	}
	if n := mustSum(t, repo, owner, now); n != 0 {
		t.Fatalf("bytes after second failed: got %d, want 0 (no double-decrement)", n)
	}
}

func TestShaleLifecycle_ReconcilerAgesOutStuckPending(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale lifecycle test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	// Shrink the pending timeout for the test, restore after.
	orig := storage.PendingPasteTimeout
	storage.PendingPasteTimeout = time.Minute
	t.Cleanup(func() { storage.PendingPasteTimeout = orig })

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:life-stuck"

	// One pending paste that is too old (created 2 min ago) and one that is
	// fresh (created now). The reconciler should age out only the old one.
	old := insertPending(t, repo, owner, "stuckold", 250, now.Add(-2*time.Minute))
	fresh := insertPending(t, repo, owner, "freshpen", 150, now)

	// Run the reconciler at `now`: old is past the 1-min timeout, fresh is not.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	gotOld, err := repo.Get(old.Slug)
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if gotOld.Status != domain.PasteStatusFailed {
		t.Fatalf("stuck-old status: got %q, want failed (aged out)", gotOld.Status)
	}
	gotFresh, err := repo.Get(fresh.Slug)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if gotFresh.Status != domain.PasteStatusPending {
		t.Fatalf("fresh status: got %q, want pending (within timeout)", gotFresh.Status)
	}
	// Only the fresh paste still charges quota (the aged-out one released).
	if n := mustSum(t, repo, owner, now); n != 150 {
		t.Fatalf("bytes after age-out: got %d, want 150 (only fresh remains)", n)
	}
}
