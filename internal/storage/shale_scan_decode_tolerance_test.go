//go:build slatedb

package storage_test

// Decode-tolerance policy tests for the shale background scans.
//
// docs/SPEC.md "Decode tolerance is per-scan-semantics" defines three
// policies, and these tests pin each one against a real ShaleRepo on the
// shared MinIO test bucket:
//
//   - Policy 1 (idempotent sweeps/reconcile): an undecodable record is
//     SKIPPED + logged and the pass CONTINUES, still processing the other
//     good records and returning success. The next tick retries the bad
//     row. TestShaleDecodeTolerance_ReconcileSkipsBadRecord pins this for
//     Reconcile across BOTH of its enumeration-index reprojections: a
//     poisoned pastes/ row (the paste index pass) and a poisoned sites/ row
//     (the re-homed site index pass).
//
//   - Policy 2 (blob-GC ref-set scan): an undecodable record must FAIL
//     CLOSED. ReferencedBlobSHAs aborts the pass and returns an error,
//     never a partial keep-set, so the sweep caller deletes NOTHING that
//     pass. Skipping here would UNDER-COUNT references and delete a live
//     blob (irreversible). TestShaleDecodeTolerance_BlobGCFailsClosed
//     drives the full Sweep.Once and asserts zero blobs removed, then
//     demonstrates the regression a skip-on-bad change would introduce.
//
//   - Policy 3 (user-facing reads): a read of a corrupt record still HARD
//     FAILS - the user sees an error, not a silent skip.
//     TestShaleDecodeTolerance_UserReadHardFails pins Get.
//
//	go test -tags slatedb -run TestShaleDecodeTolerance ./internal/storage
//
// All three skip cleanly unless MINIO_TEST_ENDPOINT is set.

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

// corruptJSON is bytes that pasteRow / versionRow / the reservation marker
// all fail to json.Unmarshal: a bare token that is not a JSON object.
var corruptJSON = []byte("this-is-not-json{")

// --- Policy 1: idempotent reconcile skips + continues -----------------------

// TestShaleDecodeTolerance_ReconcileSkipsBadRecord pins Policy 1: a single
// undecodable pastes/ row (and a single undecodable reservation marker) must
// NOT stall Reconcile. The pass skips the bad records, still heals the
// healthy owner's derived index, and returns success - the next tick retries
// the poisoned rows.
func TestShaleDecodeTolerance_ReconcileSkipsBadRecord(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:tolerant"

	// A healthy paste for the owner: its derived index entry will be the
	// thing the reconcile pass must still rebuild despite the poison.
	good := domain.Paste{
		Slug: domain.Slug("goodpst1"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-good", Size: 100,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), good, 0, now); err != nil {
		t.Fatalf("insert good paste: %v", err)
	}
	// Drain the deferred confirm before we manually desync the index below:
	// a not-yet-run confirm would re-write the index entry we delete.
	repo.WaitPendingConfirms()

	// Poison #1: an undecodable authoritative pastes/ row. A second slug so
	// its key sorts into the same aggregate scan Reconcile's paste-index
	// reprojection walks.
	badSlug := domain.Slug("badpst22")
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(badSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt paste row: %v", err)
	}

	// Poison #2: an undecodable authoritative sites/ row. Reconcile's re-homed
	// site-index reprojection (reconcileSiteIndexPass) scans sites/, so it must
	// skip this row rather than abort the whole pass.
	badSiteSlug := domain.Slug("badsite2")
	if err := repo.PutRawForTest(storage.SiteKeyForTest(badSiteSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt site row: %v", err)
	}

	// Desync the good paste's derived index so the pass has real work to do:
	// drop its identity_pastes entry. Reconcile must rebuild it - proving the
	// poison did not short-circuit the pass before it reached the healthy row.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, good.Slug.String())); err != nil {
		t.Fatalf("desync good index entry: %v", err)
	}
	if got := mustCount(t, repo, owner); got != 0 {
		t.Fatalf("post-desync count: got %d, want 0 (good index dropped)", got)
	}

	// Reconcile must SUCCEED despite both poisoned records.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile must skip+continue on bad records, not error: %v", err)
	}

	// The healthy owner's index was rebuilt: the good paste is back in the
	// list. This is the "the pass still processed the OTHER good records"
	// half of Policy 1.
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

	// The poison is untouched (skip+log leaves it for the next tick to retry) -
	// both the paste row and the site row.
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

// --- Policy 1 (Fix 3): reconciler heals an un-indexed undecodable row -------

// TestShaleDecodeTolerance_ReconcileHealsUnindexedUndecodableRow pins the
// fail-closed heal a naive skip+continue lacked. The quota scan reads THROUGH
// the enumeration index, so an undecodable authoritative row whose index entry
// was ALSO lost (a crash between the row write and the index write) is
// enumerated by NOTHING and silently drops from the owner's sum - a DURABLE
// under-count that lets the owner exceed the cap permanently, because the
// reconciler is the only thing that heals a missing index entry and a naive
// skip drops exactly this one.
//
// The fix: on an undecodable pastes/ (or sites/) row the reconciler derives the
// owner decode-independently from slug_owner/<slug> and projects a placeholder
// enumeration entry, so the next quota scan ENUMERATES the slug and re-reads the
// authoritative row - which then HARD-FAILS the scan (fail-closed reject), never
// a silent under-count. The residual (no slug_owner) is documented + pinned.
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
	// Models a paste whose authoritative row got corrupted AND whose enumeration
	// index entry was lost to a crash. slug_owner is a raw owner string
	// (insertAuthoritative writes it decode-independently), so the reconciler can
	// still derive the owner.
	owner := "key:heal"
	badSlug := domain.Slug("healbad1")
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(badSlug), corruptJSON); err != nil {
		t.Fatalf("seed corrupt paste row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacySlugOwnerKeyForTest(badSlug), []byte(owner)); err != nil {
		t.Fatalf("seed slug_owner: %v", err)
	}

	// Pre-reconcile: the corrupt row is enumerated by NOTHING (no index entry),
	// so the quota scan is BLIND to it - it returns 0 with NO error, a silent,
	// DURABLE under-count. This is the hole the fix closes.
	if got, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("pre-reconcile sum should not error (un-indexed row invisible): %v", err)
	} else if got != 0 {
		t.Fatalf("pre-reconcile sum: got %d, want 0 (un-indexed corrupt row invisible)", got)
	}

	// Reconcile derives the owner from slug_owner and projects a placeholder
	// enumeration entry so the row is no longer invisible - and does NOT error.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile must not error on the corrupt-but-ownable row: %v", err)
	}
	// The enumeration entry now exists.
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, badSlug.String())); err != nil {
		t.Fatalf("read healed paste index entry: %v", err)
	} else if len(raw) == 0 {
		t.Fatalf("reconcile must project an enumeration entry for the corrupt-but-ownable paste (fail-closed heal)")
	}
	// Post-reconcile: the scan now ENUMERATES badSlug, re-reads the corrupt
	// authoritative row, and HARD-FAILS (fail-closed) - rejecting the owner's
	// next upload rather than silently under-counting. This is the gate: an
	// undecodable authoritative row is a hard error, never absent.
	if got, err := repo.SumActiveBytesByOwner(owner, now); err == nil {
		t.Fatalf("post-reconcile sum must HARD-FAIL on the enumerated corrupt row (fail-closed); got %d, nil error", got)
	}

	// --- SITE: corrupt row + slug_owner, but NO identity_sites entry --------
	// The site deploy's pre-claim writes slug_owner on the transactional
	// shale-blob path (prod); seed it to model that.
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
	// without slug_owner the owner cannot be derived, so the row stays invisible.
	// Only reachable on the metadata-only path; prod always writes slug_owner.
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

// --- Policy 2 (THE IMPORTANT ONE): blob-GC ref scan fails closed ------------

// fakeBlobs is an in-memory SweepBlobs whose Remove records every deletion.
// The blob-GC fail-closed test asserts Remove is NEVER called when the
// ref-set scan errors.
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
// guard. A corrupt pastes/ row must make ReferencedBlobSHAs ABORT (return an
// error, never a partial keep-set), and the Sweep caller must therefore
// delete NOTHING this pass - including the live blob the corrupt row still
// references.
//
// The test also demonstrates the regression a skip-on-bad change would
// introduce: it confirms the corrupt row's referenced sha is NOT recoverable
// from the (aborted) scan, so if the scan had skipped instead of aborting,
// the keep-set would omit that sha and the sweep would delete a live blob.
func TestShaleDecodeTolerance_BlobGCFailsClosed(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Two pastes, each referencing a DISTINCT blob sha. Neither is expired
	// (ExpiresAt is well in the future) so the sweep deletes no pastes; the
	// only question is which blobs it considers referenced.
	keep := domain.Paste{
		Slug: domain.Slug("keeppst1"), Identity: domain.Identity("key:gc"),
		Kind: domain.KindHTML, ContentSHA: "sha-keep-good", Size: 100,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	poisoned := domain.Paste{
		Slug: domain.Slug("poispst2"), Identity: domain.Identity("key:gc"),
		Kind: domain.KindHTML, ContentSHA: "sha-keep-poisoned", Size: 100,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), keep, 0, now); err != nil {
		t.Fatalf("insert keep paste: %v", err)
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), poisoned, 0, now); err != nil {
		t.Fatalf("insert poisoned paste: %v", err)
	}

	// Both blobs exist on disk. The honest answer is "both still referenced".
	const keepSHA = "sha-keep-good"
	const poisonedSHA = "sha-keep-poisoned"

	// Make the poisoned paste's blob be referenced SOLELY by its pastes/ row:
	// drop the auto-created v1 version row (which carries the same sha). Now
	// the ONLY thing keeping poisonedSHA alive is the row we are about to
	// corrupt - so a skip-on-bad ref scan would omit poisonedSHA from the
	// keep-set and the sweep would delete a live blob. This is what makes the
	// fail-closed assertion below a real data-loss guard, not a formality.
	if err := repo.DeleteRawForTest(storage.LegacyVersionKeyForTest(poisoned.Slug, 1)); err != nil {
		t.Fatalf("drop poisoned v1 version row: %v", err)
	}

	// Baseline (before corruption): ReferencedBlobSHAs returns BOTH shas and
	// the sweep deletes nothing. Pins that the test fixture itself is sound.
	baseRefs, err := repo.ReferencedBlobSHAs()
	if err != nil {
		t.Fatalf("baseline ReferencedBlobSHAs: %v", err)
	}
	if !containsAllSHAs(baseRefs, keepSHA, poisonedSHA) {
		t.Fatalf("baseline refs %v must contain both %q and %q", baseRefs, keepSHA, poisonedSHA)
	}

	// Now corrupt the poisoned paste's authoritative row to undecodable bytes.
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(poisoned.Slug), corruptJSON); err != nil {
		t.Fatalf("corrupt poisoned paste row: %v", err)
	}

	// (a) The repo-level claim: the ref-set scan FAILS CLOSED. It returns an
	//     error and a nil/partial slice - NEVER a usable keep-set missing the
	//     poisoned sha. This is the regression guard: a skip-on-bad change
	//     would instead return (["sha-keep-good"], nil), omitting the poisoned
	//     sha, and the sweep below would delete its live blob.
	refs, err := repo.ReferencedBlobSHAs()
	if err == nil {
		t.Fatalf("ReferencedBlobSHAs must FAIL CLOSED on a corrupt row; got refs=%v, nil error (a skip-on-bad regression)", refs)
	}
	if shaContains(refs, keepSHA) || shaContains(refs, poisonedSHA) {
		t.Fatalf("failed-closed ReferencedBlobSHAs must not return a partial keep-set; got %v", refs)
	}

	// (b) The caller-level claim: the full Sweep.Once, given that erroring
	//     ref scan, deletes NOTHING. Both blobs survive - including the live
	//     blob the corrupt row still references.
	blobs := newFakeBlobs(keepSHA, poisonedSHA)
	silent := log.New(io.Discard, "", 0)
	sweep := &service.Sweep{
		Repo:   repo,
		Blobs:  blobs,
		Logger: silent,
		Now:    func() time.Time { return now },
	}
	pastesDeleted, blobsGCd, err := sweep.Once(now)
	if err == nil {
		t.Fatalf("Sweep.Once must surface the ref-scan error so the loop logs it; got nil")
	}
	if blobsGCd != 0 {
		t.Fatalf("Sweep.Once must GC zero blobs when the ref scan fails closed; got %d", blobsGCd)
	}
	if pastesDeleted != 0 {
		t.Fatalf("no paste was expired; want 0 pastes deleted, got %d", pastesDeleted)
	}
	if len(blobs.removed) != 0 {
		t.Fatalf("DATA LOSS: Sweep removed blobs %v on a fail-closed ref scan; it must delete NOTHING", blobs.removed)
	}
	// Both blobs are still present - the live blob the corrupt row references
	// was NOT deleted.
	if !blobs.has(keepSHA) || !blobs.has(poisonedSHA) {
		t.Fatalf("both blobs must survive a fail-closed pass; present=%v", blobs.snapshot())
	}
}

// --- Policy 3: user-facing reads still hard-fail ----------------------------

// TestShaleDecodeTolerance_UserReadHardFails pins Policy 3: a user read of a
// corrupt record surfaces an error rather than silently skipping. The
// tolerant background scans must NOT have leaked their skip behavior into the
// per-request read path.
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
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}

	// Corrupt the authoritative row to undecodable bytes.
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(p.Slug), corruptJSON); err != nil {
		t.Fatalf("corrupt paste row: %v", err)
	}

	// Get must return an error - the user sees corrupt data surfaced, not a
	// silent skip / empty result.
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
