package storage_test

// The version index cache is a DISPOSABLE copy of the authoritative
// versions/<slug>/<N> rows (docs/SPEC.md "The version index cache"). These
// tests pin its two load-bearing properties: cheap reads fall back to the rows
// when the cache is absent or damaged, and NO destructive decision reads it, so
// a stale or wrong cache can never cause a wrong destructive outcome.

import (
	"context"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

var vcNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func vcInsert(t *testing.T, r *storage.ShaleRepo, slug domain.Slug, owner, sha string, size int) {
	t.Helper()
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: sha, Size: size, CreatedAt: vcNow, UpdatedAt: vcNow,
	}
	if err := r.InsertWithQuotaCheck(context.Background(), p, 0, vcNow); err != nil {
		t.Fatalf("insert %s: %v", slug, err)
	}
	r.WaitPendingConfirms()
}

func vcAppend(t *testing.T, r *storage.ShaleRepo, slug domain.Slug, sha string, size int) int {
	t.Helper()
	res, err := r.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, sha, size, 0, vcNow)
	if err != nil {
		t.Fatalf("append %s %s: %v", slug, sha, err)
	}
	return res.NewVer
}

type verSummary struct {
	VerNum  int
	Deleted bool
}

func vcListSummary(t *testing.T, r *storage.ShaleRepo, slug domain.Slug) []verSummary {
	t.Helper()
	vers, err := r.ListVersions(slug)
	if err != nil {
		t.Fatalf("ListVersions %s: %v", slug, err)
	}
	out := make([]verSummary, 0, len(vers))
	for _, v := range vers {
		out = append(out, verSummary{VerNum: v.VerNum, Deleted: v.Deleted})
	}
	return out
}

func sameSummary(a, b []verSummary) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// (a) ListVersions returns the SAME result whether the cache is present, absent,
// or corrupt: present it reads the cache, absent/corrupt it falls back to the
// authoritative rows.
func TestVersionCache_ListVersionsIdenticalPresentAbsentCorrupt(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcls0001")
	vcInsert(t, repo, slug, "key:vcls", "sha-vcls-v1", 100)
	vcAppend(t, repo, slug, "sha-vcls-v2", 200)
	vcAppend(t, repo, slug, "sha-vcls-v3", 300)
	// Tombstone v2 at the repo level (not served; head serves v3), so the set
	// carries a live-and-tombstoned mix.
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("tombstone v2: %v", err)
	}

	want := []verSummary{{3, false}, {2, true}, {1, false}} // newest-first
	present := vcListSummary(t, repo, slug)
	if !sameSummary(present, want) {
		t.Fatalf("cache-present ListVersions = %+v, want %+v", present, want)
	}

	// Absent: drop the cache key. ListVersions must scan the rows.
	if err := repo.DeleteRawForTest(storage.VersionsDocKeyForTest(slug)); err != nil {
		t.Fatalf("drop cache: %v", err)
	}
	absent := vcListSummary(t, repo, slug)
	if !sameSummary(absent, want) {
		t.Fatalf("cache-absent ListVersions = %+v, want %+v (fallback broken)", absent, want)
	}

	// Corrupt: plant undecodable JSON. ListVersions must fall open to the rows.
	mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), []byte("this-is-not-json{"))
	corrupt := vcListSummary(t, repo, slug)
	if !sameSummary(corrupt, want) {
		t.Fatalf("cache-corrupt ListVersions = %+v, want %+v (fail-open broken)", corrupt, want)
	}
}

// (b) THE CENTRAL TEST: a deliberately-wrong cache cannot cause a wrong
// destructive outcome, because every destructive decision reads the rows and
// the head, never the cache.
func TestVersionCache_WrongCacheCannotMisleadDestructiveOps(t *testing.T) {
	// Served-blob safety: rows say v2 is the newest LIVE version (v3 tombstoned),
	// so v2 is served while unpinned. A cache that lies and shows v3 LIVE would,
	// if consulted, make a guard compute newest-live=v3 and ALLOW deleting v2 -
	// freeing the served blob. The rows-based guard must refuse.
	t.Run("delete_servedGuard_readsRows", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("vcwd0001")
		vcInsert(t, repo, slug, "key:vcwd", "sha-vcwd-v1", 100)
		vcAppend(t, repo, slug, "sha-vcwd-v2", 200)
		vcAppend(t, repo, slug, "sha-vcwd-v3", 300)
		// Pin v2 so v3 can be tombstoned (the served version can't be deleted),
		// then unpin so the head serves the newest live version (v2).
		v2, err := repo.GetVersion(slug, 2)
		if err != nil {
			t.Fatalf("get v2: %v", err)
		}
		if err := repo.SetPinnedVersion(slug, v2); err != nil {
			t.Fatalf("pin v2: %v", err)
		}
		if err := repo.DeleteVersion(slug, 3); err != nil {
			t.Fatalf("tombstone v3: %v", err)
		}
		if err := repo.Unpin(slug); err != nil {
			t.Fatalf("unpin: %v", err)
		}

		// Plant the lie: v3 shown LIVE, so a cache-based newest-live would be v3.
		wrong, err := storage.VersionsDocValueForTest([]storage.VersionCacheRecordForTest{
			{VerNum: 1, ContentSHA: "sha-vcwd-v1", Size: 100},
			{VerNum: 2, ContentSHA: "sha-vcwd-v2", Size: 200},
			{VerNum: 3, ContentSHA: "sha-vcwd-v3", Size: 300, Deleted: false},
		})
		if err != nil {
			t.Fatalf("encode wrong cache: %v", err)
		}
		mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), wrong)

		// v2 IS served (rows: newest live) - the guard must say so despite the lie.
		if served, err := repo.IsVersionServed(slug, 2); err != nil || !served {
			t.Fatalf("IsVersionServed(v2) = %v, %v; want true (rows say v2 is served, the wrong cache must not free its blob)", served, err)
		}
		// v3 is a tombstone in the rows: it serves nothing, cache-lie or not.
		if served, err := repo.IsVersionServed(slug, 3); err != nil || served {
			t.Fatalf("IsVersionServed(v3) = %v, %v; want false (v3 is deleted in the rows)", served, err)
		}
	})

	// Unpin picks the newest LIVE version from the rows, never the cache: a cache
	// that shows a tombstoned version as live must not become the served head.
	t.Run("unpin_readsRows", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("vcwu0001")
		vcInsert(t, repo, slug, "key:vcwu", "sha-vcwu-v1", 100)
		vcAppend(t, repo, slug, "sha-vcwu-v2", 200)
		vcAppend(t, repo, slug, "sha-vcwu-v3", 300)
		v1, err := repo.GetVersion(slug, 1)
		if err != nil {
			t.Fatalf("get v1: %v", err)
		}
		if err := repo.SetPinnedVersion(slug, v1); err != nil {
			t.Fatalf("pin v1: %v", err)
		}
		if err := repo.DeleteVersion(slug, 3); err != nil {
			t.Fatalf("tombstone v3: %v", err)
		}

		// Plant a lie showing v3 LIVE, so a cache-based unpin would roll to v3.
		wrong, err := storage.VersionsDocValueForTest([]storage.VersionCacheRecordForTest{
			{VerNum: 1, ContentSHA: "sha-vcwu-v1", Size: 100},
			{VerNum: 2, ContentSHA: "sha-vcwu-v2", Size: 200},
			{VerNum: 3, ContentSHA: "sha-vcwu-v3", Size: 300, Deleted: false},
		})
		if err != nil {
			t.Fatalf("encode wrong cache: %v", err)
		}
		mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), wrong)

		if err := repo.Unpin(slug); err != nil {
			t.Fatalf("unpin: %v", err)
		}
		head, err := repo.Get(slug)
		if err != nil {
			t.Fatalf("get head: %v", err)
		}
		// v2 is the newest LIVE version in the rows; v3 is a tombstone.
		if head.PinnedVersion != 0 || head.ContentSHA != "sha-vcwu-v2" {
			t.Fatalf("unpin rolled head to pinned=%d sha=%q; want unpinned/sha-vcwu-v2 (rows' newest live, not the cache's lie)", head.PinnedVersion, head.ContentSHA)
		}
	})

	// Pin repoints the head from row N, never the cache: a cache lie about N does
	// not change which descriptor the head takes.
	t.Run("pin_readsRow", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("vcwp0001")
		vcInsert(t, repo, slug, "key:vcwp", "sha-vcwp-v1", 100)
		vcAppend(t, repo, slug, "sha-vcwp-v2", 200)

		// A lie showing v1 deleted; pin must still roll the head to v1's row.
		wrong, err := storage.VersionsDocValueForTest([]storage.VersionCacheRecordForTest{
			{VerNum: 1, ContentSHA: "sha-vcwp-v1", Size: 100, Deleted: true},
			{VerNum: 2, ContentSHA: "sha-vcwp-v2", Size: 200},
		})
		if err != nil {
			t.Fatalf("encode wrong cache: %v", err)
		}
		mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), wrong)

		v1, err := repo.GetVersion(slug, 1)
		if err != nil {
			t.Fatalf("get v1: %v", err)
		}
		if err := repo.SetPinnedVersion(slug, v1); err != nil {
			t.Fatalf("pin v1: %v", err)
		}
		head, err := repo.Get(slug)
		if err != nil {
			t.Fatalf("get head: %v", err)
		}
		if head.PinnedVersion != 1 || head.ContentSHA != "sha-vcwp-v1" {
			t.Fatalf("pin rolled head to pinned=%d sha=%q; want 1/sha-vcwp-v1 (from row v1, not the cache)", head.PinnedVersion, head.ContentSHA)
		}
	})
}

// (b) blob-path branch of the served-version guard: when the head carries a blob
// id (the production path), the served version is named EXACTLY by its blob id,
// so a content-sha duplicate is not mistaken for the served version. Planted
// rows exercise this branch on the untagged sha-keyed build.
func TestVersionCache_ServedGuardBlobIDExact(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcbi0001")
	owner := "key:vcbi"
	// v1 and v2 share a content sha (a re-upload of identical bytes) but have
	// distinct blob ids. The head serves v2 (unpinned).
	head := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-dup", Size: 100, CreatedAt: vcNow, UpdatedAt: vcNow,
	}
	// The head's served descriptor names v2's blob id (the production shape),
	// plus two version rows sharing the content sha but with distinct blob ids.
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), storage.PasteRowValueWithBlobForTest(head, "blob-v2"))
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), storage.VersionRowValueForTest(1, "sha-dup", "blob-v1", 100, false))
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 2), storage.VersionRowValueForTest(2, "sha-dup", "blob-v2", 100, false))

	// v2's blob is the served one; v1 is a content duplicate, NOT served.
	if served, err := repo.IsVersionServed(slug, 2); err != nil || !served {
		t.Fatalf("IsVersionServed(v2 blob path) = %v, %v; want true", served, err)
	}
	if served, err := repo.IsVersionServed(slug, 1); err != nil || served {
		t.Fatalf("IsVersionServed(v1 content-dup) = %v, %v; want false (distinct blob id)", served, err)
	}
}

// (c) A write refreshes the cache: after each insert/append/tombstone the cache
// matches the authoritative rows again.
func TestVersionCache_WritesRefreshTheCache(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcrf0001")

	assertCache := func(label string, want []verSummary) {
		t.Helper()
		recs, ok, err := repo.ReadVersionCacheForTest(slug)
		if err != nil || !ok {
			t.Fatalf("%s: cache absent/unreadable (ok=%v err=%v)", label, ok, err)
		}
		got := make([]verSummary, 0, len(recs))
		for _, r := range recs {
			got = append(got, verSummary{VerNum: r.VerNum, Deleted: r.Deleted})
		}
		// The cache is written in row order (ascending); compare as sets by vernum.
		byVer := map[int]bool{}
		for _, g := range got {
			byVer[g.VerNum] = g.Deleted
		}
		if len(byVer) != len(want) {
			t.Fatalf("%s: cache has %d records, want %d: %+v", label, len(byVer), len(want), got)
		}
		for _, w := range want {
			if del, ok := byVer[w.VerNum]; !ok || del != w.Deleted {
				t.Fatalf("%s: cache record v%d deleted=%v present=%v, want deleted=%v", label, w.VerNum, del, ok, w.Deleted)
			}
		}
	}

	vcInsert(t, repo, slug, "key:vcrf", "sha-vcrf-v1", 100)
	assertCache("after insert", []verSummary{{1, false}})

	vcAppend(t, repo, slug, "sha-vcrf-v2", 200)
	assertCache("after append", []verSummary{{1, false}, {2, false}})

	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1: %v", err)
	}
	assertCache("after tombstone", []verSummary{{1, true}, {2, false}})
}

// (d) The serving path is identical with the cache present, absent, and corrupt:
// serving reads the head's descriptor, which the cache never touches.
func TestVersionCache_ServingIdenticalRegardlessOfCache(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcsv0001")
	vcInsert(t, repo, slug, "key:vcsv", "sha-vcsv-v1", 100)
	vcAppend(t, repo, slug, "sha-vcsv-v2", 200)
	const servedSHA = "sha-vcsv-v2"

	type served struct {
		sha  string
		size int
		kind domain.ContentKind
	}
	read := func() served {
		t.Helper()
		p, err := repo.Get(slug)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return served{sha: p.ContentSHA, size: p.Size, kind: p.Kind}
	}

	present := read()
	if present.sha != servedSHA {
		t.Fatalf("served sha = %q, want %q", present.sha, servedSHA)
	}

	if err := repo.DeleteRawForTest(storage.VersionsDocKeyForTest(slug)); err != nil {
		t.Fatalf("drop cache: %v", err)
	}
	if absent := read(); absent != present {
		t.Fatalf("serving changed with the cache absent: %+v vs %+v", absent, present)
	}

	mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), []byte("not-json{"))
	if corrupt := read(); corrupt != present {
		t.Fatalf("serving changed with the cache corrupt: %+v vs %+v", corrupt, present)
	}

	// A decodable but WRONG cache (a different sha, a phantom v3) must not change
	// what is served: serving reads the head, never the cache.
	wrong, err := storage.VersionsDocValueForTest([]storage.VersionCacheRecordForTest{
		{VerNum: 1, ContentSHA: "sha-vcsv-v1", Size: 100},
		{VerNum: 2, ContentSHA: "sha-vcsv-v2", Size: 200},
		{VerNum: 3, ContentSHA: "sha-vcsv-wrong", Size: 999},
	})
	if err != nil {
		t.Fatalf("encode wrong cache: %v", err)
	}
	mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), wrong)
	if wrongC := read(); wrongC != present {
		t.Fatalf("serving changed with a wrong cache: %+v vs %+v", wrongC, present)
	}
}

// (e) Envelope-level damage (a truncated LWW envelope, NOT just invalid JSON) on
// the cache falls open to the rows, not an error. The truncated envelope makes
// the strip FAIL (a Get error), distinct from invalid JSON (which decodes past
// the strip), and the read must still return the right answer.
func TestVersionCache_TruncatedEnvelopeFallsBack(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcen0001")
	vcInsert(t, repo, slug, "key:vcen", "sha-vcen-v1", 100)
	vcAppend(t, repo, slug, "sha-vcen-v2", 200)

	// A valid LWW envelope, then truncated mid-header so cluster.Decode fails.
	full := cluster.Encode(cluster.Envelope{
		Stamp:   cluster.Stamp{TimestampNanos: uint64(vcNow.UnixNano()), NodeID: "n"},
		Payload: []byte(`{"versions":[]}`),
	})
	if len(full) == 0 || full[0] != 0xE0 {
		t.Fatalf("setup: expected an LWW envelope (magic 0xE0)")
	}
	truncated := full[:4] // keep the magic, drop the rest of the stamp/payload
	mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), truncated)

	// The strip fails on this value: the raw read errors, which is what makes it
	// ENVELOPE damage rather than invalid JSON.
	if _, err := repo.GetRawForTest(storage.VersionsDocKeyForTest(slug)); err == nil {
		t.Fatalf("setup: a truncated envelope must make the strip fail; got nil error")
	}

	// The cheap read must fall open to the rows, not surface the strip error.
	got := vcListSummary(t, repo, slug)
	want := []verSummary{{2, false}, {1, false}}
	if !sameSummary(got, want) {
		t.Fatalf("ListVersions over a truncated-envelope cache = %+v, want %+v (fail-open broken)", got, want)
	}
}

// (f) An "old binary" write (a row flipped WITHOUT touching the cache) leaves the
// cache stale, yet no destructive op is misled (they read the rows). A new append
// upserts ONLY its own row (no rescan), so it does NOT heal the row-level
// staleness; a whole re-stamp (unpin) does. This is the rolling-deploy safety
// argument as a test, and it pins that append maintains the cache by upsert.
func TestVersionCache_OldBinaryWriteLeavesStaleCacheButStaysSafe(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("vcob0001")
	vcInsert(t, repo, slug, "key:vcob", "sha-vcob-v1", 100)
	vcAppend(t, repo, slug, "sha-vcob-v2", 200)
	vcAppend(t, repo, slug, "sha-vcob-v3", 300)

	// Old binary tombstones v1 (a legitimate non-served delete) by writing the
	// ROW directly, with no cache code, so the cache is left stale.
	oldRow, err := storage.LegacyVersionValueForTest(1, domain.KindHTML, "sha-vcob-v1", 100, vcNow, true)
	if err != nil {
		t.Fatalf("encode old-binary tombstone row: %v", err)
	}
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), oldRow)

	assertCacheShowsV1Live := func(label string) {
		t.Helper()
		recs, ok, err := repo.ReadVersionCacheForTest(slug)
		if err != nil || !ok {
			t.Fatalf("%s: cache absent/unreadable: ok=%v err=%v", label, ok, err)
		}
		for _, r := range recs {
			if r.VerNum == 1 && r.Deleted {
				t.Fatalf("%s: expected the stale cache to still show v1 LIVE, got it deleted: %+v", label, recs)
			}
		}
	}
	// The cache is now STALE: it still shows v1 as live.
	assertCacheShowsV1Live("after the old-binary row tombstone")

	assertServedSafe := func(label string, servedVer int) {
		t.Helper()
		// The served-version guard reads the rows and the head: the newest live
		// version is served, and v1 (tombstoned) never is, despite the stale cache.
		if served, err := repo.IsVersionServed(slug, servedVer); err != nil || !served {
			t.Fatalf("%s: IsVersionServed(v%d) = %v, %v; want true from the rows", label, servedVer, served, err)
		}
		if served, err := repo.IsVersionServed(slug, 1); err != nil || served {
			t.Fatalf("%s: IsVersionServed(v1) = %v, %v; want false (v1 tombstoned in the rows)", label, served, err)
		}
	}
	assertServedSafe("with the stale cache", 3)

	// A new append UPSERTS its own row with no rescan, so it does NOT heal the
	// row-level staleness: the cache still shows v1 live and the cheap read stays
	// stale (all four versions read as live). Destructive ops remain safe.
	vcAppend(t, repo, slug, "sha-vcob-v4", 400)
	assertCacheShowsV1Live("after a new-binary append")
	assertServedSafe("after a new-binary append", 4)
	stale := vcListSummary(t, repo, slug)
	staleWant := []verSummary{{4, false}, {3, false}, {2, false}, {1, false}}
	if !sameSummary(stale, staleWant) {
		t.Fatalf("append must not rescan: ListVersions = %+v, want the stale %+v (append should upsert, not heal)", stale, staleWant)
	}

	// A whole re-stamp (unpin) scans the rows and rewrites the document, healing
	// the stale entry: the cheap read now reflects v1 as deleted.
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin (whole re-stamp): %v", err)
	}
	healed := vcListSummary(t, repo, slug)
	healedWant := []verSummary{{4, false}, {3, false}, {2, false}, {1, true}}
	if !sameSummary(healed, healedWant) {
		t.Fatalf("after a whole re-stamp, ListVersions = %+v, want %+v (heal broken)", healed, healedWant)
	}
}
