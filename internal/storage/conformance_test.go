package storage_test

// Backend-agnostic conformance suite for the storage contract: the same
// assertions run against any implementation the newRepo factory supplies, so a
// backend swap can be proven behaviour-preserving. runConformanceWithSites is
// the entry point; each backend adds a factory file calling it.
//
// House rules:
//   - Touch a backend ONLY through conformanceRepo, never a backend-specific
//     helper, so the suite cannot pin a behavior one backend has and another
//     lacks.
//   - Create pastes through InsertWithQuotaCheck with caps=0 (the documented
//     "no quota enforcement" path), since not every backend has an Insert
//     helper.
//   - Slugs are fixed 8-char SlugAlphabet strings, so runs are deterministic.
//   - Surprising behavior is pinned as-is with a comment, not "fixed".

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// conformanceRepo is the union of the four service-layer interfaces a metadata
// backend must satisfy. Any type satisfying it can be driven through
// runConformanceWithSites, whether it is one struct or several sharing a db.
type conformanceRepo interface {
	service.PasteRepo
	service.PasteAdmin
	service.KeyGateRepo
}

// fixedNow is the reference clock for the suite. Truncated to the
// second so values round-trip through RFC3339 string encodings without
// sub-second drift on any backend.
var fixedNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

// conformCaps declares the points where a backend's observable behavior may
// differ by design. Anything NOT expressed here must be identical across
// backends, so each flag is an explicit, reviewed exception.
type conformCaps struct {
	// StrictQuotaUnderConcurrency is true for backends where the check and the
	// write are one atomic boundary, so a byte cap holds exactly under
	// concurrent writes. Gates the ROOM per-room cap concurrency test; all
	// three backends hold it. The per-IDENTITY quota is gated separately
	// because the two strictness properties diverge on shale.
	StrictQuotaUnderConcurrency bool

	// StrictIdentityQuotaUnderConcurrency is true for backends that enforce the
	// per-IDENTITY paste/site byte cap exactly under concurrent uploads from
	// one identity: slatedb (a
	// per-identity lockQuota stripe held across the sum + the write, valid
	// because SlateDB is single-writer, so only in-process goroutines race).
	// FALSE for shale, whose scan-and-compare is NOT atomic with the
	// authoritative write: two concurrent uploads from one identity can both
	// land, a bounded over-admit backstopped by the bucket quota (docs/SPEC.md
	// "Scan-derived quota"). Gates conformQuotaConcurrentCeiling (paste) +
	// conformSitePerOwnerCapConcurrentCeiling (site).
	StrictIdentityQuotaUnderConcurrency bool
}

// runConformanceWithSites runs the paste contract suite against the backend
// newRepo produces (a fresh, empty repo per call), plus the site suite when
// newSites is non-nil and the room suite when newRooms is non-nil. name labels
// the subtests so failures identify the backend; caps declares its by-design
// exceptions.
//
// The site/room factories MUST return repos sharing the backing store of the
// paste repo from the same call, or the cross-quota and cross-family subtests
// exercise nothing real.
func runConformanceWithSites(
	t *testing.T,
	name string,
	caps conformCaps,
	newRepo func(t *testing.T) conformanceRepo,
	newSites func(t *testing.T) (conformanceRepo, conformanceSiteRepo),
	newRooms func(t *testing.T) roomConformanceStores,
) {
	t.Helper()
	if newSites != nil {
		runSiteConformance(t, name, caps, newSites)
	}
	if newRooms != nil {
		runRoomConformance(t, name, caps, newRooms)
	}
	t.Run(name+"/InsertAndGet", func(t *testing.T) { conformInsertAndGet(t, newRepo(t)) })
	t.Run(name+"/QuotaConcurrentCeiling", func(t *testing.T) { conformQuotaConcurrentCeiling(t, newRepo(t), caps) })
	t.Run(name+"/GetNotFound", func(t *testing.T) { conformGetNotFound(t, newRepo(t)) })
	t.Run(name+"/DuplicateSlug", func(t *testing.T) { conformDuplicateSlug(t, newRepo(t)) })
	t.Run(name+"/QuotaRejectsOverCap", func(t *testing.T) { conformQuotaRejectsOverCap(t, newRepo(t)) })
	t.Run(name+"/QuotaCountsAllVersions", func(t *testing.T) { conformQuotaCountsAllVersions(t, newRepo(t)) })
	t.Run(name+"/QuotaFreedByDelete", func(t *testing.T) { conformQuotaFreedByDelete(t, newRepo(t)) })
	t.Run(name+"/QuotaFreedByDeleteVersion", func(t *testing.T) { conformQuotaFreedByDeleteVersion(t, newRepo(t)) })
	t.Run(name+"/QuotaPerIdentityIndependent", func(t *testing.T) { conformQuotaPerIdentityIndependent(t, newRepo(t)) })
	t.Run(name+"/AppendBumpsVersion", func(t *testing.T) { conformAppendBumpsVersion(t, newRepo(t)) })
	t.Run(name+"/PinUnpinRollsHead", func(t *testing.T) { conformPinUnpinRollsHead(t, newRepo(t)) })
	t.Run(name+"/AppendRespectsPin", func(t *testing.T) { conformAppendRespectsPin(t, newRepo(t)) })
	t.Run(name+"/PinOlderAfterMultipleAppends", func(t *testing.T) { conformPinOlderAfterMultipleAppends(t, newRepo(t)) })
	t.Run(name+"/DeleteVersionTombstones", func(t *testing.T) { conformDeleteVersionTombstones(t, newRepo(t)) })
	t.Run(name+"/VerNumNotReusedAfterTombstone", func(t *testing.T) { conformVerNumNotReused(t, newRepo(t)) })
	t.Run(name+"/RepoIsNotOwnerGated", func(t *testing.T) { conformRepoIsNotOwnerGated(t, newRepo(t)) })
	t.Run(name+"/OwnerStats", func(t *testing.T) { conformOwnerStats(t, newRepo(t)) })
	t.Run(name+"/SetName", func(t *testing.T) { conformSetName(t, newRepo(t)) })
	t.Run(name+"/KeyGateAdmitAndKnown", func(t *testing.T) { conformKeyGateAdmitAndKnown(t, newRepo(t)) })
	t.Run(name+"/KeyGateSubnetLimit", func(t *testing.T) { conformKeyGateSubnetLimit(t, newRepo(t)) })
	t.Run(name+"/KeyGateSubnetsIndependent", func(t *testing.T) { conformKeyGateSubnetsIndependent(t, newRepo(t)) })
	t.Run(name+"/KeyGateWindowAges", func(t *testing.T) { conformKeyGateWindowAges(t, newRepo(t)) })
	t.Run(name+"/KeyGateForgetsOutOfWindow", func(t *testing.T) { conformKeyGateForgetsOutOfWindow(t, newRepo(t)) })
}

// --- helpers ---------------------------------------------------------

// pasteOf builds a v1 paste with a content sha derived from the slug, stamped
// at fixedNow.
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
	}
}

// pendingConfirmsDrainer is implemented by a backend whose
// InsertWithQuotaCheck defers the derived-index confirm to a background
// goroutine. Draining it after an insert makes ListByOwner / CountByOwner /
// OwnerFirstSeen deterministic; backends that write the index synchronously do
// not implement it.
type pendingConfirmsDrainer interface{ WaitPendingConfirms() }

// drainConfirms blocks until any deferred confirm-insert the repo launched has
// run. No-op for backends that confirm synchronously.
func drainConfirms(r conformanceRepo) {
	if d, ok := r.(pendingConfirmsDrainer); ok {
		d.WaitPendingConfirms()
	}
}

// insert creates a paste with no caps (caps=0 = no quota enforcement) and
// fails the test on error.
func insert(t *testing.T, r conformanceRepo, p domain.Paste) {
	t.Helper()
	if err := r.InsertWithQuotaCheck(context.Background(), p, 0, fixedNow); err != nil {
		t.Fatalf("insert %q: %v", p.Slug, err)
	}
	drainConfirms(r)
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
}

func conformGetNotFound(t *testing.T, r conformanceRepo) {
	if _, err := r.Get("nopaste2"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get missing: got %v, want ErrNotFound", err)
	}
}

func conformDuplicateSlug(t *testing.T, r conformanceRepo) {
	p := pasteOf("dup23456", "key:alice", 10)
	insert(t, r, p)
	err := r.InsertWithQuotaCheck(context.Background(), p, 0, fixedNow)
	if err == nil {
		t.Fatalf("duplicate insert should error")
	}
	// Pins the contract service/upload.go's isSlugTaken depends on: every
	// backend surfaces the sentinel, bare or wrapped, so the retry fires.
	if !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("duplicate-slug error must be storage.ErrSlugTaken (errors.Is), got %v", err)
	}
}

// --- contract: quota -------------------------------------------------

// conformQuotaConcurrentCeiling pins the CEILING under concurrency: N
// goroutines insert distinct pastes for ONE identity against a per-owner cap
// admitting only K, and the bytes that land never exceed the cap however the
// inserts interleave. Gated on caps.StrictIdentityQuotaUnderConcurrency, since
// a scan-based per-identity check over-admits by a bounded amount by design.
func conformQuotaConcurrentCeiling(t *testing.T, r conformanceRepo, caps conformCaps) {
	const (
		body = 100
		k    = 3
		n    = 8
	)
	cap := int64(k * body) // admits exactly k pastes of `body`
	var landed int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slug := fmt.Sprintf("cc%06d", i)
			// A non-nil error (over-quota, or a transient backend lock) means
			// the paste did not land. Only the ceiling is asserted, so which
			// error it was does not matter.
			if err := r.InsertWithQuotaCheck(context.Background(), pasteOf(slug, "key:race", body), cap, fixedNow); err == nil {
				atomic.AddInt64(&landed, 1)
			}
		}(i)
	}
	wg.Wait()
	if !caps.StrictIdentityQuotaUnderConcurrency {
		// A scan-based per-identity check is not atomic with the authoritative
		// write, so a bounded same-owner over-admit can breach the ceiling.
		// Record it rather than asserting strictness.
		t.Logf("backend does not guarantee strict per-identity quota under concurrency (scan-based over-admit): %d pastes x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
		return
	}
	if landed*body > cap {
		t.Fatalf("quota ceiling breached under concurrency: %d pastes x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
	}
}

func conformQuotaRejectsOverCap(t *testing.T, r conformanceRepo) {
	const cap = 1000
	// First paste fits exactly at 600.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("q1234567", "key:q", 600), cap, fixedNow); err != nil {
		t.Fatalf("first insert (600 under 1000): %v", err)
	}
	// Second would be 600+500=1100 > 1000 → reject.
	err := r.InsertWithQuotaCheck(context.Background(), pasteOf("q2234567", "key:q", 500), cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap insert: got %v, want ErrOverUserQuota", err)
	}
}

func conformQuotaCountsAllVersions(t *testing.T, r conformanceRepo) {
	const cap = 1000
	// v1 = 600 fits.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("v1234567", "key:v", 600), cap, fixedNow); err != nil {
		t.Fatalf("v1 insert: %v", err)
	}
	// Append v2 = 600 → total 1200 > 1000 → reject. Pins "all non-deleted
	// versions count toward quota," not just the head.
	_, err := r.AppendVersionWithQuotaCheck(context.Background(), "v1234567", domain.KindHTML, "sha-v-v2", 600, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("append over cap: got %v, want ErrOverUserQuota", err)
	}
	// A smaller append that keeps the sum under cap succeeds.
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "v1234567", domain.KindHTML, "sha-v-v2b", 300, cap, fixedNow); err != nil {
		t.Fatalf("append within cap (600+300=900): %v", err)
	}
}

func conformQuotaFreedByDelete(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("d1234567", "key:d", 900), cap, fixedNow); err != nil {
		t.Fatalf("insert 900: %v", err)
	}
	// 900 used → 300 more would exceed.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("d2234567", "key:d", 300), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("pre-delete 300 should be over quota: %v", err)
	}
	// Delete the 900 paste, freeing all its bytes.
	if err := r.Delete("d1234567", "key:d", fixedNow); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("d2234567", "key:d", 300), cap, fixedNow); err != nil {
		t.Fatalf("post-delete 300 should fit: %v", err)
	}
}

func conformQuotaFreedByDeleteVersion(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("dv123456", "key:dv", 300), cap, fixedNow); err != nil {
		t.Fatalf("v1 insert 300: %v", err)
	}
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "dv123456", domain.KindHTML, "sha-dv-v2", 600, cap, fixedNow); err != nil {
		t.Fatalf("v2 append 600 (total 900): %v", err)
	}
	// v3 = 300 would be 1200 > 1000.
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "dv123456", domain.KindHTML, "sha-dv-v3", 300, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("v3 pre-tombstone should be over quota: %v", err)
	}
	// Tombstone v1 (300), freeing those bytes.
	if err := r.DeleteVersion("dv123456", 1); err != nil {
		t.Fatalf("delete version 1: %v", err)
	}
	// Now 600 used → v3 of 300 fits.
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "dv123456", domain.KindHTML, "sha-dv-v3b", 300, cap, fixedNow); err != nil {
		t.Fatalf("v3 post-tombstone should fit: %v", err)
	}
}

func conformQuotaPerIdentityIndependent(t *testing.T, r conformanceRepo) {
	const cap = 1000
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("ia123456", "key:alice", 900), cap, fixedNow); err != nil {
		t.Fatalf("alice insert: %v", err)
	}
	// Bob has his own budget: his 900 is unaffected by alice.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("ib123456", "key:bob", 900), cap, fixedNow); err != nil {
		t.Fatalf("bob insert: %v", err)
	}
	// Alice still can't add more.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("ia223456", "key:alice", 200), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("alice second should be over quota: %v", err)
	}
}

// --- contract: versions, pin, tombstones ----------------------------

func conformAppendBumpsVersion(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("ab123456", "key:a", 10))
	res, err := r.AppendVersionWithQuotaCheck(context.Background(), "ab123456", domain.KindMarkdown, "sha-ab-v2", 20, 0, fixedNow)
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
}

func conformPinUnpinRollsHead(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("pu123456", "key:p", 10))
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "pu123456", domain.KindHTML, "sha-pu-v2", 20, 0, fixedNow); err != nil {
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
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "ap123456", domain.KindHTML, "sha-ap-v2", 20, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	v1, _ := r.GetVersion("ap123456", 1)
	if err := r.SetPinnedVersion("ap123456", v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// Append v3 while pinned: WasPinned=true, head stays on v1.
	res, err := r.AppendVersionWithQuotaCheck(context.Background(), "ap123456", domain.KindHTML, "sha-ap-v3", 30, 0, fixedNow)
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

// conformPinOlderAfterMultipleAppends pins a NON-adjacent older version after
// the head has rolled forward to v3: the denormalized head ContentSHA/Size must
// roll back to the pinned version's bytes, since that head field is what the
// public serving path resolves. PinUnpinRollsHead only covers pinning one
// version back.
func conformPinOlderAfterMultipleAppends(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("po123456", "key:p", 10))
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "po123456", domain.KindHTML, "sha-po-v2", 20, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "po123456", domain.KindHTML, "sha-po-v3", 30, 0, fixedNow); err != nil {
		t.Fatalf("append v3: %v", err)
	}
	// Unpinned through all three appends: head followed to v3.
	if p, _ := r.Get("po123456"); p.ContentSHA != "sha-po-v3" || p.PinnedVersion != 0 {
		t.Fatalf("unpinned head should be v3, got sha=%q pinned=%d", p.ContentSHA, p.PinnedVersion)
	}
	v1, err := r.GetVersion("po123456", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	// The v1 row must still carry v1's own sha (not the head's).
	if v1.ContentSHA == "sha-po-v3" {
		t.Fatalf("v1 row carries the head's sha %q - version rows are leaking the head content", v1.ContentSHA)
	}
	// Pin v1 (two versions behind the head): head must roll back to v1's bytes.
	if err := r.SetPinnedVersion("po123456", v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	if p, _ := r.Get("po123456"); p.PinnedVersion != 1 || p.ContentSHA != v1.ContentSHA || p.Size != 10 {
		t.Fatalf("pin v1 must roll head to v1, got pinned=%d sha=%q size=%d (want sha=%q size=10)", p.PinnedVersion, p.ContentSHA, p.Size, v1.ContentSHA)
	}
	// Re-pin to v2 (the middle version): head must roll to v2's bytes.
	v2, err := r.GetVersion("po123456", 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if err := r.SetPinnedVersion("po123456", v2); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	if p, _ := r.Get("po123456"); p.PinnedVersion != 2 || p.ContentSHA != "sha-po-v2" || p.Size != 20 {
		t.Fatalf("pin v2 must roll head to v2, got pinned=%d sha=%q size=%d", p.PinnedVersion, p.ContentSHA, p.Size)
	}
	// Unpin: head rolls forward to the latest (v3).
	if err := r.Unpin("po123456"); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if p, _ := r.Get("po123456"); p.PinnedVersion != 0 || p.ContentSHA != "sha-po-v3" || p.Size != 30 {
		t.Fatalf("unpin must roll head to v3, got pinned=%d sha=%q size=%d", p.PinnedVersion, p.ContentSHA, p.Size)
	}
}

func conformDeleteVersionTombstones(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("dt123456", "key:d", 10))
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "dt123456", domain.KindHTML, "sha-dt-v2", 20, 0, fixedNow); err != nil {
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
	// Re-deleting an already-tombstoned version is a repo-level no-op: the
	// service layer, not the repo, maps repeats to ErrVersionAlreadyDeleted.
	if err := r.DeleteVersion("dt123456", 1); err != nil {
		t.Fatalf("re-delete tombstone should be a no-op at the repo level, got %v", err)
	}
}

func conformVerNumNotReused(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("vn123456", "key:v", 10))
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "vn123456", domain.KindHTML, "sha-vn-v2", 10, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	// Tombstone v2, then append again: the next number must be 3, since
	// MAX(ver_num) counts tombstones.
	if err := r.DeleteVersion("vn123456", 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	res, err := r.AppendVersionWithQuotaCheck(context.Background(), "vn123456", domain.KindHTML, "sha-vn-v3", 10, 0, fixedNow)
	if err != nil {
		t.Fatalf("append after tombstone: %v", err)
	}
	if res.NewVer != 3 {
		t.Fatalf("version number must not be reused after a tombstone, got %d (want 3)", res.NewVer)
	}
}

// --- contract: owner-gating is a service concern, NOT a repo concern -

func conformRepoIsNotOwnerGated(t *testing.T, r conformanceRepo) {
	// The repo operates on slugs regardless of owner: IDOR protection lives in
	// the service layer (Manage.requireOwner). A backend that added owner
	// checks here would change observable behavior.
	insert(t, r, pasteOf("og123456", "key:alice", 10))
	// Get by slug returns alice's paste with no owner argument at all.
	got, err := r.Get("og123456")
	if err != nil {
		t.Fatalf("repo Get is not owner-gated: %v", err)
	}
	if got.Identity.String() != "key:alice" {
		t.Fatalf("got identity %q, want key:alice", got.Identity)
	}
	// SetName + Delete name the paste's OWN identity + CreatedAt (a concurrency
	// guard against a delete+re-mint of the slug, NOT a session-owner check):
	// any caller holding the paste can name them, so IDOR gating still lives
	// only in the service layer.
	if err := r.SetName("og123456", "renamed", got.Identity, got.CreatedAt); err != nil {
		t.Fatalf("repo SetName is not owner-gated: %v", err)
	}
	if err := r.Delete("og123456", got.Identity, got.CreatedAt); err != nil {
		t.Fatalf("repo Delete is not owner-gated: %v", err)
	}
}

// --- contract: blob GC reference set --------------------------------

// --- contract: owner stats ------------------------------------------

func conformOwnerStats(t *testing.T, r conformanceRepo) {
	const owner = "key:stats"
	// Distinct UpdatedAt so the list order is observable rather than a tie.
	pA := pasteOf("st123456", owner, 100)
	insert(t, r, pA)
	pB := pasteOf("st223456", owner, 200)
	pB.UpdatedAt = fixedNow.Add(time.Hour)
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

	// ListByOwner: most recently updated first (pB before pA), owner-scoped.
	list, err := r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list by owner: got %d pastes, want 2", len(list))
	}
	if list[0].Slug != "st223456" || list[1].Slug != "st123456" {
		t.Fatalf("ListByOwner should be most-recently-updated first, got %q,%q", list[0].Slug, list[1].Slug)
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

	// CountByOwner counts only LIVE pastes and must AGREE with ListByOwner even
	// when a delete leaves a stale derived-index entry behind: a raw
	// len(index) count would over-report the orphan.
	if err := r.Delete("st223456", domain.Identity(owner), fixedNow); err != nil {
		t.Fatalf("delete for count-repair regression: %v", err)
	}
	n, err = r.CountByOwner(owner)
	if err != nil {
		t.Fatalf("count by owner after delete: %v", err)
	}
	list, err = r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner after delete: %v", err)
	}
	if n != 1 || len(list) != 1 {
		t.Fatalf("after deleting 1 of 2: CountByOwner=%d, ListByOwner=%d, want both 1 (count must ignore orphan index entries)", n, len(list))
	}
}

func conformSetName(t *testing.T, r conformanceRepo) {
	insert(t, r, pasteOf("sn123456", "key:s", 10))
	if err := r.SetName("sn123456", "my label", "key:s", fixedNow); err != nil {
		t.Fatalf("set name: %v", err)
	}
	p, _ := r.Get("sn123456")
	if p.Name != "my label" {
		t.Fatalf("set name: got %q, want %q", p.Name, "my label")
	}
	// Empty string clears the label.
	if err := r.SetName("sn123456", "", "key:s", fixedNow); err != nil {
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
	for i := range limit {
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
	for i := range limit {
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
	for i := range limit {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "11.0.0.0/24", old, limit, window); err != nil {
			t.Fatalf("old admit %d: %v", i, err)
		}
	}
	// The old rows are outside the window, so a fresh key is admitted even
	// though the subnet holds `limit` total rows.
	if _, err := r.AdmitNewKey("key:new", "11.0.0.0/24", fixedNow, limit, window); err != nil {
		t.Fatalf("aged-out rows should free the budget: %v", err)
	}
}

// conformKeyGateForgetsOutOfWindow pins the port-visible half of the lazy
// prune: a pair whose row has aged past the window is no longer "known", so a
// later session from it is a FRESH admission that consumes a slot. Backends
// drop the row at different moments (slatedb inside the admit transaction,
// shale as the subnet scan walks past it), so the contract is stated in terms
// of what a caller can observe rather than when the delete lands.
func conformKeyGateForgetsOutOfWindow(t *testing.T, r conformanceRepo) {
	const window = 24 * time.Hour
	old := fixedNow.Add(-48 * time.Hour)
	if _, err := r.AdmitNewKey("key:stale", "12.0.0.0/24", old, 20, window); err != nil {
		t.Fatalf("seed admit: %v", err)
	}
	// A fresh in-window row, which must NOT be forgotten.
	if _, err := r.AdmitNewKey("key:keep", "12.0.0.0/24", fixedNow, 20, window); err != nil {
		t.Fatalf("fresh admit: %v", err)
	}

	known, err := r.AdmitNewKey("key:stale", "12.0.0.0/24", fixedNow, 20, window)
	if err != nil {
		t.Fatalf("re-admit of the aged-out pair: %v", err)
	}
	if known {
		t.Fatalf("a pair whose row aged past the window must re-admit as FRESH (known=false), got known=true")
	}

	known, err = r.AdmitNewKey("key:keep", "12.0.0.0/24", fixedNow, 20, window)
	if err != nil {
		t.Fatalf("re-admit of the in-window pair: %v", err)
	}
	if !known {
		t.Fatalf("an in-window pair must stay known, got known=false")
	}
}

// --- small slice helpers --------------------------------------------
