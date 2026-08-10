package storage_test

// Append numbers from the head's monotonic high-water mark, not a version scan
// (docs/SPEC.md "Numbering also leaves the version scan"). These tests pin the
// numbering invariant - a number is NEVER reused, not for a tombstone, not under
// a stale-low mark, not under concurrent appends - and that the fast path takes
// no version-family scan while the pre-cache migration path takes exactly one.
// They reuse the vc* helpers from versions_doc_test.go.

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// (1) A tombstoned number is never reused, even when the head high-water mark is
// forced stale-low (below the tombstoned top). The cache and the recovery scan
// both count the tombstone, and the candidate absent-check on the still-present
// tombstone row is the final backstop.
//
// SABOTAGE: number from a live-only max (maxVerNum -> latestActiveVerNum on the
// cache and scan sources) -> the append proposes the tombstoned number, the
// absent-check refuses it every attempt, and the append exhausts its renumber
// attempts and errors. No number is ever reused.
func TestAppendNumbering_TombstonedNumberNotReusedUnderStaleLowMark(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("annt0001")
	vcInsert(t, repo, slug, "key:annt", "sha-annt-v1", 100)
	vcAppend(t, repo, slug, "sha-annt-v2", 100)
	vcAppend(t, repo, slug, "sha-annt-v3", 100)

	// Tombstone the TOP version, then force the mark below it. A tombstone keeps
	// its row and does not lower the mark; the forced-low mark stands in for a
	// corrupted or legacy mark that under-counts.
	if err := repo.DeleteVersion(slug, 3); err != nil {
		t.Fatalf("tombstone v3: %v", err)
	}
	if err := repo.ForceHeadLatestVersionForTest(slug, 1); err != nil {
		t.Fatalf("force stale-low mark: %v", err)
	}

	res, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-annt-v4", 100, 0, vcNow)
	if err != nil {
		t.Fatalf("append after tombstone under a stale-low mark: %v", err)
	}
	// The tombstoned top is number 3; the new number must be strictly above it.
	if res.NewVer != 4 {
		t.Fatalf("append numbered %d; want 4 (a tombstoned number, 3, must never be reused)", res.NewVer)
	}
}

// (2) A stale-low head mark does not cause a reuse: forcing BOTH the mark and the
// cache stale-low makes the fast-path candidate collide with a taken number, and
// the collision-recovery scan advances past the true max.
//
// SABOTAGE: remove the collision-recovery scan (drop needScan on the errVerTaken
// retry) -> the retry re-proposes the same taken number every attempt, the
// absent-check refuses it, and the append exhausts its attempts and errors.
func TestAppendNumbering_StaleLowMarkDoesNotReuse(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("anst0001")
	vcInsert(t, repo, slug, "key:anst", "sha-anst-v1", 100)
	vcAppend(t, repo, slug, "sha-anst-v2", 100)
	vcAppend(t, repo, slug, "sha-anst-v3", 100)
	const trueMax = 3

	// Force the mark AND the cache stale-low, so the fast-path candidate (mark+1)
	// names an already-taken number and the recovery scan must run to recover.
	if err := repo.ForceHeadLatestVersionForTest(slug, 1); err != nil {
		t.Fatalf("force stale-low mark: %v", err)
	}
	staleCache, err := storage.VersionsDocValueForTest([]storage.VersionCacheRecordForTest{
		{VerNum: 1, ContentSHA: "sha-anst-v1", Size: 100},
	})
	if err != nil {
		t.Fatalf("encode stale-low cache: %v", err)
	}
	mustPutRaw(t, repo, storage.VersionsDocKeyForTest(slug), staleCache)

	res, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-anst-v4", 100, 0, vcNow)
	if err != nil {
		t.Fatalf("append under a stale-low mark: %v", err)
	}
	// The committed number is the true max + 1, NOT the stale-mark + 1 (== 2).
	if res.NewVer != trueMax+1 {
		t.Fatalf("append numbered %d; want %d (true max + 1, not the stale-mark + 1 == 2)", res.NewVer, trueMax+1)
	}
}

// (3) A cache-present append performs NO version-family scan; a cache-absent
// (pre-cache migration) append performs exactly one.
//
// SABOTAGE: make the append scan unconditionally (the pre-change behavior) -> the
// cache-present no-scan assertion fails.
func TestAppendNumbering_FastPathTakesNoVersionScan(t *testing.T) {
	repo := newShaleRepoForTest(t)

	var scans int
	repo.SetVersionScanHookForTest(func(domain.Slug) { scans++ })
	defer repo.SetVersionScanHookForTest(nil)

	// Cache-present append: 0 scans.
	present := domain.Slug("anfp0001")
	vcInsert(t, repo, present, "key:anfp", "sha-anfp-v1", 100)
	scans = 0
	vcAppend(t, repo, present, "sha-anfp-v2", 100)
	if scans != 0 {
		t.Fatalf("cache-present append scanned the version family %d time(s); want 0 (fast path)", scans)
	}

	// Cache-absent append: exactly 1 scan (migration rebuild).
	absent := domain.Slug("anfp0002")
	vcInsert(t, repo, absent, "key:anfp", "sha-anfp2-v1", 100)
	if err := repo.DeleteRawForTest(storage.VersionsDocKeyForTest(absent)); err != nil {
		t.Fatalf("drop cache: %v", err)
	}
	scans = 0
	vcAppend(t, repo, absent, "sha-anfp2-v2", 100)
	if scans != 1 {
		t.Fatalf("cache-absent append scanned the version family %d time(s); want exactly 1 (migration)", scans)
	}
}

// (4) Concurrent appends receive distinct version numbers: they serialize on the
// head CAS and the candidate absent-check, and losers re-scan and advance. Run
// under -race for the ordering coverage.
func TestAppendNumbering_ConcurrentAppendsDistinctNumbers(t *testing.T) {
	repo := newShaleRepoForTest(t)
	slug := domain.Slug("ancc0001")
	vcInsert(t, repo, slug, "key:ancc", "sha-ancc-v1", 100)

	const n = 8
	var wg sync.WaitGroup
	nums := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-ancc-c"+strconv.Itoa(i), 100, 0, vcNow)
			nums[i], errs[i] = res.NewVer, err
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent append %d: %v", i, errs[i])
		}
		if seen[nums[i]] {
			t.Fatalf("duplicate version number %d across concurrent appends: %v", nums[i], nums)
		}
		seen[nums[i]] = true
	}
}
