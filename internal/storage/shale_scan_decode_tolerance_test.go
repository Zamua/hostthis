//go:build slatedb

package storage_test

// Decode-tolerance policy tests for the shale background scans, pinned against
// a real ShaleRepo on the shared MinIO test bucket. docs/SPEC.md "Decode
// tolerance is per-scan-semantics" defines the three policies:
//
//   - Policy 1 (idempotent sweeps/reconcile): an undecodable record is SKIPPED
//     and logged, the pass continues over the good records and returns success,
//     and the next tick retries the bad row.
//
//   - Policy 2 (blob-GC ref-set scan): an undecodable record must FAIL CLOSED.
//     ReferencedBlobSHAs returns an error, never a partial keep-set, so the
//     sweep deletes NOTHING that pass. Skipping would under-count references
//     and delete a live blob, which is irreversible.
//
//   - Policy 3 (user-facing reads): a read of a corrupt record HARD FAILS. The
//     user sees an error, not a silent skip.
//
//	go test -tags slatedb -run TestShaleDecodeTolerance ./internal/storage
//
// All skip cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"io"
	"log"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// corruptJSON fails json.Unmarshal into pasteRow, versionRow and the
// reservation marker alike: a bare token that is not a JSON object.
var corruptJSON = []byte("this-is-not-json{")

// --- Policy 1: idempotent reconcile skips + continues -----------------------

// TestShaleDecodeTolerance_ReconcileSkipsBadRecord pins Policy 1 across BOTH of
// Reconcile's enumeration-index reprojections (a poisoned pastes/ row and a
// poisoned sites/ row): the pass skips them, still heals the healthy owner's
// derived index, and returns success.
func TestShaleDecodeTolerance_ReconcileSkipsBadRecord(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:tolerant"

	// A healthy paste whose derived index entry the pass must still rebuild
	// despite the poison.
	good := domain.Paste{
		Slug: domain.Slug("goodpst1"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-good", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), good, 0, now); err != nil {
		t.Fatalf("insert good paste: %v", err)
	}
	// Drain the deferred confirm before desyncing the index below: a
	// not-yet-run confirm would re-write the entry the desync deletes.
	repo.WaitPendingConfirms()

	// Poison #1: an undecodable authoritative pastes/ row, under a slug whose
	// key sorts into the same aggregate scan the paste-index reprojection
	// walks.
	badSlug := domain.Slug("badpst22")
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(badSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt paste row: %v", err)
	}

	// Poison #2: an undecodable authoritative sites/ row, which
	// reconcileSiteIndexPass must skip rather than abort the whole pass on.
	badSiteSlug := domain.Slug("badsite2")
	if err := repo.PutRawForTest(storage.SiteKeyForTest(badSiteSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt site row: %v", err)
	}

	// Drop the good paste's identity_pastes entry so the pass has real work:
	// rebuilding it proves the poison did not short-circuit the pass before
	// it reached the healthy row.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, good.Slug.String())); err != nil {
		t.Fatalf("desync good index entry: %v", err)
	}
	if got := mustCount(t, repo, owner); got != 0 {
		t.Fatalf("post-desync count: got %d, want 0 (good index dropped)", got)
	}

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile must skip+continue on bad records, not error: %v", err)
	}

	// The "still processed the OTHER good records" half of Policy 1.
	if got := mustCount(t, repo, owner); got != 1 {
		t.Fatalf("post-reconcile count: got %d, want 1 (good index rebuilt despite poison)", got)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(list) != 1 || list[0].Slug != good.Slug {
		t.Fatalf("post-reconcile list: got %+v, want just %q", list, good.Slug)
	}

	// Both poisoned rows survive the pass, left for the next tick to retry.
	raw, err := repo.GetRawForTest(storage.LegacyPasteKeyForTest(badSlug))
	if err != nil {
		t.Fatalf("read poisoned paste row: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("reconcile must not delete the poisoned paste row, it skips it; got empty")
	}
	rawSite, err := repo.GetRawForTest(storage.SiteKeyForTest(badSiteSlug))
	if err != nil {
		t.Fatalf("read poisoned site row: %v", err)
	}
	if len(rawSite) == 0 {
		t.Fatalf("reconcile must not delete the poisoned site row, it skips it; got empty")
	}
}

// --- Policy 1: reconciler heals an un-indexed undecodable row ---------------

// TestShaleDecodeTolerance_ReconcileHealsUnindexedUndecodableRow pins the
// fail-closed heal that plain skip+continue lacks. The quota scan sums
// enumeration entries, so an undecodable authoritative row whose index entry
// was ALSO lost (a crash between the row write and the index write) is
// enumerated by NOTHING and silently drops from the owner's sum: a DURABLE
// under-count letting the owner exceed the cap permanently, since the
// reconciler is the only thing that heals a missing index entry.
//
// So on an undecodable pastes/ or sites/ row the reconciler derives the owner
// decode-independently from slug_owner/<slug> and projects a FAIL-CLOSED
// PLACEHOLDER enumeration entry, making the owner's next quota scan hard-fail
// rather than under-count. The residual (no slug_owner) is pinned below.
//
//	go test -tags slatedb -run TestShaleDecodeTolerance_ReconcileHeals ./internal/storage
func TestShaleDecodeTolerance_ReconcileHealsUnindexedUndecodableRow(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// --- PASTE: corrupt row + slug_owner, but NO identity_pastes entry ------
	// A paste whose authoritative row is corrupt AND whose enumeration entry
	// was lost to a crash. slug_owner holds a raw owner string
	// (insertAuthoritative writes it decode-independently), so the owner is
	// still derivable.
	owner := "key:heal"
	badSlug := domain.Slug("healbad1")
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(badSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt paste row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacySlugOwnerKeyForTest(badSlug), []byte(owner)); err != nil {
		t.Fatalf("seed slug_owner: %v", err)
	}

	// Nothing enumerates the corrupt row yet, so the quota scan is BLIND to
	// it: 0 with no error, the silent durable under-count.
	if got, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("pre-reconcile sum should not error (un-indexed row invisible): %v", err)
	} else if got != 0 {
		t.Fatalf("pre-reconcile sum: got %d, want 0 (un-indexed corrupt row invisible)", got)
	}

	// Reconcile derives the owner from slug_owner and projects a placeholder
	// entry, without erroring.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile must not error on the corrupt-but-ownable row: %v", err)
	}
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, badSlug.String())); err != nil {
		t.Fatalf("read healed paste index entry: %v", err)
	} else if len(raw) == 0 {
		t.Fatalf("reconcile must project an enumeration entry for the corrupt-but-ownable paste (fail-closed heal)")
	}
	// The scan now sees the placeholder and HARD-FAILS, rejecting the owner's
	// next upload: an undecodable authoritative row is an error, never absent.
	if got, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatalf("post-reconcile sum must HARD-FAIL on the enumerated corrupt row (fail-closed); got %d, nil error", got)
	}

	// --- SITE: corrupt row + slug_owner, but NO identity_sites entry --------
	// The site deploy's pre-claim writes slug_owner on the transactional
	// shale-blob path; seed it to model that.
	siteOwner := "key:healsite"
	badSite := domain.Slug("healbsit")
	if err := repo.PutRawForTest(storage.SiteKeyForTest(badSite), corruptJSON); err != nil {
		t.Fatalf("seed corrupt site row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacySlugOwnerKeyForTest(badSite), []byte(siteOwner)); err != nil {
		t.Fatalf("seed site slug_owner: %v", err)
	}
	if got, err := repo.SumActiveSiteBytesByOwner(siteOwner, now); err != nil {
		t.Fatalf("pre-reconcile site sum should not error: %v", err)
	} else if got != 0 {
		t.Fatalf("pre-reconcile site sum: got %d, want 0 (un-indexed corrupt site invisible)", got)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (site) must not error: %v", err)
	}
	if raw, err := repo.GetRawForTest(storage.IdentitySiteKeyForTest(siteOwner, badSite.String())); err != nil {
		t.Fatalf("read healed site index entry: %v", err)
	} else if len(raw) == 0 {
		t.Fatalf("reconcile must project an enumeration entry for the corrupt-but-ownable site (fail-closed heal)")
	}
	if got, err := repo.SumActiveSiteBytesByOwner(siteOwner, now); err == nil {
		t.Fatalf("post-reconcile site sum must HARD-FAIL on the enumerated corrupt site (fail-closed); got %d, nil error", got)
	}

	// --- RESIDUAL: corrupt row with NO slug_owner stays un-enumerated -------
	// Documented residual (docs/SPEC.md "Decode tolerance of the quota scan"):
	// with no slug_owner the owner cannot be derived, so the row stays
	// invisible. Only reachable on the metadata-only path.
	orphanOwner := "key:healorphan"
	orphan := domain.Slug("healorph")
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(orphan), corruptJSON); err != nil {
		t.Fatalf("seed orphan corrupt row: %v", err)
	}
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile (orphan) must not error: %v", err)
	}
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(orphanOwner, orphan.String())); err != nil {
		t.Fatalf("read orphan index entry: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("no slug_owner: reconcile cannot derive the owner, so it must NOT project an entry; got a non-empty entry")
	}
	if got, err := repo.SumActiveBytesByOwner(orphanOwner, now); err != nil {
		t.Fatalf("orphan sum should not error (documented residual: invisible): %v", err)
	} else if got != 0 {
		t.Fatalf("orphan sum: got %d, want 0 (documented residual - no slug_owner to derive)", got)
	}
}

// --- Policy 2: blob-GC ref scan fails closed --------------------------------

// fakeBlobs is an in-memory SweepBlobs whose Remove records every deletion, so
// a test can assert Remove is NEVER called.
type fakeBlobs struct {
	mu      sync.Mutex
	present map[string]struct{}
	removed []string
}

func newFakeBlobs(shas ...string) *fakeBlobs {
	b := &fakeBlobs{present: make(map[string]struct{})}
	for _, s := range shas {
		b.present[s] = struct{}{}
	}
	return b
}

func (b *fakeBlobs) WalkBlobs(fn func(sha string) error) error {
	b.mu.Lock()
	shas := make([]string, 0, len(b.present))
	for s := range b.present {
		shas = append(shas, s)
	}
	b.mu.Unlock()
	for _, s := range shas {
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func (b *fakeBlobs) Remove(sha string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.present, sha)
	b.removed = append(b.removed, sha)
	return nil
}

// TestShaleDecodeTolerance_BlobGCFailsClosed pins Policy 2, the data-loss
// guard: a corrupt pastes/ row makes ReferencedBlobSHAs ABORT (an error, never
// a partial keep-set), so the Sweep caller deletes NOTHING that pass, including
// the live blob the corrupt row still references.
//
// It also shows what a skip-on-bad change would cost: the corrupt row's sha is
// not recoverable from the aborted scan, so a skipping scan would omit it from
// the keep-set and the sweep would delete a live blob.
func TestShaleDecodeTolerance_BlobGCFailsClosed(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Two pastes referencing DISTINCT blob shas, neither expired, so the
	// sweep deletes no pastes and the only question is which blobs it
	// considers referenced.
	keep := domain.Paste{
		Slug: domain.Slug("keeppst1"), Identity: domain.Identity("key:gc"),
		Kind: domain.KindHTML, ContentSHA: "sha-keep-good", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	poisoned := domain.Paste{
		Slug: domain.Slug("poispst2"), Identity: domain.Identity("key:gc"),
		Kind: domain.KindHTML, ContentSHA: "sha-keep-poisoned", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), keep, 0, now); err != nil {
		t.Fatalf("insert keep paste: %v", err)
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), poisoned, 0, now); err != nil {
		t.Fatalf("insert poisoned paste: %v", err)
	}

	// Both blobs exist on disk; the honest answer is "both still referenced".
	const keepSHA = "sha-keep-good"
	const poisonedSHA = "sha-keep-poisoned"

	// Dropping the auto-created v1 version row (same sha) leaves the pastes/
	// row about to be corrupted as the ONLY reference to poisonedSHA. That is
	// what makes the fail-closed assertion a real data-loss guard: a
	// skip-on-bad scan would omit poisonedSHA and the sweep would delete a
	// live blob.
	if err := repo.DeleteRawForTest(storage.LegacyVersionKeyForTest(poisoned.Slug, 1)); err != nil {
		t.Fatalf("drop poisoned v1 version row: %v", err)
	}

	// Baseline: both shas referenced, proving the fixture itself is sound.
	baseRefs, err := repo.ReferencedBlobSHAs()
	if err != nil {
		t.Fatalf("baseline ReferencedBlobSHAs: %v", err)
	}
	if !containsAllSHAs(baseRefs, keepSHA, poisonedSHA) {
		t.Fatalf("baseline refs %v must contain both %q and %q", baseRefs, keepSHA, poisonedSHA)
	}

	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(poisoned.Slug), corruptJSON); err != nil {
		t.Fatalf("corrupt poisoned paste row: %v", err)
	}

	// (a) Repo level: the ref-set scan FAILS CLOSED, never returning a usable
	//     keep-set that omits the poisoned sha.
	refs, err := repo.ReferencedBlobSHAs()
	if err == nil {
		t.Fatalf("ReferencedBlobSHAs must FAIL CLOSED on a corrupt row; got refs=%v, nil error (a skip-on-bad regression)", refs)
	}
	if shaContains(refs, keepSHA) || shaContains(refs, poisonedSHA) {
		t.Fatalf("failed-closed ReferencedBlobSHAs must not return a partial keep-set; got %v", refs)
	}

	// (b) Caller level: Sweep.Once, given that erroring ref scan, deletes
	//     NOTHING, so the live blob the corrupt row references survives.
	blobs := newFakeBlobs(keepSHA, poisonedSHA)
	silent := log.New(io.Discard, "", 0)
	sweep := &service.Sweep{
		Repo:   repo,
		Blobs:  blobs,
		Logger: silent,
		Now:    func() time.Time { return now },
	}
	blobsGCd, err := sweep.Once(now)
	if err == nil {
		t.Fatalf("Sweep.Once must surface the ref-scan error so the loop logs it; got nil")
	}
	if blobsGCd != 0 {
		t.Fatalf("Sweep.Once must GC zero blobs when the ref scan fails closed; got %d", blobsGCd)
	}
	if len(blobs.removed) != 0 {
		t.Fatalf("DATA LOSS: Sweep removed blobs %v on a fail-closed ref scan; it must delete NOTHING", blobs.removed)
	}
	if !blobs.has(keepSHA) || !blobs.has(poisonedSHA) {
		t.Fatalf("both blobs must survive a fail-closed pass; present=%v", blobs.snapshot())
	}
}

// --- Policy 3: user-facing reads still hard-fail ----------------------------

// TestShaleDecodeTolerance_UserReadHardFails pins Policy 3: a user read of a
// corrupt record surfaces an error, so the tolerant background scans cannot
// have leaked their skip behavior into the per-request read path.
func TestShaleDecodeTolerance_UserReadHardFails(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := domain.Paste{
		Slug: domain.Slug("readpst1"), Identity: domain.Identity("key:read"),
		Kind: domain.KindHTML, ContentSHA: "sha-read", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}

	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(p.Slug), corruptJSON); err != nil {
		t.Fatalf("corrupt paste row: %v", err)
	}

	got, err := repo.Get(p.Slug)
	if err == nil {
		t.Fatalf("Get of a corrupt row must hard-fail; got paste=%+v, nil error", got)
	}
}

// --- helpers ---------------------------------------------------------------

func (b *fakeBlobs) has(sha string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.present[sha]
	return ok
}

func (b *fakeBlobs) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.present))
	for s := range b.present {
		out = append(out, s)
	}
	return out
}

func shaContains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func containsAllSHAs(haystack []string, needles ...string) bool {
	for _, n := range needles {
		if !shaContains(haystack, n) {
			return false
		}
	}
	return true
}
