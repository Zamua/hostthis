//go:build slatedb

package storage_test

// The scan-derived quota SUMS THE CACHED VALUES of the enumeration entries: one
// prefix scan, zero per-entry fan-out to the {slug} shards. Each entry caches
// the paste's live byte sum and expires_at, and the scan trusts exactly those
// fields. The freshness contract pinned here:
//
//   - every size-changing operation maintains the cached size,
//   - the scan does NO per-entry follow-up reads, observable because a
//     stale-but-decodable entry whose authoritative rows are ABSENT still
//     contributes its cached values,
//   - the reconciler's reprojection is the drift healer, rebuilding cached
//     values and pruning orphans even under an owner with no records left,
//   - fail-closed (Policy 3): an undecodable entry or a fail-closed
//     placeholder HARD-FAILS the scan, since skipping would under-count,
//   - shale's quota result equals sqlite's on the same op sequence, and an
//     out-of-band index corruption is healed back to parity.
//
//	go test -tags slatedb -run 'TestShaleQuotaScan|TestShaleSiteQuotaScan|TestShaleReconcileDoesNotResurrect|TestShaleQuotaParity' ./internal/storage
//
// All tests skip unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleQuotaScanSumsCachedIndexValues(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale cached-values test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:cachesz"
	slug := domain.Slug("cachesz1")

	// confirmInsert seeds the cached size at the live sum.
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-cachesz-v1", Size: 300,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	repo.WaitPendingConfirms()
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	if got := readCachedIndexSize(t, repo, idxKey); got != 300 {
		t.Fatalf("cached size after insert: got %d, want 300 (seeded at v1)", got)
	}

	// Append v2 = +200: the append must refresh the cached size to the live
	// sum, since the scan sums nothing else.
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-cachesz-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
		t.Fatalf("cached size after append: got %d, want 500 (append must refresh the cache)", got)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after append: got %d, want 500", got)
	}

	// A tombstone sheds its bytes from the cached size.
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 200 {
		t.Fatalf("cached size after tombstone: got %d, want 200 (tombstone must refresh the cache)", got)
	}
	if got := mustSum(t, repo, owner, now); got != 200 {
		t.Fatalf("sum after tombstone: got %d, want 200", got)
	}

	// The no-fan-out contract, made observable: a stale-but-decodable entry
	// whose authoritative rows are ABSENT still contributes its cached values,
	// because the scan never resolves the head row. Bounded over-count: it
	// counts until the reconciler prunes it.
	staleKey := storage.IdentityPasteKeyForTest(owner, "cachegon")
	writeIndexEntryJSON(t, repo, staleKey, 999, now)
	if got := mustSum(t, repo, owner, now); got != 200+999 {
		t.Fatalf("a stale-but-decodable entry must contribute its cached value (no per-entry fan-out): got %d, want %d", got, 200+999)
	}

	// An EXPIRED stale entry self-excludes via its cached expires_at.
	expiredKey := storage.IdentityPasteKeyForTest(owner, "cacheexp")
	writeIndexEntryJSON(t, repo, expiredKey, 5000, now.Add(-time.Hour))
	if got := mustSum(t, repo, owner, now); got != 200+999 {
		t.Fatalf("an expired stale entry must self-exclude (cached expiry): got %d, want %d", got, 200+999)
	}

	// The cache IS the measure, so out-of-band corruption is reflected by the
	// scan and healed by the reprojection.
	writeCachedIndexSize(t, repo, idxKey, 1)
	if got := mustSum(t, repo, owner, now); got != 1+999 {
		t.Fatalf("the scan sums the cached size (corrupted to 1): got %d, want %d", got, 1+999)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 200 {
		t.Fatalf("reprojection must rebuild the cached size from the authoritative rows: got %d, want 200", got)
	}
	if got := mustSum(t, repo, owner, now); got != 200 {
		t.Fatalf("sum after reconcile: got %d, want 200 (stale entries pruned, cache healed)", got)
	}
	if raw, err := repo.GetRawForTest(staleKey); err != nil {
		t.Fatalf("read stale entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("reconcile must prune the stale entry (authoritative paste gone); got %q", raw)
	}

	// The orphan prune must be GLOBAL: a per-owner reprojection never visits
	// an owner with no authoritative pastes, so its entry would over-count
	// forever.
	orphanOwner := "key:cacheorph"
	orphanKey := storage.IdentityPasteKeyForTest(orphanOwner, "orphgone")
	writeIndexEntryJSON(t, repo, orphanKey, 777, now)
	if got := mustSum(t, repo, orphanOwner, now); got != 777 {
		t.Fatalf("pre-reconcile orphan sum: got %d, want 777 (cached value counts until pruned)", got)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (orphan): %v", err)
	}
	if got := mustSum(t, repo, orphanOwner, now); got != 0 {
		t.Fatalf("post-reconcile orphan sum: got %d, want 0 (global prune must cover owners with no rows)", got)
	}
	if raw, err := repo.GetRawForTest(orphanKey); err != nil {
		t.Fatalf("read orphan entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("reconcile must prune an orphan entry under an owner with no authoritative pastes; got %q", raw)
	}
}

// TestShaleQuotaScanFailClosed pins Policy 3: an entry that does not decode, or
// that carries the reconciler's fail-closed placeholder, HARD-FAILS the check
// and rejects the upload. Skipping it would under-count and over-admit. The
// reconciler prunes a bogus entry, restoring the owner's checks.
func TestShaleQuotaScanFailClosed(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale fail-closed test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:qfc"
	good := domain.Paste{
		Slug: domain.Slug("qfcgood1"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-qfc", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), good, 0, now); err != nil {
		t.Fatalf("insert good: %v", err)
	}
	repo.WaitPendingConfirms()

	// Undecodable ENTRY: hard-fail.
	badKey := storage.IdentityPasteKeyForTest(owner, "qfcbad11")
	if err := repo.PutRawForTest(badKey, corruptJSON); err != nil {
		t.Fatalf("seed corrupt entry: %v", err)
	}
	if got, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatalf("an undecodable entry must HARD-FAIL the quota scan (Policy 3); got %d, nil error", got)
	}
	if err := repo.DeleteRawForTest(badKey); err != nil {
		t.Fatalf("clear corrupt entry: %v", err)
	}

	// Fail-closed PLACEHOLDER entry, what the reconciler projects for an
	// undecodable authoritative record: hard-fail.
	if err := repo.PutRawForTest(badKey, []byte(`{"placeholder":true}`)); err != nil {
		t.Fatalf("seed placeholder entry: %v", err)
	}
	if got, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatalf("a fail-closed placeholder entry must HARD-FAIL the quota scan; got %d, nil error", got)
	}

	// A placeholder whose authoritative row is ABSENT is bogus: the prune
	// confirms the row is gone and drops it, restoring the owner's checks.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 100 {
		t.Fatalf("sum after reconcile prunes the bogus placeholder: got %d, want 100", got)
	}
}

// TestShaleReconcileDoesNotResurrectFailedPasteEntry pins the failed-status
// index policy: MarkFailed drops the enumeration entry, the reprojection never
// re-adds a failed row's entry, and the prune drops a leftover one. Without it
// the quota scan - which sums whatever the index lists, with no per-entry
// status read - counts a failed paste's bytes forever.
func TestShaleReconcileDoesNotResurrectFailedPasteEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale failed-entry test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:failstay"
	p := insertPending(t, repo, owner, "failstay", 400, now)
	if err := repo.MarkFailed(p.Slug); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	idxKey := storage.IdentityPasteKeyForTest(owner, p.Slug.String())
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("sum after failed: got %d, want 0", got)
	}

	// The reprojection must not resurrect the failed paste's entry.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if raw, err := repo.GetRawForTest(idxKey); err != nil {
		t.Fatalf("read entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("reconcile must not resurrect a failed paste's enumeration entry; got %q", raw)
	}
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("sum after reconcile: got %d, want 0", got)
	}

	// A leftover entry models a crash between MarkFailed's status flip and its
	// entry drop: it over-counts for the bounded window, and the prune drops
	// it even though the head row still exists, because that row is failed.
	writeIndexEntryJSON(t, repo, idxKey, 400, now)
	if got := mustSum(t, repo, owner, now); got != 400 {
		t.Fatalf("leftover failed-paste entry should count until pruned (bounded over-count): got %d, want 400", got)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (leftover): %v", err)
	}
	if raw, err := repo.GetRawForTest(idxKey); err != nil {
		t.Fatalf("read leftover entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("the prune must drop a failed paste's leftover entry; got %q", raw)
	}
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("sum after leftover prune: got %d, want 0", got)
	}
}

// TestShaleSiteQuotaScanSumsCachedIndexValues is the site mirror: the
// identity_sites entry is value-bearing (cached deduped size + expiry), deploy
// and replace maintain it, a bare marker entry falls back to the authoritative
// row until the reconciler enriches it, orphans are pruned globally, and a
// fail-closed placeholder hard-fails the scan.
func TestShaleSiteQuotaScanSumsCachedIndexValues(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale site cached-values test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:sitecache"
	slug := domain.Slug("sitecach")

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA: "sha-sitecache-index", Size: 400, ContentType: "text/html; charset=utf-8",
	})
	site := domain.Site{
		Slug: slug, Identity: domain.Identity(owner), Manifest: man,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertSiteWithQuotaCheck(context.Background(), site, 400, 0, now); err != nil {
		t.Fatalf("deploy site: %v", err)
	}

	// The deploy writes the value-bearing projection, not a bare marker.
	idxKey := storage.IdentitySiteKeyForTest(owner, slug.String())
	if got := readCachedIndexSize(t, repo, idxKey); got != 400 {
		t.Fatalf("site entry cached size after deploy: got %d, want 400 (value-bearing entry)", got)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 400 {
		t.Fatalf("site sum after deploy: got %d, want 400", got)
	}

	// An in-place re-deploy refreshes the cached values.
	man2 := domain.NewManifest()
	man2.Add("index.html", domain.ManifestEntry{
		SHA: "sha-sitecache-index-v2", Size: 250, ContentType: "text/html; charset=utf-8",
	})
	site2 := domain.Site{
		Slug: slug, Identity: domain.Identity(owner), Manifest: man2,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.ReplaceSiteWithQuotaCheck(context.Background(), site2, 250, 0, now); err != nil {
		t.Fatalf("replace site: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 250 {
		t.Fatalf("site entry cached size after replace: got %d, want 250 (replace must refresh the cache)", got)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 250 {
		t.Fatalf("site sum after replace: got %d, want 250", got)
	}

	// A bare marker entry: the sum falls back to the authoritative row, and
	// the reconciler enriches the entry to the JSON projection.
	if err := repo.PutRawForTest(idxKey, storage.MarkerValueForTest()); err != nil {
		t.Fatalf("seed legacy marker entry: %v", err)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 250 {
		t.Fatalf("legacy marker entry must fall back to the authoritative row: got %d, want 250", got)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (enrich): %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 250 {
		t.Fatalf("reconcile must enrich the legacy marker to the JSON projection: got %d, want 250", got)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 250 {
		t.Fatalf("site sum after enrichment: got %d, want 250", got)
	}

	// Global orphan prune, site side: a stale entry under an owner with no
	// authoritative sites counts until pruned.
	orphanOwner := "key:sitecorph"
	orphanKey := storage.IdentitySiteKeyForTest(orphanOwner, "sitegone")
	writeIndexEntryJSON(t, repo, orphanKey, 777, now)
	if got := mustSiteSum(t, repo, orphanOwner, now); got != 777 {
		t.Fatalf("pre-reconcile orphan site sum: got %d, want 777 (cached value counts until pruned)", got)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (site orphan): %v", err)
	}
	if got := mustSiteSum(t, repo, orphanOwner, now); got != 0 {
		t.Fatalf("post-reconcile orphan site sum: got %d, want 0 (global prune)", got)
	}
	if raw, err := repo.GetRawForTest(orphanKey); err != nil {
		t.Fatalf("read orphan site entry post-reconcile: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("reconcile must prune an orphan site entry; got %q", raw)
	}

	// Fail-closed placeholder hard-fails the site scan.
	badKey := storage.IdentitySiteKeyForTest(owner, "sitebad1")
	if err := repo.PutRawForTest(badKey, []byte(`{"placeholder":true}`)); err != nil {
		t.Fatalf("seed placeholder site entry: %v", err)
	}
	if got, err := repo.SumActiveSiteBytesByOwner(owner, now); err == nil {
		t.Fatalf("a fail-closed placeholder site entry must HARD-FAIL the scan; got %d, nil error", got)
	}
}

// TestShaleQuotaParityWithSqlite drives the same op sequence (inserts, appends,
// version tombstones) against both backends and asserts the quota sums agree at
// every step, then corrupts shale's index entry out-of-band and asserts the
// reprojection restores parity.
func TestShaleQuotaParityWithSqlite(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale/sqlite parity test (start dev MinIO first)")
	}
	shale := newShaleRepoOnUniqueDB(t, endpoint)

	db, err := storage.Open(filepath.Join(t.TempDir(), "parity.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sq := storage.NewPasteRepo(db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:parity"
	p1 := domain.Paste{
		Slug: domain.Slug("parity1a"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-parity1", Size: 300,
		CreatedAt: now, UpdatedAt: now}
	p2 := domain.Paste{
		Slug: domain.Slug("parity2b"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-parity2", Size: 200,
		CreatedAt: now, UpdatedAt: now}

	assertParity := func(step string) {
		t.Helper()
		shaleSum, err := shale.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("%s: shale sum: %v", step, err)
		}
		sqSum, err := sq.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("%s: sqlite sum: %v", step, err)
		}
		if shaleSum != sqSum {
			t.Fatalf("%s: quota parity broken: shale=%d sqlite=%d", step, shaleSum, sqSum)
		}
	}

	for _, ins := range []domain.Paste{p1, p2} {
		if err := shale.InsertWithQuotaCheck(context.Background(), ins, 0, now); err != nil {
			t.Fatalf("shale insert %s: %v", ins.Slug, err)
		}
		if err := sq.InsertWithQuotaCheck(context.Background(), ins, 0, now); err != nil {
			t.Fatalf("sqlite insert %s: %v", ins.Slug, err)
		}
	}
	shale.WaitPendingConfirms()
	assertParity("after inserts")

	if _, err := shale.AppendVersionWithQuotaCheck(context.Background(), p1.Slug, domain.KindHTML, "sha-parity1-v2", 150, 0, now); err != nil {
		t.Fatalf("shale append: %v", err)
	}
	if _, err := sq.AppendVersionWithQuotaCheck(context.Background(), p1.Slug, domain.KindHTML, "sha-parity1-v2", 150, 0, now); err != nil {
		t.Fatalf("sqlite append: %v", err)
	}
	assertParity("after append")

	if err := shale.DeleteVersion(p1.Slug, 1); err != nil {
		t.Fatalf("shale tombstone: %v", err)
	}
	if err := sq.DeleteVersion(p1.Slug, 1); err != nil {
		t.Fatalf("sqlite tombstone: %v", err)
	}
	assertParity("after tombstone")

	if _, err := shale.AppendVersionWithQuotaCheck(context.Background(), p2.Slug, domain.KindHTML, "sha-parity2-v2", 50, 0, now); err != nil {
		t.Fatalf("shale append p2: %v", err)
	}
	if _, err := sq.AppendVersionWithQuotaCheck(context.Background(), p2.Slug, domain.KindHTML, "sha-parity2-v2", 50, 0, now); err != nil {
		t.Fatalf("sqlite append p2: %v", err)
	}
	assertParity("after second append")

	// The divergence proves the corruption landed in the thing the scan
	// sums...
	idxKey := storage.IdentityPasteKeyForTest(owner, p1.Slug.String())
	writeCachedIndexSize(t, shale, idxKey, 999999)
	shaleSum, err := shale.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("shale sum (corrupted): %v", err)
	}
	sqSum, err := sq.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sqlite sum: %v", err)
	}
	if shaleSum == sqSum {
		t.Fatalf("corruption did not land in the cached measure: shale=%d sqlite=%d", shaleSum, sqSum)
	}
	// ...and the reprojection heals it back to parity.
	if err := shale.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertParity("after reconcile heals the corruption")
}

// --- helpers ---------------------------------------------------------------

// readCachedIndexSize returns the cached "size" on the entry at idxKey, failing
// the test if the entry is absent or undecodable.
func readCachedIndexSize(t *testing.T, repo *storage.ShaleRepo, idxKey []byte) int {
	t.Helper()
	raw, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read index entry: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("index entry %q absent", idxKey)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode index entry %q: %v", raw, err)
	}
	sz, ok := m["size"].(float64)
	if !ok {
		t.Fatalf("index entry has no numeric size field: %v", m)
	}
	return int(sz)
}

// writeCachedIndexSize overwrites the cached "size" at idxKey, preserving the
// other cached fields and bypassing the CAS write path. Models a corrupt
// denormalized cache: a valid entry whose size disagrees with its rows.
func writeCachedIndexSize(t *testing.T, repo *storage.ShaleRepo, idxKey []byte, want int) {
	t.Helper()
	raw, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read index entry for corruption: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode index entry for corruption: %v", err)
	}
	m["size"] = want
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode corrupted index entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, out); err != nil {
		t.Fatalf("write corrupted index entry: %v", err)
	}
}

// writeIndexEntryJSON plants a decodable entry at idxKey with the given cached
// size + expiry, bypassing the CAS write path. Models an orphaned entry whose
// authoritative rows may not exist, the shape a crash mid-delete leaves. The
// field set is the shared subset of identityPasteRow / identitySiteRow, so one
// helper serves both families.
func writeIndexEntryJSON(t *testing.T, repo *storage.ShaleRepo, idxKey []byte, size int, createdAt time.Time) {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"name":       "",
		"size":       size,
		"created_at": createdAt,
	})
	if err != nil {
		t.Fatalf("encode index entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, out); err != nil {
		t.Fatalf("write index entry: %v", err)
	}
}
