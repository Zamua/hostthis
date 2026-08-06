//go:build slatedb

package storage_test

// Reconciler test for the shale backend's scan-derived quota.
//
// docs/SPEC.md "Derived indexes and repair-on-read": the per-identity quota is
// DERIVED by scanning the per-owner enumeration indexes (identity_pastes/* and
// identity_sites/*) and summing the authoritative row sizes; there is no stored
// counter. The one quota-relevant gap is a crash between the authoritative
// {slug} row write and the {id} index write, which leaves a live row the index
// does not list, so a scan UNDER-counts that owner until the reconciler
// reprojects. The reconciler only ever reprojects the indexes from the
// authoritative rows (an idempotent SET heal) and never writes an aggregate, so
// nothing can durably drift.
//
// This pins the heal for both families: drop an index entry whose authoritative
// paste/site still exists, assert the under-count, reconcile, assert the index
// is rebuilt and the scan exact again.
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

func TestShaleReconciler_RebuildsDerivedIndex(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale reconciler test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// --- paste enumeration index heal -------------------------------------

	pasteOwner := "key:recon"
	pA := domain.Paste{
		Slug: domain.Slug("recon1ab"), Identity: domain.Identity(pasteOwner),
		Kind: domain.KindHTML, ContentSHA: "sha-recon1", Size: 300,
		CreatedAt: now, UpdatedAt: now}
	pB := domain.Paste{
		Slug: domain.Slug("recon2cd"), Identity: domain.Identity(pasteOwner),
		Kind: domain.KindHTML, ContentSHA: "sha-recon2", Size: 200,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), pA, 0, now); err != nil {
		t.Fatalf("insert pA: %v", err)
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), pB, 0, now); err != nil {
		t.Fatalf("insert pB: %v", err)
	}
	// A second version on pA, so the scan must sum across versions.
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), pA.Slug, domain.KindHTML, "sha-recon1-v2", 100, 0, now); err != nil {
		t.Fatalf("append pA v2: %v", err)
	}

	// Authoritative live bytes for the owner: pA(300 + 100) + pB(200).
	const wantPasteBytes = 600

	// Baseline before the desync. mustCount drains the deferred index confirm
	// so the index is settled.
	if got := mustCount(t, repo, pasteOwner); got != 2 {
		t.Fatalf("baseline paste count: got %d, want 2", got)
	}
	if got := mustSum(t, repo, pasteOwner, now); got != wantPasteBytes {
		t.Fatalf("baseline paste sum: got %d, want %d", got, wantPasteBytes)
	}

	// The crash-between-row-and-index window: drop pA's identity_pastes entry
	// while pA still exists authoritatively. The quota scan enumerates THROUGH
	// that index, so pA's bytes and pA itself drop out.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(pasteOwner, pA.Slug.String())); err != nil {
		t.Fatalf("drop pA index entry: %v", err)
	}
	if got := mustCount(t, repo, pasteOwner); got != 1 {
		t.Fatalf("post-desync paste count: got %d, want 1 (pA dropped from index)", got)
	}
	if got := mustSum(t, repo, pasteOwner, now); got != 200 {
		t.Fatalf("post-desync paste sum: got %d, want 200 (Window A under-count: only pB enumerated)", got)
	}

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := mustSum(t, repo, pasteOwner, now); got != wantPasteBytes {
		t.Fatalf("post-reconcile paste sum: got %d, want %d (index reprojected, scan exact again)", got, wantPasteBytes)
	}
	if got := mustCount(t, repo, pasteOwner); got != 2 {
		t.Fatalf("post-reconcile paste count: got %d, want 2 (index rebuilt)", got)
	}
	// The rebuilt entry is a real entry, not a tombstone read.
	raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(pasteOwner, pA.Slug.String()))
	if err != nil {
		t.Fatalf("read rebuilt paste index entry: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("reconcile should have rebuilt pA's index entry, got empty")
	}
	// The heal stays inside the owner: no leak across owners.
	list, err := repo.ListByOwner(pasteOwner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	for _, p := range list {
		if p.Identity.String() != pasteOwner {
			t.Fatalf("reconcile leaked a non-owner paste: %+v", p)
		}
	}

	// --- site enumeration index heal -------------------------------------

	siteOwner := "key:reconsite"
	siteSlug := domain.Slug("reconsite")
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA: "sha-reconsite-index", Size: 400, ContentType: "text/html; charset=utf-8",
	})
	site := domain.Site{
		Slug: siteSlug, Identity: domain.Identity(siteOwner), Manifest: man,
		CreatedAt: now, UpdatedAt: now}
	const wantSiteBytes int64 = 400
	if err := repo.InsertSiteWithQuotaCheck(context.Background(), site, man.DedupedSize(), 0, now); err != nil {
		t.Fatalf("deploy site: %v", err)
	}

	if got := mustSiteSum(t, repo, siteOwner, now); got != wantSiteBytes {
		t.Fatalf("baseline site sum: got %d, want %d", got, wantSiteBytes)
	}
	if got := mustSiteCount(t, repo, siteOwner, now); got != 1 {
		t.Fatalf("baseline site count: got %d, want 1", got)
	}

	// The same crash window on the site side: the scan enumerates through
	// identity_sites, so dropping the entry drops the site's bytes.
	if err := repo.DeleteRawForTest(storage.IdentitySiteKeyForTest(siteOwner, siteSlug.String())); err != nil {
		t.Fatalf("drop site index entry: %v", err)
	}
	if got := mustSiteSum(t, repo, siteOwner, now); got != 0 {
		t.Fatalf("post-desync site sum: got %d, want 0 (Window A under-count for sites)", got)
	}
	if got := mustSiteCount(t, repo, siteOwner, now); got != 0 {
		t.Fatalf("post-desync site count: got %d, want 0 (site dropped from index)", got)
	}

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (site): %v", err)
	}
	if got := mustSiteSum(t, repo, siteOwner, now); got != wantSiteBytes {
		t.Fatalf("post-reconcile site sum: got %d, want %d (site index reprojected)", got, wantSiteBytes)
	}
	if got := mustSiteCount(t, repo, siteOwner, now); got != 1 {
		t.Fatalf("post-reconcile site count: got %d, want 1 (site index rebuilt)", got)
	}
	rawSite, err := repo.GetRawForTest(storage.IdentitySiteKeyForTest(siteOwner, siteSlug.String()))
	if err != nil {
		t.Fatalf("read rebuilt site index entry: %v", err)
	}
	if len(rawSite) == 0 {
		t.Fatalf("reconcile should have rebuilt the site's index entry, got empty")
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
	// writes from a deferred confirm goroutine; draining makes the count
	// deterministic. A no-op when nothing is pending.
	repo.WaitPendingConfirms()
	n, err := repo.CountByOwner(owner)
	if err != nil {
		t.Fatalf("count by owner: %v", err)
	}
	return n
}

func mustSiteSum(t *testing.T, repo *storage.ShaleRepo, owner string, now time.Time) int64 {
	t.Helper()
	n, err := repo.SumActiveSiteBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum active site bytes: %v", err)
	}
	return n
}

func mustSiteCount(t *testing.T, repo *storage.ShaleRepo, owner string, now time.Time) int {
	t.Helper()
	sites, err := repo.ListSitesByOwner(owner, now)
	if err != nil {
		t.Fatalf("list sites by owner: %v", err)
	}
	return len(sites)
}
