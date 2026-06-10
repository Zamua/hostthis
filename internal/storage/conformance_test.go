package storage_test

// Backend-agnostic conformance suite for the storage contract.
//
// hostthis can run on more than one metadata backend (sqlite today,
// slatedb behind a build tag, a shale-backed cluster planned). Every
// backend implements the same four service-layer interfaces, and the
// service / SSH / HTTP layers must behave identically no matter which
// is wired. This suite pins the OBSERVABLE contract those interfaces
// expose so a backend swap can be proven behavior-preserving: the same
// assertions run against any implementation supplied through the
// `newRepo` factory.
//
// runConformance is the entry point. The sqlite backend calls it from
// conformance_sqlite_test.go (default build). The slatedb backend calls
// it from conformance_slatedb_test.go (build tag `slatedb`). A future
// shale-backed backend adds its own factory file and calls the same
// runConformance, which is the acceptance gate for "did the swap change
// any observable behavior."
//
// Design notes:
//   - The suite touches a backend ONLY through the conformanceRepo
//     interface (the union of the four service interfaces). It does not
//     reach for backend-specific helpers, so it cannot accidentally pin
//     a behavior one backend has and another lacks.
//   - Pastes are created through InsertWithQuotaCheck with caps=0 (no
//     cap) rather than an `Insert` convenience helper, because not every
//     backend exposes such a helper. caps=0 is the documented "no quota
//     enforcement" path and is exactly what the sqlite Insert helper does
//     internally.
//   - Slugs are fixed, valid 8-char strings from SlugAlphabet so runs are
//     deterministic (no random-slug collision flakiness).
//   - This pins CURRENT behavior. Where a current behavior is surprising
//     it is pinned as-is with a comment, not "fixed."

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// conformanceRepo is the union of the four service-layer interfaces a
// metadata backend must satisfy. Any type satisfying it can be driven
// through runConformance. Both the sqlite PasteRepo (+ KeyGateRepo, in
// the sqlite case these are two structs sharing one db) and the
// slatedb SlateRepo (one struct) implement this.
type conformanceRepo interface {
	service.PasteRepo
	service.PasteAdmin
	service.SweepRepo
	service.KeyGateRepo
}

// fixedNow is the reference clock for the suite. Truncated to the
// second so values round-trip through RFC3339 string encodings without
// sub-second drift on any backend.
var fixedNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

// runConformance runs the full contract suite against the backend the
// factory produces. `name` labels the subtests so failures identify the
// backend. The factory returns a fresh, empty repo per call.
func runConformance(t *testing.T, name string, newRepo func(t *testing.T) conformanceRepo) {
	t.Helper()
	t.Run(name+"/InsertAndGet", func(t *testing.T) { conformInsertAndGet(t, newRepo(t)) })
	t.Run(name+"/GetNotFound", func(t *testing.T) { conformGetNotFound(t, newRepo(t)) })
	t.Run(name+"/DuplicateSlug", func(t *testing.T) { conformDuplicateSlug(t, newRepo(t)) })
	t.Run(name+"/QuotaRejectsOverCap", func(t *testing.T) { conformQuotaRejectsOverCap(t, newRepo(t)) })
	t.Run(name+"/QuotaCountsAllVersions", func(t *testing.T) { conformQuotaCountsAllVersions(t, newRepo(t)) })
	t.Run(name+"/QuotaFreedByDelete", func(t *testing.T) { conformQuotaFreedByDelete(t, newRepo(t)) })
	t.Run(name+"/QuotaFreedByDeleteVersion", func(t *testing.T) { conformQuotaFreedByDeleteVersion(t, newRepo(t)) })
	t.Run(name+"/QuotaPerIdentityIndependent", func(t *testing.T) { conformQuotaPerIdentityIndependent(t, newRepo(t)) })
	t.Run(name+"/ServiceCapAcrossIdentities", func(t *testing.T) { conformServiceCapAcrossIdentities(t, newRepo(t)) })
	t.Run(name+"/AppendBumpsVersion", func(t *testing.T) { conformAppendBumpsVersion(t, newRepo(t)) })
	t.Run(name+"/PinUnpinRollsHead", func(t *testing.T) { conformPinUnpinRollsHead(t, newRepo(t)) })
	t.Run(name+"/AppendRespectsPin", func(t *testing.T) { conformAppendRespectsPin(t, newRepo(t)) })
	t.Run(name+"/DeleteVersionTombstones", func(t *testing.T) { conformDeleteVersionTombstones(t, newRepo(t)) })
	t.Run(name+"/VerNumNotReusedAfterTombstone", func(t *testing.T) { conformVerNumNotReused(t, newRepo(t)) })
	t.Run(name+"/RepoIsNotOwnerGated", func(t *testing.T) { conformRepoIsNotOwnerGated(t, newRepo(t)) })
	t.Run(name+"/ExpiredSlugs", func(t *testing.T) { conformExpiredSlugs(t, newRepo(t)) })
	t.Run(name+"/UnreferencedBlobSHAs", func(t *testing.T) { conformUnreferencedBlobSHAs(t, newRepo(t)) })
	t.Run(name+"/OwnerStats", func(t *testing.T) { conformOwnerStats(t, newRepo(t)) })
	t.Run(name+"/SetName", func(t *testing.T) { conformSetName(t, newRepo(t)) })
	t.Run(name+"/KeyGateAdmitAndKnown", func(t *testing.T) { conformKeyGateAdmitAndKnown(t, newRepo(t)) })
	t.Run(name+"/KeyGateSubnetLimit", func(t *testing.T) { conformKeyGateSubnetLimit(t, newRepo(t)) })
	t.Run(name+"/KeyGateSubnetsIndependent", func(t *testing.T) { conformKeyGateSubnetsIndependent(t, newRepo(t)) })
	t.Run(name+"/KeyGateWindowAges", func(t *testing.T) { conformKeyGateWindowAges(t, newRepo(t)) })
	t.Run(name+"/KeyGatePruneOld", func(t *testing.T) { conformKeyGatePruneOld(t, newRepo(t)) })
}

// --- helpers ---------------------------------------------------------

// pasteOf builds a Paste with the given slug/identity/size and a v1
// content sha derived from the slug, stamped at fixedNow with the
// standard retention window.
func pasteOf(slug, identity string, size int) domain.Paste {
	return domain.Paste{
		Slug:          domain.Slug(slug),
		Identity:      domain.Identity(identity),
		Kind:          domain.KindHTML,
		ContentSHA:    "sha-" + slug + "-v1",
		Size:          size,
		PinnedVersion: 0,
		CreatedAt:     fixedNow,
		UpdatedAt:     fixedNow,
		ExpiresAt:     fixedNow.Add(domain.RetentionWindow),
	}
}

// insert creates a paste with no caps (caps=0 → no quota enforcement).
// Fails the test on error.
func insert(t *testing.T, r conformanceRepo, p domain.Paste) {
	t.Helper()
	if err := r.InsertWithQuotaCheck(p, 0, 0, fixedNow); err != nil {
		t.Fatalf("insert %q: %v", p.Slug, err)
	}
}

// --- contract: insert / get -----------------------------------------

func conformInsertAndGet(t *testing.T, r conformanceRepo) {
	p := pasteOf("abc23456", "key:alice", 42)
	p.Name = "demo"
	insert(t, r, p)
	got, err := r.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Slug != p.Slug || got.Identity != p.Identity || got.Kind != p.Kind ||
		got.ContentSHA != p.ContentSHA || got.Size != p.Size || got.Name != p.Name {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, p)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("time round-trip mismatch: got %v/%v want %v/%v",
			got.CreatedAt, got.ExpiresAt, p.CreatedAt, p.ExpiresAt)
	}
}

func conformGetNotFound(t *testing.T, r conformanceRepo) {
	if _, err := r.Get("nopaste2"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get missing: got %v, want ErrNotFound", err)
	}
}

func conformDuplicateSlug(t *testing.T, r conformanceRepo) {
	p := pasteOf("dup23456", "key:alice", 10)
	insert(t, r, p)
	err := r.InsertWithQuotaCheck(p, 0, 0, fixedNow)
	if err == nil {
		t.Fatalf("duplicate insert should error")
	}
	// The upload service sniffs err.Error() for "slug" to know it should
	// retry with a fresh slug; the message must contain it. (Pins the
	// contract isSlugTaken in service/upload.go depends on.)
	if !containsSlug(err) {
		t.Fatalf("duplicate-slug error message must contain %q, got %q", "slug", err.Error())
	}
}

func containsSlug(err error) bool {
	return err != nil && strings.Contains(err.Error(), "slug")
}

// --- contract: quota -------------------------------------------------

func conformQuotaRejectsOverCap(t *testing.T, r conformanceRepo) {
	const cap = 1000
	// First paste fits exactly at 600.
	if err := r.InsertWithQuotaCheck(pasteOf("q1234567", "key:q", 600), 0, cap, fixedNow); err != nil {
		t.Fatalf("first insert (600 under 1000): %v", err)
	}
	// Second would be 600+500=1100 > 1000 → reject.
	err := r.InsertWithQuotaCheck(pasteOf("q2234567", "key:q", 500), 0, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap insert: got %v, want ErrOverUserQuota", err)
	}
}

func conformQuotaCountsAllVersions(t *testing.T, r conformanceRepo) {
	const cap = 1000
	// v1 = 600 fits.
	if err := r.InsertWithQuotaCheck(pasteOf("v1234567", "key:v", 600), 0, cap, fixedNow); err != nil {
		t.Fatalf("v1 insert: %v", err)
	}
	// Append v2 = 600 → total 1200 > 1000 → reject. Pins "all non-deleted
	// versions count toward quota," not just the head.
	_, err := r.AppendVersionWithQuotaCheck("v1234567", domain.KindHTML, "sha-v-v2", 600, 0, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("append over cap: got %v, want ErrOverUserQuota", err)
	}
	// A smaller append that keeps the sum under cap succeeds.
	if _, err := r.AppendVersionWithQuotaCheck("v1234567", domain.KindHTML, "sha-v-v2b", 300, 0, cap, fixedNow); err != nil {
		t.Fatalf("append within cap (600+300=900): %v", err)
	}
}

func conformQuotaFreedByDelete(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(pasteOf("d1234567", "key:d", 900), 0, cap, fixedNow); err != nil {
		t.Fatalf("insert 900: %v", err)
	}
	// 900 used → 300 more would exceed.
	if err := r.InsertWithQuotaCheck(pasteOf("d2234567", "key:d", 300), 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("pre-delete 300 should be over quota: %v", err)
	}
	// Delete the 900 paste, freeing all its bytes.
	if err := r.Delete("d1234567"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := r.InsertWithQuotaCheck(pasteOf("d2234567", "key:d", 300), 0, cap, fixedNow); err != nil {
		t.Fatalf("post-delete 300 should fit: %v", err)
	}
}

func conformQuotaFreedByDeleteVersion(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(pasteOf("dv123456", "key:dv", 300), 0, cap, fixedNow); err != nil {
		t.Fatalf("v1 insert 300: %v", err)
	}
	if _, err := r.AppendVersionWithQuotaCheck("dv123456", domain.KindHTML, "sha-dv-v2", 600, 0, cap, fixedNow); err != nil {
		t.Fatalf("v2 append 600 (total 900): %v", err)
	}
	// v3 = 300 would be 1200 > 1000.
	if _, err := r.AppendVersionWithQuotaCheck("dv123456", domain.KindHTML, "sha-dv-v3", 300, 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("v3 pre-tombstone should be over quota: %v", err)
	}
	// Tombstone v1 (300), freeing those bytes.
	if err := r.DeleteVersion("dv123456", 1); err != nil {
		t.Fatalf("delete version 1: %v", err)
	}
	// Now 600 used → v3 of 300 fits.
	if _, err := r.AppendVersionWithQuotaCheck("dv123456", domain.KindHTML, "sha-dv-v3b", 300, 0, cap, fixedNow); err != nil {
		t.Fatalf("v3 post-tombstone should fit: %v", err)
	}
}

func conformQuotaPerIdentityIndependent(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(pasteOf("ia123456", "key:alice", 900), 0, cap, fixedNow); err != nil {
		t.Fatalf("alice insert: %v", err)
	}
	// Bob has his own budget: his 900 is unaffected by alice.
	if err := r.InsertWithQuotaCheck(pasteOf("ib123456", "key:bob", 900), 0, cap, fixedNow); err != nil {
		t.Fatalf("bob insert: %v", err)
	}
	// Alice still can't add more.
	if err := r.InsertWithQuotaCheck(pasteOf("ia223456", "key:alice", 200), 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("alice second should be over quota: %v", err)
	}
}

func conformServiceCapAcrossIdentities(t *testing.T, r conformanceRepo) {
	const svcCap = 1000
	// Two different identities share the service-wide cap.
	if err := r.InsertWithQuotaCheck(pasteOf("sa123456", "key:alice", 700), svcCap, 0, fixedNow); err != nil {
		t.Fatalf("alice 700 under svc cap: %v", err)
	}
	// Bob's 400 would push service total to 1100 > 1000 → ErrServiceFull.
	if err := r.InsertWithQuotaCheck(pasteOf("sb123456", "key:bob", 400), svcCap, 0, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("bob over service cap: got %v, want ErrServiceFull", err)
	}
}

// --- contract: versions, pin, tombstones ----------------------------

func conformAppendBumpsVersion(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("ab123456", "key:a", 10))
	res, err := r.AppendVersionWithQuotaCheck("ab123456", domain.KindMarkdown, "sha-ab-v2", 20, 0, 0, fixedNow)
	if err != nil {
		t.Fatalf("append v2: %v", err)
	}
	if res.NewVer != 2 {
		t.Fatalf("append should produce ver 2, got %d", res.NewVer)
	}
	if res.WasPinned {
		t.Fatalf("unpinned paste should report WasPinned=false")
	}
	// Unpinned: head rolls forward to v2's bytes.
	p, err := r.Get("ab123456")
	if err != nil {
		t.Fatalf("get after append: %v", err)
	}
	if p.ContentSHA != "sha-ab-v2" || p.Size != 20 || p.Kind != domain.KindMarkdown {
		t.Fatalf("unpinned head should roll to v2, got sha=%q size=%d kind=%q", p.ContentSHA, p.Size, p.Kind)
	}
	// Append resets the retention clock from `now`.
	if !p.ExpiresAt.Equal(fixedNow.Add(domain.RetentionWindow)) {
		t.Fatalf("append should reset expiry to now+window, got %v", p.ExpiresAt)
	}
}

func conformPinUnpinRollsHead(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("pu123456", "key:p", 10))
	if _, err := r.AppendVersionWithQuotaCheck("pu123456", domain.KindHTML, "sha-pu-v2", 20, 0, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	// Pin to v1: head rolls back to v1's bytes.
	v1, err := r.GetVersion("pu123456", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := r.SetPinnedVersion("pu123456", v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	p, _ := r.Get("pu123456")
	if p.PinnedVersion != 1 || p.ContentSHA != v1.ContentSHA || p.Size != v1.Size {
		t.Fatalf("pin should roll head to v1, got pinned=%d sha=%q size=%d", p.PinnedVersion, p.ContentSHA, p.Size)
	}
	// Unpin: head rolls forward to latest (v2).
	if err := r.Unpin("pu123456"); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	p, _ = r.Get("pu123456")
	if p.PinnedVersion != 0 || p.ContentSHA != "sha-pu-v2" || p.Size != 20 {
		t.Fatalf("unpin should roll head to v2, got pinned=%d sha=%q size=%d", p.PinnedVersion, p.ContentSHA, p.Size)
	}
}

func conformAppendRespectsPin(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("ap123456", "key:a", 10))
	if _, err := r.AppendVersionWithQuotaCheck("ap123456", domain.KindHTML, "sha-ap-v2", 20, 0, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	v1, _ := r.GetVersion("ap123456", 1)
	if err := r.SetPinnedVersion("ap123456", v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// Append v3 while pinned: WasPinned=true, head stays on v1.
	res, err := r.AppendVersionWithQuotaCheck("ap123456", domain.KindHTML, "sha-ap-v3", 30, 0, 0, fixedNow)
	if err != nil {
		t.Fatalf("append v3 (pinned): %v", err)
	}
	if res.NewVer != 3 || !res.WasPinned {
		t.Fatalf("append-while-pinned: got NewVer=%d WasPinned=%v, want 3/true", res.NewVer, res.WasPinned)
	}
	p, _ := r.Get("ap123456")
	if p.ContentSHA != v1.ContentSHA || p.PinnedVersion != 1 {
		t.Fatalf("pinned head must stay on v1 after append, got sha=%q pinned=%d", p.ContentSHA, p.PinnedVersion)
	}
}

func conformDeleteVersionTombstones(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("dt123456", "key:d", 10))
	if _, err := r.AppendVersionWithQuotaCheck("dt123456", domain.KindHTML, "sha-dt-v2", 20, 0, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	if err := r.DeleteVersion("dt123456", 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	// The tombstoned row stays in ListVersions, flagged deleted.
	vers, err := r.ListVersions("dt123456")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var v1 *domain.Version
	for i := range vers {
		if vers[i].VerNum == 1 {
			v1 = &vers[i]
		}
	}
	if v1 == nil {
		t.Fatalf("v1 tombstone should still be listed, got %+v", vers)
	}
	if !v1.Deleted {
		t.Fatalf("v1 should be flagged deleted, got %+v", *v1)
	}
	// ListVersions is newest-first.
	if len(vers) >= 2 && vers[0].VerNum < vers[len(vers)-1].VerNum {
		t.Fatalf("ListVersions should be newest-first, got order %+v", vers)
	}
	// GetVersion returns the tombstone too.
	got, err := r.GetVersion("dt123456", 1)
	if err != nil {
		t.Fatalf("get tombstoned v1: %v", err)
	}
	if !got.Deleted {
		t.Fatalf("GetVersion should return tombstone flagged deleted, got %+v", got)
	}
	// Re-deleting an already-tombstoned version is a repo-level no-op
	// (the service layer maps repeats to ErrVersionAlreadyDeleted; the
	// repo itself does not error on the second flip). Pins CURRENT
	// behavior.
	if err := r.DeleteVersion("dt123456", 1); err != nil {
		t.Fatalf("re-delete tombstone should be a no-op at the repo level, got %v", err)
	}
}

func conformVerNumNotReused(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("vn123456", "key:v", 10))
	if _, err := r.AppendVersionWithQuotaCheck("vn123456", domain.KindHTML, "sha-vn-v2", 10, 0, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	// Tombstone v2, then append again: next number must be 3, not a
	// reused 2 (MAX(ver_num) counts tombstones).
	if err := r.DeleteVersion("vn123456", 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	res, err := r.AppendVersionWithQuotaCheck("vn123456", domain.KindHTML, "sha-vn-v3", 10, 0, 0, fixedNow)
	if err != nil {
		t.Fatalf("append after tombstone: %v", err)
	}
	if res.NewVer != 3 {
		t.Fatalf("version number must not be reused after a tombstone, got %d (want 3)", res.NewVer)
	}
}

// --- contract: owner-gating is a service concern, NOT a repo concern -

func conformRepoIsNotOwnerGated(t *testing.T, r conformanceRepo) {
	// The repo operates on slugs regardless of owner. IDOR protection
	// lives in the service layer (Manage.requireOwner), not here. A
	// backend that added owner checks would change observable behavior,
	// so pin that it does NOT.
	insert(t, r, pasteOf("og123456", "key:alice", 10))
	// Get by slug returns alice's paste with no owner argument at all.
	got, err := r.Get("og123456")
	if err != nil {
		t.Fatalf("repo Get is not owner-gated: %v", err)
	}
	if got.Identity.String() != "key:alice" {
		t.Fatalf("got identity %q, want key:alice", got.Identity)
	}
	// SetName + Delete take no owner and succeed for any caller.
	if err := r.SetName("og123456", "renamed"); err != nil {
		t.Fatalf("repo SetName is not owner-gated: %v", err)
	}
	if err := r.Delete("og123456"); err != nil {
		t.Fatalf("repo Delete is not owner-gated: %v", err)
	}
}

// --- contract: expiry ------------------------------------------------

func conformExpiredSlugs(t *testing.T, r conformanceRepo) {
	// One paste expiring soon, one far in the future.
	soon := pasteOf("ex123456", "key:e", 10)
	soon.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, soon)

	far := pasteOf("ex223456", "key:e", 10)
	far.ExpiresAt = fixedNow.Add(48 * time.Hour)
	insert(t, r, far)

	// At a time past `soon` but before `far`, only `soon` is expired.
	at := fixedNow.Add(2 * time.Hour)
	slugs, err := r.ExpiredSlugs(at)
	if err != nil {
		t.Fatalf("expired slugs: %v", err)
	}
	if !sliceHas(slugs, "ex123456") {
		t.Fatalf("ex123456 should be expired at %v, got %v", at, slugs)
	}
	if sliceHas(slugs, "ex223456") {
		t.Fatalf("ex223456 should NOT be expired at %v, got %v", at, slugs)
	}
	// Inclusive boundary: expires_at == now counts as expired.
	atBoundary := fixedNow.Add(time.Hour)
	slugs, err = r.ExpiredSlugs(atBoundary)
	if err != nil {
		t.Fatalf("expired slugs at boundary: %v", err)
	}
	if !sliceHas(slugs, "ex123456") {
		t.Fatalf("expires_at == now should be inclusive-expired, got %v", slugs)
	}
}

// --- contract: blob GC reference set --------------------------------

func conformUnreferencedBlobSHAs(t *testing.T, r conformanceRepo) {
	// Empty repo: no references. (The sweep's data-loss guard depends on
	// distinguishing "legitimately empty" from "buggy zero": here it's
	// legitimately empty, with no pastes.)
	refs, err := r.UnreferencedBlobSHAs()
	if err != nil {
		t.Fatalf("referenced shas (empty): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("empty repo should reference no shas, got %v", refs)
	}

	// One paste + one extra version → BOTH shas are referenced. The
	// method's name says "unreferenced" but it RETURNS the referenced set
	// (the sweep keeps everything in it). Pinned as the CURRENT contract
	// the sweep relies on. A backend that returned an empty set here while
	// pastes exist would trip the sweep's data-loss guard.
	insert(t, r, pasteOf("gc123456", "key:g", 10)) // v1 sha = sha-gc123456-v1
	if _, err := r.AppendVersionWithQuotaCheck("gc123456", domain.KindHTML, "sha-gc-v2", 20, 0, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	refs, err = r.UnreferencedBlobSHAs()
	if err != nil {
		t.Fatalf("referenced shas: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("with pastes present the referenced set MUST be non-empty (sweep guard); got 0")
	}
	if !sliceHas(refs, "sha-gc123456-v1") {
		t.Fatalf("v1 sha should be referenced, got %v", refs)
	}
	if !sliceHas(refs, "sha-gc-v2") {
		t.Fatalf("v2 sha should be referenced, got %v", refs)
	}

	// KNOWN BACKEND DIVERGENCE (pinned here so it is not silently lost,
	// NOT asserted because the two existing backends genuinely disagree):
	// whether a TOMBSTONED version's content sha stays in the referenced
	// set differs between backends.
	//   - sqlite: SELECT DISTINCT content_sha FROM versions has NO
	//     deleted filter, so a tombstoned version's sha IS still
	//     referenced (its blob is NOT GC'd while any tombstone points at
	//     it). Verified empirically.
	//   - slatedb (SlateRepo.UnreferencedBlobSHAs): filters !v.Deleted,
	//     so a tombstoned version's sha is NOT referenced and its blob
	//     becomes eligible for GC.
	// The behaviors differ in whether DeleteVersion eventually frees the
	// tombstoned blob bytes (sqlite keeps them until the whole paste
	// dies; slatedb GCs them on the next sweep). A future ShaleRepo MUST
	// pick one deliberately; this suite assertion is intentionally
	// limited to NON-deleted shas, which both backends agree are always
	// referenced. If the ShaleRepo adopts the slatedb rule, extend this
	// suite to assert it AND update the sqlite backend (or the SPEC) so
	// the two stop diverging.
	if err := r.DeleteVersion("gc123456", 2); err != nil {
		t.Fatalf("tombstone v2: %v", err)
	}
	refs, err = r.UnreferencedBlobSHAs()
	if err != nil {
		t.Fatalf("referenced shas after tombstone: %v", err)
	}
	// Shared invariant both backends hold: the still-live v1 head sha is
	// always referenced, tombstone or no tombstone.
	if !sliceHas(refs, "sha-gc123456-v1") {
		t.Fatalf("live v1 sha must stay referenced after tombstoning v2, got %v", refs)
	}
}

// --- contract: owner stats ------------------------------------------

func conformOwnerStats(t *testing.T, r conformanceRepo) {
	const owner = "key:stats"
	// Two pastes for the owner with different expiries to check ordering.
	pA := pasteOf("st123456", owner, 100)
	pA.ExpiresAt = fixedNow.Add(48 * time.Hour) // later
	insert(t, r, pA)
	pB := pasteOf("st223456", owner, 200)
	pB.ExpiresAt = fixedNow.Add(12 * time.Hour) // sooner
	insert(t, r, pB)
	// A different owner's paste must not leak into the stats.
	insert(t, r, pasteOf("st323456", "key:other", 500))

	// CountByOwner.
	n, err := r.CountByOwner(owner)
	if err != nil {
		t.Fatalf("count by owner: %v", err)
	}
	if n != 2 {
		t.Fatalf("count by owner: got %d, want 2", n)
	}

	// ListByOwner: soonest-to-expire first (pB before pA), owner-scoped.
	list, err := r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list by owner: got %d pastes, want 2", len(list))
	}
	if list[0].Slug != "st223456" || list[1].Slug != "st123456" {
		t.Fatalf("ListByOwner should be soonest-to-expire first, got %q,%q", list[0].Slug, list[1].Slug)
	}
	for _, p := range list {
		if p.Identity.String() != owner {
			t.Fatalf("ListByOwner leaked a non-owner paste: %+v", p)
		}
		if p.LatestVersion < 1 {
			t.Fatalf("ListByOwner should populate LatestVersion, got %d", p.LatestVersion)
		}
	}

	// SumActiveBytesByOwner = 100 + 200 = 300.
	used, err := r.SumActiveBytesByOwner(owner, fixedNow)
	if err != nil {
		t.Fatalf("sum active bytes: %v", err)
	}
	if used != 300 {
		t.Fatalf("sum active bytes: got %d, want 300", used)
	}

	// OwnerFirstSeen = earliest created_at (both at fixedNow here).
	first, err := r.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("owner first seen: %v", err)
	}
	if !first.Equal(fixedNow) {
		t.Fatalf("owner first seen: got %v, want %v", first, fixedNow)
	}
	// Unknown owner → zero time, no error.
	first, err = r.OwnerFirstSeen("key:nobody")
	if err != nil {
		t.Fatalf("owner first seen (unknown): %v", err)
	}
	if !first.IsZero() {
		t.Fatalf("unknown owner first seen should be zero time, got %v", first)
	}
}

func conformSetName(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("sn123456", "key:s", 10))
	if err := r.SetName("sn123456", "my label"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	p, _ := r.Get("sn123456")
	if p.Name != "my label" {
		t.Fatalf("set name: got %q, want %q", p.Name, "my label")
	}
	// Empty string clears the label.
	if err := r.SetName("sn123456", ""); err != nil {
		t.Fatalf("clear name: %v", err)
	}
	p, _ = r.Get("sn123456")
	if p.Name != "" {
		t.Fatalf("clear name: got %q, want empty", p.Name)
	}
}

// --- contract: key gate ---------------------------------------------

func conformKeyGateAdmitAndKnown(t *testing.T, r conformanceRepo) {
	const window = 24 * time.Hour
	known, err := r.AdmitNewKey("key:abc", "1.2.3.0/24", fixedNow, 20, window)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if known {
		t.Fatalf("first sight of (key, subnet) should report known=false")
	}
	// Same pair again → known, no accounting.
	known, err = r.AdmitNewKey("key:abc", "1.2.3.0/24", fixedNow.Add(time.Hour), 20, window)
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !known {
		t.Fatalf("returning pair should report known=true")
	}
}

func conformKeyGateSubnetLimit(t *testing.T, r conformanceRepo) {
	const (
		window = 24 * time.Hour
		limit  = 5
	)
	for i := 0; i < limit; i++ {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "9.9.9.0/24", fixedNow, limit, window); err != nil {
			t.Fatalf("admit %d under limit: %v", i, err)
		}
	}
	// The (limit+1)th fresh key from this subnet is refused.
	if _, err := r.AdmitNewKey("key:z", "9.9.9.0/24", fixedNow, limit, window); !errors.Is(err, storage.ErrTooManyNewKeys) {
		t.Fatalf("over-limit admit: got %v, want ErrTooManyNewKeys", err)
	}
}

func conformKeyGateSubnetsIndependent(t *testing.T, r conformanceRepo) {
	const (
		window = 24 * time.Hour
		limit  = 3
	)
	for i := 0; i < limit; i++ {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "10.0.0.0/24", fixedNow, limit, window); err != nil {
			t.Fatalf("fill subnet A %d: %v", i, err)
		}
	}
	// A different subnet has its own untouched budget.
	if _, err := r.AdmitNewKey("key:fresh", "10.0.1.0/24", fixedNow, limit, window); err != nil {
		t.Fatalf("different subnet should have its own budget: %v", err)
	}
}

func conformKeyGateWindowAges(t *testing.T, r conformanceRepo) {
	const (
		window = 24 * time.Hour
		limit  = 2
	)
	old := fixedNow.Add(-48 * time.Hour) // outside the 24h window
	for i := 0; i < limit; i++ {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "11.0.0.0/24", old, limit, window); err != nil {
			t.Fatalf("old admit %d: %v", i, err)
		}
	}
	// At `now`, the old rows are outside the window so they don't count:
	// a fresh key is admitted even though the subnet has `limit` total
	// rows.
	if _, err := r.AdmitNewKey("key:new", "11.0.0.0/24", fixedNow, limit, window); err != nil {
		t.Fatalf("aged-out rows should free the budget: %v", err)
	}
}

func conformKeyGatePruneOld(t *testing.T, r conformanceRepo) {
	const window = 24 * time.Hour
	old := fixedNow.Add(-48 * time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "12.0.0.0/24", old, 20, window); err != nil {
			t.Fatalf("old admit %d: %v", i, err)
		}
	}
	// A fresh in-window row that must NOT be pruned.
	if _, err := r.AdmitNewKey("key:keep", "12.0.0.0/24", fixedNow, 20, window); err != nil {
		t.Fatalf("fresh admit: %v", err)
	}
	// Prune everything older than the window cutoff (now - window).
	cutoff := fixedNow.Add(-window)
	n, err := r.DeleteFirstSeenOlderThan(cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 3 {
		t.Fatalf("prune should remove the 3 old rows, got %d", n)
	}
	// The fresh row survives: it still reports known.
	known, err := r.AdmitNewKey("key:keep", "12.0.0.0/24", fixedNow, 20, window)
	if err != nil {
		t.Fatalf("post-prune admit of kept key: %v", err)
	}
	if !known {
		t.Fatalf("the in-window row should survive prune (known=true), got known=false")
	}
}

// --- small slice helpers --------------------------------------------

// sliceHas reports whether want is in ss. (Named to avoid colliding
// with the s3 blob test's contains/containsString helpers, which live
// in the same test package with different signatures.)
func sliceHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
