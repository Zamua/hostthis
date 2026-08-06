//go:build slatedb

package storage_test

// The scan-derived quota SUMS THE CACHED VALUES of the enumeration entries: one
// prefix scan, zero per-entry fan-out to the {slug} shards. Each entry caches
// the paste's live byte sum, and the scan trusts exactly that
// fields. The freshness contract pinned here:
//
//   - every size-changing operation maintains the cached size,
//   - the scan does NO per-entry follow-up reads, observable because a
//     stale-but-decodable entry whose authoritative rows are ABSENT still
//     contributes its cached values,
//   - nothing heals drift in the background: a corrupted cached value
//     persists until that slug's own next write, and an orphan entry is
//     never pruned,
//   - fail-closed (Policy 2): an undecodable entry or a fail-closed
//     placeholder HARD-FAILS the scan, since skipping would under-count,
//   - shale's quota result equals sqlite's on the same op sequence.
//
//	go test -tags slatedb -run 'TestShaleQuotaScan|TestShaleSiteQuotaScan|TestShaleListDoesNotResurrect|TestShaleQuotaParity' ./internal/storage
//
// All tests skip unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"encoding/json"
	"errors"
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
	// counts until the owner's next list prunes it.
	staleKey := storage.IdentityPasteKeyForTest(owner, "cachegon")
	writeIndexEntryJSON(t, repo, staleKey, 999, now)
	if got := mustSum(t, repo, owner, now); got != 200+999 {
		t.Fatalf("a stale-but-decodable entry must contribute its cached value (no per-entry fan-out): got %d, want %d", got, 200+999)
	}

	// Nothing rebuilds the cache in the background, so out-of-band corruption
	// persists: the write paths are the only maintainers, and a list reads no
	// authoritative rows to compare against.
	writeCachedIndexSize(t, repo, idxKey, 1)
	if got := mustSum(t, repo, owner, now); got != 1+999 {
		t.Fatalf("the scan sums the cached size (corrupted to 1): got %d, want %d", got, 1+999)
	}
	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 1 {
		t.Fatalf("a list must not rebuild the cached size: got %d, want 1 (unchanged)", got)
	}

	// The phantom is listed, not pruned: its entry survives and keeps counting.
	if raw, err := repo.GetRawForTest(staleKey); err != nil {
		t.Fatalf("read phantom entry after list: %v", err)
	} else if len(raw) == 0 {
		t.Fatal("a phantom entry must survive a list, not be pruned")
	}
	if got := mustSum(t, repo, owner, now); got != 1+999 {
		t.Fatalf("sum after list: got %d, want %d (nothing healed, nothing pruned)", got, 1+999)
	}

	// The next write to that slug is what corrects its cached size.
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-cachesz-v3", 50, 0, now); err != nil {
		t.Fatalf("append v3: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 250 {
		t.Fatalf("the slug's own next write must refresh its cache: got %d, want 250", got)
	}

	// An orphan entry is permanent: no read prunes it, not its owner's own
	// list and not anyone else's (docs/SPEC.md "Phantom entries are accepted,
	// not repaired"). Deleting the paste is the owner's way out.
	orphanOwner := "key:cacheorph"
	orphanKey := storage.IdentityPasteKeyForTest(orphanOwner, "orphgone")
	writeIndexEntryJSON(t, repo, orphanKey, 777, now)
	for _, who := range []string{owner, orphanOwner} {
		if _, err := repo.ListByOwner(who); err != nil {
			t.Fatalf("list (%s): %v", who, err)
		}
	}
	if got := mustSum(t, repo, orphanOwner, now); got != 777 {
		t.Fatalf("no list may prune an orphan entry: got %d, want 777", got)
	}
	if raw, err := repo.GetRawForTest(orphanKey); err != nil {
		t.Fatalf("read orphan entry after both lists: %v", err)
	} else if len(raw) == 0 {
		t.Fatal("the orphan entry must survive: nothing deletes on absence")
	}
	// A LEGACY-shaped orphan cannot be rendered without its row, so the list
	// omits it - while the quota scan, which reads only the entry, still
	// counts it. The two surfaces disagree, and that is the accepted cost of
	// the listing never resolving rows.
	got, err := repo.ListByOwner(orphanOwner)
	if err != nil {
		t.Fatalf("list orphan owner: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an unrenderable legacy orphan must be omitted from the list: got %+v", got)
	}

	// A FAT orphan - the shape a crash between the two writes leaves today -
	// carries everything the list renders, so it IS shown. That visibility is
	// what makes a phantom clearable by its owner.
	fatKey := storage.IdentityPasteKeyForTest(orphanOwner, "orphfat1")
	writeFatIndexEntryJSON(t, repo, fatKey, 123, now)
	got, err = repo.ListByOwner(orphanOwner)
	if err != nil {
		t.Fatalf("list orphan owner (fat): %v", err)
	}
	if len(got) != 1 || got[0].Slug != "orphfat1" {
		t.Fatalf("a fat phantom must be listed to its owner: got %+v", got)
	}
}

// TestShaleQuotaScanFailClosed pins Policy 2: an entry that does not decode,
// or that carries a fail-closed placeholder, HARD-FAILS the check and rejects
// the upload. Skipping it would under-count and over-admit. Nothing writes a
// placeholder any more, so one can only arrive from a store an older
// deployment wrote; nothing clears it either, which is the operator-repair
// contract stated in docs/SPEC.md.
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
		t.Fatalf("an undecodable entry must HARD-FAIL the quota scan (Policy 2); got %d, nil error", got)
	}
	if err := repo.DeleteRawForTest(badKey); err != nil {
		t.Fatalf("clear corrupt entry: %v", err)
	}

	// Fail-closed PLACEHOLDER entry, the shape an older deployment projected
	// for an undecodable authoritative record: hard-fail.
	if err := repo.PutRawForTest(badKey, []byte(`{"placeholder":true}`)); err != nil {
		t.Fatalf("seed placeholder entry: %v", err)
	}
	if got, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatalf("a fail-closed placeholder entry must HARD-FAIL the quota scan; got %d, nil error", got)
	}

	// No read clears it: the owner stays locked out until an operator removes
	// the entry. Fail-safe (refuses writes) rather than silently under-counting.
	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatal("a list must not clear a placeholder; the scan must still hard-fail")
	}
	if err := repo.DeleteRawForTest(badKey); err != nil {
		t.Fatalf("operator repair: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 100 {
		t.Fatalf("sum after operator repair: got %d, want 100", got)
	}
}

// TestShaleListDoesNotResurrectFailedPasteEntry pins the failed-status index
// policy: MarkFailed drops the enumeration entry and no read re-adds it. A
// leftover entry from a crash mid-MarkFailed is a phantom like any other -
// counted, listed, and cleared only by its owner.
func TestShaleListDoesNotResurrectFailedPasteEntry(t *testing.T) {
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

	// No read resurrects the failed paste's entry.
	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	}
	if raw, err := repo.GetRawForTest(idxKey); err != nil {
		t.Fatalf("read entry after list: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("a list must not resurrect a failed paste's enumeration entry; got %q", raw)
	}
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("sum after list: got %d, want 0", got)
	}

	// A leftover entry models a crash between MarkFailed's status flip and its
	// entry drop. It over-counts permanently: the failed row is never read, so
	// nothing can tell this entry from a live one.
	writeIndexEntryJSON(t, repo, idxKey, 400, now)
	if got := mustSum(t, repo, owner, now); got != 400 {
		t.Fatalf("leftover failed-paste entry counts: got %d, want 400", got)
	}
	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list (leftover): %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 400 {
		t.Fatalf("a list must not drop the leftover entry: got %d, want 400", got)
	}
	// Delete is the owner's way out, and it works on the entry alone.
	if err := repo.Delete(p.Slug); err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("sum after the owner deletes it: got %d, want 0", got)
	}
}

// TestShaleSiteQuotaScanSumsCachedIndexValues is the site mirror: the
// identity_sites entry is value-bearing (cached deduped size), deploy
// and replace maintain it, a bare marker entry falls back to the authoritative
// row until the owner's next list enriches it, orphans are never pruned, and a
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
	// the owner's next list enriches the entry to the full projection.
	if err := repo.PutRawForTest(idxKey, storage.MarkerValueForTest()); err != nil {
		t.Fatalf("seed legacy marker entry: %v", err)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 250 {
		t.Fatalf("legacy marker entry must fall back to the authoritative row: got %d, want 250", got)
	}
	if _, err := repo.ListSitesByOwner(owner, now); err != nil {
		t.Fatalf("list (enrich): %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != 250 {
		t.Fatalf("the list must enrich the legacy marker to the full projection: got %d, want 250", got)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 250 {
		t.Fatalf("site sum after enrichment: got %d, want 250", got)
	}

	// Site phantoms behave exactly like paste phantoms: no list prunes them.
	orphanOwner := "key:sitecorph"
	orphanKey := storage.IdentitySiteKeyForTest(orphanOwner, "sitegone")
	writeIndexEntryJSON(t, repo, orphanKey, 777, now)
	for _, who := range []string{owner, orphanOwner} {
		if _, err := repo.ListSitesByOwner(who, now); err != nil {
			t.Fatalf("list (%s): %v", who, err)
		}
	}
	if got := mustSiteSum(t, repo, orphanOwner, now); got != 777 {
		t.Fatalf("no list may prune an orphan site entry: got %d, want 777", got)
	}
	if raw, err := repo.GetRawForTest(orphanKey); err != nil {
		t.Fatalf("read orphan site entry after both lists: %v", err)
	} else if len(raw) == 0 {
		t.Fatal("the orphan site entry must survive: nothing deletes on absence")
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
// every step, then corrupts shale's index entry out-of-band and asserts that
// the slug's own next write - not any background pass - restores parity.
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
	// ...a list does NOT heal it - nothing reads the authoritative rows...
	if _, err := shale.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := readCachedIndexSize(t, shale, idxKey); got != 999999 {
		t.Fatalf("a list must not heal the corrupted cache: got %d, want 999999", got)
	}

	// ...and the slug's own next write is what restores parity.
	if _, err := shale.AppendVersionWithQuotaCheck(context.Background(), p1.Slug, domain.KindHTML, "sha-parity1-v3", 25, 0, now); err != nil {
		t.Fatalf("shale append p1 v3: %v", err)
	}
	if _, err := sq.AppendVersionWithQuotaCheck(context.Background(), p1.Slug, domain.KindHTML, "sha-parity1-v3", 25, 0, now); err != nil {
		t.Fatalf("sqlite append p1 v3: %v", err)
	}
	assertParity("after the slug's own next write restores parity")
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
// size, bypassing the CAS write path. Models an orphaned entry whose
// authoritative rows may not exist, the shape a crash mid-delete leaves. The
// field set is the shared subset of identityPasteRow / identitySiteRow, so one
// helper serves both families.
// writeFatIndexEntryJSON writes an entry carrying the display fields, the shape
// the current insert path produces.
func writeFatIndexEntryJSON(t *testing.T, repo *storage.ShaleRepo, idxKey []byte, size int, at time.Time) {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"name": "", "size": size, "served_size": size, "created_at": at,
		"kind": string(domain.KindHTML), "latest_version": 1, "updated_at": at,
	})
	if err != nil {
		t.Fatalf("encode fat index entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, out); err != nil {
		t.Fatalf("write fat index entry: %v", err)
	}
}

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

// TestShaleListReportsServedAndStoredSizes pins that a multi-version paste's
// list row distinguishes the SERVED version's bytes from the total the quota
// charges, both served from the cached entry with no authoritative read.
// Collapsing them hides the multi-version size note from every owner.
func TestShaleListReportsServedAndStoredSizes(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale served-size test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:servedsz"
	slug := domain.Slug("servedsz")
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-served-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-served-v2", 50, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}

	got, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list: got %d items, want 1", len(got))
	}
	if got[0].Size != 50 {
		t.Errorf("served size: got %d, want 50 (v2, the head version)", got[0].Size)
	}
	if got[0].StoredBytes != 350 {
		t.Errorf("stored bytes: got %d, want 350 (both live versions)", got[0].StoredBytes)
	}

	// An entry missing the served size is incomplete, not renderable: the list
	// must resolve it rather than report zero.
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	incomplete, err := json.Marshal(map[string]any{
		"name": "", "size": 350, "created_at": now,
		"kind": string(domain.KindHTML), "latest_version": 2, "updated_at": now,
	})
	if err != nil {
		t.Fatalf("encode incomplete entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, incomplete); err != nil {
		t.Fatalf("write incomplete entry: %v", err)
	}
	got, err = repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list (incomplete entry): %v", err)
	}
	if len(got) != 1 || got[0].Size != 50 {
		t.Fatalf("an entry with no served size must be resolved, not rendered as zero: got %+v", got)
	}
}
