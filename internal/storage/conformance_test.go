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
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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

// conformCaps captures the points where a backend's observable behavior
// is allowed to differ by design. Everything NOT expressed here must be
// identical across backends; these flags are the explicit, reviewed
// exceptions so the suite pins each backend's intended behavior rather
// than silently accepting either.
type conformCaps struct {
	// ExpiryFreesQuotaAtReadTime is true for backends that exclude an
	// expired-but-unswept paste's/site's bytes from the owner sum at READ
	// time (sqlite, slatedb, and shale: their sum query / scan filters
	// expires_at > now), so an owner reclaims that quota the instant the
	// record expires. All three shipping backends now set it true: shale's
	// owner sum is a scan over the authoritative rows with a read-time expiry
	// filter (docs/SPEC.md "Scan-derived quota"), not the old
	// monotonic identity_bytes counter that only shed at sweep time.
	ExpiryFreesQuotaAtReadTime bool

	// StrictQuotaUnderConcurrency is true for backends that enforce a byte
	// cap exactly under concurrent writes where the check + the write are one
	// atomic boundary. It gates the ROOM per-room cap concurrency test
	// (conformRoomPerRoomCapConcurrentCeiling): sqlite (serializable tx),
	// slatedb (the per-room lockQuota stripe), and shale (the single-shard
	// CAS on the value key with the per-app counter in the read-set) all hold
	// it. The per-IDENTITY paste/site quota is gated separately by
	// StrictIdentityQuotaUnderConcurrency, because those two strictness
	// properties diverge on shale.
	StrictQuotaUnderConcurrency bool

	// StrictIdentityQuotaUnderConcurrency is true for backends that enforce
	// the per-IDENTITY paste/site byte cap exactly under concurrent uploads
	// from the same identity: sqlite (its serializable insert transaction)
	// and slatedb (a per-identity lockQuota stripe held across the sum + the
	// write, valid because SlateDB is single-writer so only in-process
	// goroutines can race). It is FALSE for shale: the per-identity quota is
	// a scan-and-compare that is NOT atomic with the authoritative write, so
	// two concurrent uploads from one identity can both pass the check and
	// both land - a bounded over-admit that is acceptable (one key is one
	// person; the object-store bucket quota backstops the durable total).
	// See docs/SPEC.md "Scan-derived quota". Gates
	// conformQuotaConcurrentCeiling (paste) +
	// conformSitePerOwnerCapConcurrentCeiling (site).
	StrictIdentityQuotaUnderConcurrency bool
}

// runConformance runs the full contract suite against the backend the
// factory produces. `name` labels the subtests so failures identify the
// backend. The factory returns a fresh, empty repo per call. `caps`
// declares the backend's by-design behavior exceptions.
//
// It is a thin wrapper over runConformanceWithSites with no site repo, kept
// so a backend with no static-site impl (or a caller that only wants the
// paste contract) needs no extra argument.
func runConformance(t *testing.T, name string, caps conformCaps, newRepo func(t *testing.T) conformanceRepo) {
	t.Helper()
	runConformanceWithSites(t, name, caps, newRepo, nil, nil)
}

// runConformanceWithSites runs the paste contract suite (via newRepo) and,
// when newSites is non-nil, the site contract suite too, and when newRooms is
// non-nil, the room contract suite too. The site + room factories produce
// fresh repos that MUST share the same backing store as the paste repo from
// the same factory call, so the cross-quota and cross-family / cross-kind
// subtests exercise the real interaction.
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
	t.Run(name+"/ExpiryQuotaSemantics", func(t *testing.T) { conformExpiryQuotaSemantics(t, newRepo(t), caps) })
	t.Run(name+"/GetNotFound", func(t *testing.T) { conformGetNotFound(t, newRepo(t)) })
	t.Run(name+"/DuplicateSlug", func(t *testing.T) { conformDuplicateSlug(t, newRepo(t)) })
	t.Run(name+"/QuotaRejectsOverCap", func(t *testing.T) { conformQuotaRejectsOverCap(t, newRepo(t)) })
	t.Run(name+"/QuotaCountsAllVersions", func(t *testing.T) { conformQuotaCountsAllVersions(t, newRepo(t)) })
	t.Run(name+"/AppendRevivingExpiredPasteChargesFull", func(t *testing.T) { conformAppendRevivingExpiredPasteChargesFull(t, newRepo(t)) })
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
	t.Run(name+"/ExpiredPastes", func(t *testing.T) { conformExpiredPastes(t, newRepo(t)) })
	t.Run(name+"/DeleteExpired", func(t *testing.T) { conformDeleteExpired(t, newRepo(t)) })
	t.Run(name+"/ReferencedBlobSHAs", func(t *testing.T) { conformReferencedBlobSHAs(t, newRepo(t)) })
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
		ExpiresAt:     fixedNow.Add(domain.DefaultRetentionWindow),
	}
}

// insert creates a paste with no caps (caps=0 → no quota enforcement).
// Fails the test on error.
// pendingConfirmsDrainer is implemented by a backend (the shale repo)
// whose InsertWithQuotaCheck defers the derived-index confirm step to a
// background goroutine. Draining it after an insert makes ListByOwner /
// CountByOwner / OwnerFirstSeen deterministic in tests; sqlite + slatedb
// write the index synchronously and do not implement it (the assertion is
// a no-op for them).
type pendingConfirmsDrainer interface{ WaitPendingConfirms() }

// drainConfirms blocks until any deferred confirm-insert the repo launched
// has run. No-op for backends that confirm synchronously.
func drainConfirms(r conformanceRepo) {
	if d, ok := r.(pendingConfirmsDrainer); ok {
		d.WaitPendingConfirms()
	}
}

func insert(t *testing.T, r conformanceRepo, p domain.Paste) {
	t.Helper()
	if err := r.InsertWithQuotaCheck(context.Background(), p, 0, fixedNow); err != nil {
		t.Fatalf("insert %q: %v", p.Slug, err)
	}
	// The shale backend defers the identity_pastes index write off the
	// response path; drain it so the index-reading assertions below observe
	// it. Synchronous backends are unaffected (no-op).
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
	err := r.InsertWithQuotaCheck(context.Background(), p, 0, fixedNow)
	if err == nil {
		t.Fatalf("duplicate insert should error")
	}
	// The upload service matches the slug-taken sentinel with errors.Is
	// to know it should retry with a fresh slug; every backend must
	// surface it, bare or wrapped. (Pins the contract isSlugTaken in
	// service/upload.go depends on.)
	if !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("duplicate-slug error must be storage.ErrSlugTaken (errors.Is), got %v", err)
	}
}

// --- contract: quota -------------------------------------------------

// conformQuotaConcurrentCeiling pins the strict-quota invariant under
// concurrency: N goroutines each insert a distinct paste of `body` bytes
// for ONE identity against a per-owner cap that admits only K. The
// invariant asserted is the CEILING: the bytes that land never exceed the
// cap, no matter how the inserts interleave. This is the headline
// property the shale reservation pattern exists to guarantee, and it is
// what makes "ShaleRepo passes the suite" prove strict quota. sqlite holds
// it (the insert runs in a serializable transaction) and shale holds it
// (the reservation-pattern CAS counter); the slatedb-direct backend does
// NOT (its quota sum runs outside the write transaction). Backends declare
// which side they are on via caps.StrictQuotaUnderConcurrency.
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
			// No service cap (0); per-owner cap admits k. A non-nil error
			// (over-quota, or a transient backend lock) means the paste did
			// not land; we assert only the ceiling, so the error kind does
			// not matter.
			if err := r.InsertWithQuotaCheck(context.Background(), pasteOf(slug, "key:race", body), cap, fixedNow); err == nil {
				atomic.AddInt64(&landed, 1)
			}
		}(i)
	}
	wg.Wait()
	if !caps.StrictIdentityQuotaUnderConcurrency {
		// shale: the scan-based per-identity quota check is not atomic with
		// the authoritative write, so the ceiling can be breached by a bounded
		// same-owner over-admit. Record it rather than asserting strictness.
		t.Logf("backend does not guarantee strict per-identity quota under concurrency (scan-based over-admit): %d pastes x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
		return
	}
	if landed*body > cap {
		t.Fatalf("quota ceiling breached under concurrency: %d pastes x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
	}
}

// conformExpiryQuotaSemantics pins how an expired-but-unswept paste
// affects the owner's quota sum: the ONE point backends are allowed to
// differ (conformCaps.ExpiryFreesQuotaAtReadTime + docs/SPEC.md "One
// intentional behavior change"). Insert a paste, then query
// SumActiveBytesByOwner at a time past its expiry but before any sweep
// has deleted it.
func conformExpiryQuotaSemantics(t *testing.T, r conformanceRepo, caps conformCaps) {
	p := pasteOf("xq123456", "key:xq", 500)
	p.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, p)

	// Past expiry, but the sweep has NOT run (the paste row still exists).
	after := fixedNow.Add(2 * time.Hour)
	sum, err := r.SumActiveBytesByOwner("key:xq", after)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if caps.ExpiryFreesQuotaAtReadTime {
		// Read-time exclusion (sqlite, slatedb): an expired paste stops
		// counting the instant it expires, before any sweep.
		if sum != 0 {
			t.Fatalf("read-time expiry: want 0 (expired paste excluded from sum), got %d", sum)
		}
	} else {
		// Sweep-time exclusion (shale, Option A): the bytes still count
		// until the sweep deletes the paste. Fail-safe over-count.
		if sum != 500 {
			t.Fatalf("sweep-time expiry: want 500 (expired-unswept still counts until sweep), got %d", sum)
		}
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

// conformAppendRevivingExpiredPasteChargesFull pins that appending a version
// to an EXPIRED-but-unswept paste (which the append REVIVES by resetting
// expires_at) charges the paste's FULL post-revival size - its existing live
// versions PLUS the new one - not the new version alone. Because the owner sum
// filters expires_at > now, the expired paste's existing versions are NOT in
// `used`; charging only the new version would let the revived paste come back
// live durably OVER the cap. docs/SPEC.md "Reviving an expired-but-unswept
// record charges its FULL post-revival size".
func conformAppendRevivingExpiredPasteChargesFull(t *testing.T, r conformanceRepo) {
	const cap = 1000

	// --- reject case: revived total would breach the cap ------------------
	// Paste v1 = 900, expiring in an hour.
	pr := pasteOf("rvp12345", "key:rvp", 900)
	pr.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, pr)

	// Past expiry, before any sweep: the paste is expired-unswept, so its bytes
	// no longer count (read-time exclusion).
	after := fixedNow.Add(2 * time.Hour)
	if sum, err := r.SumActiveBytesByOwner("key:rvp", after); err != nil {
		t.Fatalf("sum after expiry: %v", err)
	} else if sum != 0 {
		t.Fatalf("expired-unswept paste must not count: got %d, want 0", sum)
	}

	// Append a 900-byte v2. The append REVIVES the paste (resets expires_at), so
	// v1(900) comes back live too. Charging only v2 would see used(0)+900 <= 1000
	// and WRONGLY ADMIT, leaving the revived paste at v1+v2 = 1800 over the 1000
	// cap. The correct charge is the full post-revival total: existing live
	// v1(900) + new v2(900) = 1800 > 1000 -> reject.
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "rvp12345", domain.KindHTML, "sha-rvp-v2", 900, cap, after); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("reviving an expired paste OVER the cap must be rejected (full post-revival size charged): got %v, want ErrOverUserQuota", err)
	}
	// The rejected append left the paste untouched (still expired, still 0).
	if sum, err := r.SumActiveBytesByOwner("key:rvp", after); err != nil {
		t.Fatalf("sum after rejected append: %v", err)
	} else if sum != 0 {
		t.Fatalf("rejected append must not revive the paste: got %d, want 0 (still expired)", sum)
	}

	// --- accept case: revived total fits the cap --------------------------
	// A DIFFERENT owner: paste v1 = 400, expiring in an hour.
	pf := pasteOf("rvf12345", "key:rvf", 400)
	pf.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, pf)

	// Append a 400-byte v2 at `after`. Post-revival total = existing v1(400) +
	// new v2(400) = 800 <= 1000 -> admit. (A backend that double-charged the
	// existing bytes would wrongly reject here, so this also guards against
	// OVER-charging the revival.)
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "rvf12345", domain.KindHTML, "sha-rvf-v2", 400, cap, after); err != nil {
		t.Fatalf("reviving an expired paste WITHIN the cap should succeed (post-revival 800 <= 1000): %v", err)
	}
	// The revived paste is now live at v1+v2 = 800 bytes.
	if sum, err := r.SumActiveBytesByOwner("key:rvf", after); err != nil {
		t.Fatalf("sum after revive: %v", err)
	} else if sum != 800 {
		t.Fatalf("revived paste must count its full post-revival size: got %d, want 800 (v1 400 + v2 400)", sum)
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
	if err := r.Delete("d1234567"); err != nil {
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
	// Append resets the retention clock from `now`.
	if !p.ExpiresAt.Equal(fixedNow.Add(domain.DefaultRetentionWindow)) {
		t.Fatalf("append should reset expiry to now+window, got %v", p.ExpiresAt)
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

// conformPinOlderAfterMultipleAppends is the precise prod repro for the
// version-pinning serving bug: append v1/v2/v3 ALL unpinned (so the head
// rolls forward to v3), THEN pin a NON-adjacent older version. The
// denormalized paste head ContentSHA/Size must roll back to the pinned
// version's bytes - that head field is exactly what the public serving path
// resolves (paste head sha -> ResolveBlobID -> blob). PinUnpinRollsHead only
// covers pin-to-v1 when the head is v2 (one back); this covers the head
// sitting two versions ahead of the pin target and re-pinning to the middle.
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
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "vn123456", domain.KindHTML, "sha-vn-v2", 10, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	// Tombstone v2, then append again: next number must be 3, not a
	// reused 2 (MAX(ver_num) counts tombstones).
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

func conformExpiredPastes(t *testing.T, r conformanceRepo) {
	// One paste expiring soon, one far in the future.
	soon := pasteOf("ex123456", "key:e", 10)
	soon.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, soon)

	far := pasteOf("ex223456", "key:e", 10)
	far.ExpiresAt = fixedNow.Add(48 * time.Hour)
	insert(t, r, far)

	// At a time past `soon` but before `far`, only `soon` is expired.
	at := fixedNow.Add(2 * time.Hour)
	refs, err := r.ExpiredPastes(at)
	if err != nil {
		t.Fatalf("expired pastes: %v", err)
	}
	if !refsHaveSlug(refs, "ex123456") {
		t.Fatalf("ex123456 should be expired at %v, got %v", at, refs)
	}
	if refsHaveSlug(refs, "ex223456") {
		t.Fatalf("ex223456 should NOT be expired at %v, got %v", at, refs)
	}
	// Inclusive boundary: expires_at == now counts as expired.
	atBoundary := fixedNow.Add(time.Hour)
	refs, err = r.ExpiredPastes(atBoundary)
	if err != nil {
		t.Fatalf("expired pastes at boundary: %v", err)
	}
	if !refsHaveSlug(refs, "ex123456") {
		t.Fatalf("expires_at == now should be inclusive-expired, got %v", refs)
	}
}

// conformDeleteExpired pins the expiry pass's delete contract (docs/SPEC.md
// "The storage contract", Expiry): processing a reference the scan surfaced
// deletes the paste record (reporting true), leaves not-yet-expired pastes
// untouched, and DRAINS the scan - a re-scan after the pass sees zero
// references, and a re-processed reference is an idempotent no-op reporting
// false (no record was deleted by it).
func conformDeleteExpired(t *testing.T, r conformanceRepo) {
	// One expired paste, one still-active one.
	dead := pasteOf("de123456", "key:d", 10)
	dead.ExpiresAt = fixedNow.Add(time.Hour)
	insert(t, r, dead)

	alive := pasteOf("de223456", "key:d", 10)
	alive.ExpiresAt = fixedNow.Add(48 * time.Hour)
	insert(t, r, alive)

	at := fixedNow.Add(2 * time.Hour)
	refs, err := r.ExpiredPastes(at)
	if err != nil {
		t.Fatalf("expired pastes: %v", err)
	}
	if len(refs) != 1 || refs[0].Slug != "de123456" {
		t.Fatalf("only de123456 should be expired at %v, got %v", at, refs)
	}

	// Processing the reference deletes the record and reports true.
	deleted, err := r.DeleteExpired(refs[0])
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteExpired must report true for a live paste record")
	}
	if _, err := r.Get("de123456"); err == nil {
		t.Fatalf("expired paste should be gone after DeleteExpired")
	}

	// One pass drains what it scanned: a re-scan sees zero references.
	again, err := r.ExpiredPastes(at)
	if err != nil {
		t.Fatalf("expired pastes (re-scan): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-scan after the pass must see zero expired references, got %v", again)
	}

	// Re-processing the same reference is an idempotent no-op: no record
	// was deleted by it, so it reports false (the sweep's deleted-count
	// only reflects real paste deletions).
	deleted, err = r.DeleteExpired(refs[0])
	if err != nil {
		t.Fatalf("re-processed reference must no-op, got: %v", err)
	}
	if deleted {
		t.Fatalf("DeleteExpired must report false when the paste record was already gone")
	}

	// The not-yet-expired paste is untouched throughout.
	if _, err := r.Get("de223456"); err != nil {
		t.Fatalf("active paste must survive the expiry pass: %v", err)
	}
}

// refsHaveSlug reports whether any expired-paste reference names slug.
func refsHaveSlug(refs []domain.ExpiredPaste, slug string) bool {
	for _, ref := range refs {
		if ref.Slug.String() == slug {
			return true
		}
	}
	return false
}

// --- contract: blob GC reference set --------------------------------

func conformReferencedBlobSHAs(t *testing.T, r conformanceRepo) {
	// Empty repo: no references. (The sweep's data-loss guard depends on
	// distinguishing "legitimately empty" from "buggy zero": here it's
	// legitimately empty, with no pastes.)
	refs, err := r.ReferencedBlobSHAs()
	if err != nil {
		t.Fatalf("referenced shas (empty): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("empty repo should reference no shas, got %v", refs)
	}

	// One paste + one extra version → BOTH shas are referenced. The
	// method returns the referenced (allow-list) set the sweep keeps;
	// any blob NOT in it is GC'd. Pinned as the CURRENT contract the
	// sweep relies on. A backend that returned an empty set here while
	// pastes exist would trip the sweep's abort-on-zero-refs guard.
	insert(t, r, pasteOf("gc123456", "key:g", 10)) // v1 sha = sha-gc123456-v1
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "gc123456", domain.KindHTML, "sha-gc-v2", 20, 0, fixedNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	refs, err = r.ReferencedBlobSHAs()
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

	// CANONICAL RULE: a TOMBSTONED version's content sha is NOT in the
	// referenced set, so its blob is GC-able (a deleted version is
	// app-final and content-inaccessible; freeing quota frees storage).
	// The two existing backends still differ on this detail, so the
	// suite pins the divergence rather than asserting a value they
	// disagree on:
	//   - slatedb (SlateRepo.ReferencedBlobSHAs): filters !v.Deleted, so
	//     a tombstoned version's sha is NOT referenced and its blob
	//     becomes eligible for GC. This is the canonical rule.
	//   - sqlite (PasteRepo.ReferencedBlobSHAs): SELECT DISTINCT
	//     content_sha FROM versions has NO deleted filter, so a
	//     tombstoned version's sha IS still referenced (its blob is NOT
	//     GC'd while any tombstone points at it). This diverges from the
	//     canonical rule and is a known open follow-up.
	// This suite assertion is intentionally limited to NON-deleted shas,
	// which both backends agree are always referenced. When the sqlite
	// backend is brought in line with the canonical rule, extend this
	// suite to assert the tombstoned sha is absent from the set.
	if err := r.DeleteVersion("gc123456", 2); err != nil {
		t.Fatalf("tombstone v2: %v", err)
	}
	refs, err = r.ReferencedBlobSHAs()
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

	// #464 regression: CountByOwner must count only LIVE pastes, matching
	// ListByOwner, even when a delete leaves a stale (orphaned) derived-index
	// entry behind. Delete one of the two pastes; the count and the list must
	// both drop to 1 and AGREE. A raw len(index) count would over-report the
	// orphan (the whoami-shows-2 / list-shows-1 mismatch).
	if err := r.Delete("st223456"); err != nil {
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
	for i := range 3 {
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
	return slices.Contains(ss, want)
}
