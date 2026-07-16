//go:build slatedb

package storage_test

// Legacy (migrated-slatedb) identity_pastes entries and the paste quota
// scan.
//
// docs/SPEC.md "Scan-derived quota" / "Decode tolerance of the quota
// scan": the slatedb layout stored identity_pastes/<id>/<slug> as bare
// EMPTY markers, and the migration is in-place (no key rewritten), so a
// cutover deployment's paste scans encounter empty entry values. The scan
// must recognize that legacy shape and read the entry through its
// authoritative row plus live version sum (the slatedb per-entry
// semantics) rather than hard-failing the decode - a hard fail would
// brick EVERY quota-checked create for that owner until the first
// post-cutover reconcile. The reconciler's reprojection then enriches the
// entry with the JSON projection, retiring the fallback.
//
//	go test -tags slatedb -run TestShaleQuotaScanLegacyEmptyPasteEntry ./internal/storage
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

func TestShaleQuotaScanLegacyEmptyPasteEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale legacy-entry test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	owner := "key:legidx"
	slug := domain.Slug("legidx01")

	// Plant the migrated layout raw: the authoritative paste + version rows
	// (two live versions, 300 + 200, so the fallback provably sums the
	// version rows rather than reading the head's denormalized size),
	// slug_owner, the expiry marker - and the identity_pastes entry with the
	// EMPTY value slatedb wrote.
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-legidx-v2", Size: 200,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode legacy paste: %v", err)
	}
	v1Val, err := storage.LegacyVersionValueForTest(1, p.Kind, "sha-legidx-v1", 300, p.CreatedAt, false)
	if err != nil {
		t.Fatalf("encode legacy v1: %v", err)
	}
	v2Val, err := storage.LegacyVersionValueForTest(2, p.Kind, "sha-legidx-v2", 200, p.CreatedAt, false)
	if err != nil {
		t.Fatalf("encode legacy v2: %v", err)
	}
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), v1Val)
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 2), v2Val)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))
	mustPutRaw(t, repo, storage.LegacyExpiryKeyForTest(p.ExpiresAt, slug), storage.MarkerValueForTest())

	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	if err := repo.PutEmptyBackendForTest(idxKey); err != nil {
		t.Fatalf("plant empty legacy index entry: %v", err)
	}

	// The scan must NOT hard-fail on the empty legacy shape: it reads the
	// entry through the authoritative rows (live version sum = 500).
	got, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("quota scan over a legacy empty entry must fall back to the authoritative row, not hard-fail: %v", err)
	}
	if got != 500 {
		t.Fatalf("legacy fallback sum: got %d, want 500 (live version sum)", got)
	}

	// The bricked-create symptom, pinned end-to-end: a quota-checked insert
	// for the same owner must succeed while the legacy entry is present.
	fresh := domain.Paste{
		Slug: domain.Slug("legidx02"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-legidx-new", Size: 100,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), fresh, 1<<20, now); err != nil {
		t.Fatalf("quota-checked insert with a legacy empty entry present must succeed: %v", err)
	}
	repo.WaitPendingConfirms()
	if got := mustSum(t, repo, owner, now); got != 600 {
		t.Fatalf("sum after insert: got %d, want 600", got)
	}

	// A stale legacy entry (authoritative row GONE) contributes zero
	// instead of failing or counting phantom bytes.
	staleKey := storage.IdentityPasteKeyForTest(owner, "leggone1")
	if err := repo.PutEmptyBackendForTest(staleKey); err != nil {
		t.Fatalf("plant stale legacy entry: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 600 {
		t.Fatalf("stale legacy entry must contribute zero: got %d, want 600", got)
	}

	// The reconciler enriches the legacy entry to the JSON projection (and
	// prunes the stale one), retiring the fallback: the cached size equals
	// the live version sum and the scan agrees.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
		t.Fatalf("reconcile must enrich the legacy entry to the JSON projection: got %d, want 500", got)
	}
	if raw, err := repo.GetRawForTest(staleKey); err != nil {
		t.Fatalf("read stale legacy entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("reconcile must prune the stale legacy entry; got %q", raw)
	}
	if got := mustSum(t, repo, owner, now); got != 600 {
		t.Fatalf("sum after enrichment: got %d, want 600", got)
	}
}
