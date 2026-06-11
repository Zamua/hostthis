package storage_test

// Site operations in the backend-agnostic conformance suite.
//
// These subtests pin the OBSERVABLE static-site contract every metadata
// backend that supports sites must hold identically (sqlite and slatedb
// today; shale when wired). They run only when the backend's factory
// supplies a non-nil site repo (newSites). A site repo is the union of
// service.SiteRepo (deploy + read + per-owner site-byte sum) and
// service.SweepSites (expiry slugs + delete + referenced blob set).
//
// The site repo and the paste repo from one factory call MUST share the
// same backing store, so the cross-quota subtests (a site's bytes affect a
// paste's quota check and vice versa) and the cross-family slug-collision
// subtest exercise the real interaction, not two independent stores.

import (
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

// conformanceSiteRepo is the union of the two site-side service interfaces
// a backend that supports static sites must satisfy. The sqlite
// *storage.SiteRepo and the slatedb *storage.SlateSiteRepo both implement
// it.
type conformanceSiteRepo interface {
	service.SiteRepo
	service.SweepSites
}

// siteOf builds a Site with one file whose content is derived from the
// slug, stamped at fixedNow with the standard retention window. size is
// the single file's (and therefore the deduped) byte total.
func siteOf(slug, identity string, size int) domain.Site {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA:         "sha-" + slug + "-index",
		Size:        size,
		ContentType: "text/html; charset=utf-8",
	})
	return domain.Site{
		Slug:      domain.Slug(slug),
		Identity:  domain.Identity(identity),
		Manifest:  man,
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
		ExpiresAt: fixedNow.Add(domain.RetentionWindow),
	}
}

// insertSite deploys a site with no caps (caps=0 -> no quota enforcement).
func insertSite(t *testing.T, sr conformanceSiteRepo, s domain.Site) {
	t.Helper()
	if err := sr.InsertWithQuotaCheck(s, s.Manifest.DedupedSize(), 0, 0, fixedNow); err != nil {
		t.Fatalf("insert site %q: %v", s.Slug, err)
	}
}

// runSiteConformance runs the site contract subtests. `newSites` produces
// a fresh paste repo + site repo pair (sharing one backing store) per
// subtest, matching the paste suite's "fresh empty repo per subtest"
// discipline so the empty-store assertions hold. `caps` declares the
// backend's by-design behavior exceptions (the same flags the paste suite
// uses; sites honor ExpiryFreesQuotaAtReadTime).
func runSiteConformance(t *testing.T, name string, caps conformCaps, newSites func(t *testing.T) (conformanceRepo, conformanceSiteRepo)) {
	t.Helper()
	t.Run(name+"/Sites/DeployAndReadBack", func(t *testing.T) { _, sr := newSites(t); conformSiteDeployAndReadBack(t, sr) })
	t.Run(name+"/Sites/GetNotFound", func(t *testing.T) { _, sr := newSites(t); conformSiteGetNotFound(t, sr) })
	t.Run(name+"/Sites/SumByIdentity", func(t *testing.T) { _, sr := newSites(t); conformSiteSumByIdentity(t, sr) })
	t.Run(name+"/Sites/QuotaCountsSiteBytes", func(t *testing.T) { _, sr := newSites(t); conformSiteQuotaCountsSiteBytes(t, sr) })
	t.Run(name+"/Sites/PerOwnerCapCountsBoth", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapCountsBoth(t, r, sr) })
	t.Run(name+"/Sites/PerOwnerCapConcurrentCeiling", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapConcurrentCeiling(t, caps, r, sr) })
	t.Run(name+"/Sites/ServiceCapCountsBoth", func(t *testing.T) { r, sr := newSites(t); conformSiteServiceCapCountsBoth(t, r, sr) })
	t.Run(name+"/Sites/SlugCollisionVsPaste", func(t *testing.T) { r, sr := newSites(t); conformSiteSlugCollisionVsPaste(t, r, sr) })
	t.Run(name+"/Sites/ExpiryAndSweep", func(t *testing.T) { _, sr := newSites(t); conformSiteExpiryAndSweep(t, caps, sr) })
	t.Run(name+"/Sites/ExpirySubSecondOrdering", func(t *testing.T) { _, sr := newSites(t); conformSiteExpirySubSecondOrdering(t, sr) })
	t.Run(name+"/Sites/ReferencedBlobSHAs", func(t *testing.T) { _, sr := newSites(t); conformSiteReferencedBlobSHAs(t, sr) })
	t.Run(name+"/Sites/DedupedSizeCharged", func(t *testing.T) { _, sr := newSites(t); conformSiteDedupedSizeCharged(t, sr) })
}

// conformSiteDeployAndReadBack deploys a multi-file site and reads every
// path back byte-for-byte (sha + size + content-type), so the manifest
// round-trips through the backend's encoding identically.
func conformSiteDeployAndReadBack(t *testing.T, sr conformanceSiteRepo) {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "sha-rb-index", Size: 100, ContentType: "text/html; charset=utf-8"})
	man.Add("assets/app.js", domain.ManifestEntry{SHA: "sha-rb-js", Size: 200, ContentType: "text/javascript; charset=utf-8"})
	man.Add("style.css", domain.ManifestEntry{SHA: "sha-rb-css", Size: 50, ContentType: "text/css; charset=utf-8"})
	s := domain.Site{
		Slug:      "rb123456",
		Identity:  "key:rb",
		Manifest:  man,
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
		ExpiresAt: fixedNow.Add(domain.RetentionWindow),
	}
	insertSite(t, sr, s)

	got, err := sr.Get(s.Slug)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	if got.Slug != s.Slug || got.Identity != s.Identity {
		t.Fatalf("site round-trip mismatch: got slug=%q id=%q", got.Slug, got.Identity)
	}
	if !got.CreatedAt.Equal(s.CreatedAt) || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("site time round-trip mismatch: got %v/%v want %v/%v",
			got.CreatedAt, got.ExpiresAt, s.CreatedAt, s.ExpiresAt)
	}
	if len(got.Manifest.Files) != len(s.Manifest.Files) {
		t.Fatalf("manifest file count: got %d, want %d", len(got.Manifest.Files), len(s.Manifest.Files))
	}
	for p, want := range s.Manifest.Files {
		ge, ok := got.Manifest.Files[p]
		if !ok {
			t.Fatalf("manifest missing path %q after round-trip", p)
		}
		if ge.SHA != want.SHA || ge.Size != want.Size || ge.ContentType != want.ContentType {
			t.Fatalf("manifest entry %q mismatch: got %+v, want %+v", p, ge, want)
		}
	}
}

func conformSiteGetNotFound(t *testing.T, sr conformanceSiteRepo) {
	if _, err := sr.Get("nosite12"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get missing site: got %v, want ErrNotFound", err)
	}
}

func conformSiteSumByIdentity(t *testing.T, sr conformanceSiteRepo) {
	insertSite(t, sr, siteOf("ss123456", "key:ss", 100))
	insertSite(t, sr, siteOf("ss223456", "key:ss", 250))
	// A different owner's site must not leak into the sum.
	insertSite(t, sr, siteOf("ss323456", "key:other", 500))

	used, err := sr.SumActiveBytesByOwner("key:ss", fixedNow)
	if err != nil {
		t.Fatalf("sum active site bytes: %v", err)
	}
	if used != 350 {
		t.Fatalf("sum active site bytes by owner: got %d, want 350", used)
	}
	// Unknown owner -> zero, no error.
	used, err = sr.SumActiveBytesByOwner("key:nobody", fixedNow)
	if err != nil {
		t.Fatalf("sum active site bytes (unknown): %v", err)
	}
	if used != 0 {
		t.Fatalf("unknown owner site sum should be 0, got %d", used)
	}
}

// conformSiteQuotaCountsSiteBytes pins that a site deploy enforces the
// per-identity cap against the owner's existing SITE bytes.
func conformSiteQuotaCountsSiteBytes(t *testing.T, sr conformanceSiteRepo) {
	const cap = 1000
	// First site fits at 600.
	if err := sr.InsertWithQuotaCheck(siteOf("sq123456", "key:sq", 600), 600, 0, cap, fixedNow); err != nil {
		t.Fatalf("first site (600 under 1000): %v", err)
	}
	// Second would be 600+500=1100 > 1000 -> reject.
	err := sr.InsertWithQuotaCheck(siteOf("sq223456", "key:sq", 500), 500, 0, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap site deploy: got %v, want ErrOverUserQuota", err)
	}
	// A smaller one that keeps the sum under cap succeeds.
	if err := sr.InsertWithQuotaCheck(siteOf("sq323456", "key:sq", 300), 300, 0, cap, fixedNow); err != nil {
		t.Fatalf("site within cap (600+300=900): %v", err)
	}
}

// conformSitePerOwnerCapCountsBoth pins the cross-kind PER-OWNER cap: the
// owner's paste bytes and site bytes both count toward the same per-owner
// cap, in BOTH directions. The headline asymmetry this guards against: a
// site sees paste bytes but a paste must ALSO see site bytes, so a deploy of
// either kind is rejected when the owner's COMBINED paste+site bytes plus
// the new body would exceed userCap. The reproduced bug: an 800-byte site +
// a 300-byte paste under a 1000-byte cap wrongly admitted the paste on the
// KV backends (combined 1100 > 1000) while sqlite (and the symmetric site
// direction) correctly rejected it.
func conformSitePerOwnerCapCountsBoth(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	const cap = 1000

	// Direction 1: a SITE fills most of the cap, then a PASTE that would
	// overflow the COMBINED total is rejected. This is the direction the bug
	// broke: the paste's per-owner check must count the existing site bytes.
	if err := sr.InsertWithQuotaCheck(siteOf("pb1site1", "key:pb1", 800), 800, 0, cap, fixedNow); err != nil {
		t.Fatalf("site 800 under cap: %v", err)
	}
	// 800 (site) + 300 (paste) = 1100 > 1000 -> the paste MUST be rejected.
	if err := r.InsertWithQuotaCheck(pasteOf("pb1pst1", "key:pb1", 300), 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste over combined cap (site bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// A paste that keeps the combined total at/under cap fits: 800+200=1000.
	if err := r.InsertWithQuotaCheck(pasteOf("pb1pst2", "key:pb1", 200), 0, cap, fixedNow); err != nil {
		t.Fatalf("paste within combined cap (800+200=1000): %v", err)
	}
	// And now the owner is full: another byte is rejected.
	if err := r.InsertWithQuotaCheck(pasteOf("pb1pst3", "key:pb1", 1), 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste at full combined cap should be rejected: got %v", err)
	}

	// Direction 2 (the reverse): a PASTE fills most of the cap, then a SITE
	// that would overflow the COMBINED total is rejected. This direction was
	// already correct (the site path summed both), pinned here for symmetry.
	if err := r.InsertWithQuotaCheck(pasteOf("pb2pst1", "key:pb2", 800), 0, cap, fixedNow); err != nil {
		t.Fatalf("paste 800 under cap: %v", err)
	}
	// 800 (paste) + 300 (site) = 1100 > 1000 -> the site MUST be rejected.
	if err := sr.InsertWithQuotaCheck(siteOf("pb2site1", "key:pb2", 300), 300, 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("site over combined cap (paste bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// A site that keeps the combined total at/under cap fits: 800+200=1000.
	if err := sr.InsertWithQuotaCheck(siteOf("pb2site2", "key:pb2", 200), 200, 0, cap, fixedNow); err != nil {
		t.Fatalf("site within combined cap (800+200=1000): %v", err)
	}

	// An append also counts site bytes: the owner is at cap, so any append is
	// rejected (covers the AppendVersionWithQuotaCheck cross-kind path).
	if _, err := r.AppendVersionWithQuotaCheck("pb2pst1", domain.KindHTML, "sha-pb2-v2", 1, 0, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("append at full combined cap should be rejected (site bytes must count): got %v", err)
	}
}

// conformSitePerOwnerCapConcurrentCeiling pins the COMBINED per-owner
// ceiling under concurrent CROSS-KIND deploys: N goroutines each deploy a
// distinct paste OR site of `body` bytes for ONE owner against a per-owner
// cap that admits only K combined. The invariant is the ceiling: the bytes
// that land (paste bytes + site bytes) never exceed the cap, no matter how
// the cross-kind inserts interleave. This keeps StrictQuotaUnderConcurrency
// honest across kinds: a backend that checked paste and site quota on
// separate, non-serialized paths could let the combined total overshoot even
// if each kind alone held its ceiling. sqlite holds it (serializable
// insert tx, one identityActiveBytes spanning both kinds) and shale holds it
// (both reserve steps CAS the same {id} shard, each reading both counters);
// the slatedb-direct backend does NOT (its quota sums run outside the write
// tx), so it only logs.
func conformSitePerOwnerCapConcurrentCeiling(t *testing.T, caps conformCaps, r conformanceRepo, sr conformanceSiteRepo) {
	const (
		body = 100
		k    = 3
		n    = 8
	)
	cap := int64(k * body) // admits exactly k records of `body`, any mix of kinds
	var landed int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Alternate kinds so the race is genuinely cross-kind: even i ->
			// paste, odd i -> site, all for the same owner "key:ccx".
			var err error
			if i%2 == 0 {
				err = r.InsertWithQuotaCheck(pasteOf(fmt.Sprintf("ccp%05d", i), "key:ccx", body), 0, cap, fixedNow)
			} else {
				err = sr.InsertWithQuotaCheck(siteOf(fmt.Sprintf("ccs%05d", i), "key:ccx", body), body, 0, cap, fixedNow)
			}
			if err == nil {
				atomic.AddInt64(&landed, 1)
			}
		}(i)
	}
	wg.Wait()
	if !caps.StrictQuotaUnderConcurrency {
		t.Logf("backend does not guarantee strict cross-kind quota under concurrency (known slatedb-direct race): %d records x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
		return
	}
	if landed*body > cap {
		t.Fatalf("combined per-owner quota ceiling breached under cross-kind concurrency: %d records x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
	}
}

// conformSiteServiceCapCountsBoth pins the cross-kind service-wide cap: a
// paste's bytes and a site's bytes both count toward the same service cap,
// so a site deploy sees the bytes a paste already holds and a paste upload
// sees the bytes a site already holds.
func conformSiteServiceCapCountsBoth(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	const svcCap = 1000
	// A paste takes 700 of the service-wide budget.
	if err := r.InsertWithQuotaCheck(pasteOf("scp23456", "key:alice", 700), svcCap, 0, fixedNow); err != nil {
		t.Fatalf("paste 700 under svc cap: %v", err)
	}
	// A site of 400 would push the service total to 1100 > 1000 -> reject.
	// This proves the site deploy's service-wide sum INCLUDES paste bytes.
	if err := sr.InsertWithQuotaCheck(siteOf("scs23456", "key:bob", 400), 400, svcCap, 0, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("site over service cap (paste bytes must count): got %v, want ErrServiceFull", err)
	}
	// A site of 200 fits (700+200=900 <= 1000).
	if err := sr.InsertWithQuotaCheck(siteOf("scs33456", "key:bob", 200), 200, svcCap, 0, fixedNow); err != nil {
		t.Fatalf("site within service cap (700+200=900): %v", err)
	}
	// Now a paste of 200 would push to 1100 -> reject. This proves the
	// PASTE upload's service-wide sum INCLUDES site bytes.
	if err := r.InsertWithQuotaCheck(pasteOf("scp33456", "key:carol", 200), svcCap, 0, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("paste over service cap (site bytes must count): got %v, want ErrServiceFull", err)
	}
}

// conformSiteSlugCollisionVsPaste pins the both-directions slug collision:
// a slug is EITHER a site or a paste, never both.
func conformSiteSlugCollisionVsPaste(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// A paste owns "col12345"; a site deploy onto the same slug is rejected.
	insert(t, r, pasteOf("col12345", "key:c", 10))
	err := sr.InsertWithQuotaCheck(siteOf("col12345", "key:c", 10), 10, 0, 0, fixedNow)
	if err == nil {
		t.Fatalf("site deploy onto a paste's slug must be rejected")
	}
	if !containsSlug(err) {
		t.Fatalf("site-vs-paste collision error must contain %q, got %q", "slug", err.Error())
	}

	// A site owns "col22345"; a paste insert onto the same slug is rejected.
	insertSite(t, sr, siteOf("col22345", "key:c", 10))
	perr := r.InsertWithQuotaCheck(pasteOf("col22345", "key:c", 10), 0, 0, fixedNow)
	if perr == nil {
		t.Fatalf("paste insert onto a site's slug must be rejected")
	}
	if !containsSlug(perr) {
		t.Fatalf("paste-vs-site collision error must contain %q, got %q", "slug", perr.Error())
	}

	// Two sites cannot share a slug either.
	insertSite(t, sr, siteOf("col32345", "key:c", 10))
	derr := sr.InsertWithQuotaCheck(siteOf("col32345", "key:c", 10), 10, 0, 0, fixedNow)
	if derr == nil {
		t.Fatalf("duplicate site slug must be rejected")
	}
	if !containsSlug(derr) {
		t.Fatalf("duplicate-site-slug error must contain %q, got %q", "slug", derr.Error())
	}
}

// conformSiteExpiryAndSweep pins ExpiredSiteSlugs (inclusive boundary),
// Delete (frees quota), and the read-time expiry exclusion from the sum
// (on backends that free quota at read time).
func conformSiteExpiryAndSweep(t *testing.T, caps conformCaps, sr conformanceSiteRepo) {
	soon := siteOf("se123456", "key:se", 100)
	soon.ExpiresAt = fixedNow.Add(time.Hour)
	insertSite(t, sr, soon)
	far := siteOf("se223456", "key:se", 100)
	far.ExpiresAt = fixedNow.Add(48 * time.Hour)
	insertSite(t, sr, far)

	// At a time past `soon` but before `far`, only `soon` is expired.
	at := fixedNow.Add(2 * time.Hour)
	slugs, err := sr.ExpiredSiteSlugs(at)
	if err != nil {
		t.Fatalf("expired site slugs: %v", err)
	}
	if !sliceHas(slugs, "se123456") {
		t.Fatalf("se123456 should be expired at %v, got %v", at, slugs)
	}
	if sliceHas(slugs, "se223456") {
		t.Fatalf("se223456 should NOT be expired at %v, got %v", at, slugs)
	}
	// Inclusive boundary: expires_at == now counts as expired.
	atBoundary := fixedNow.Add(time.Hour)
	slugs, err = sr.ExpiredSiteSlugs(atBoundary)
	if err != nil {
		t.Fatalf("expired site slugs at boundary: %v", err)
	}
	if !sliceHas(slugs, "se123456") {
		t.Fatalf("expires_at == now should be inclusive-expired, got %v", slugs)
	}

	// Read-time expiry exclusion from the owner sum (on backends that free
	// quota at read time). At `at`, soon is expired-unswept; far is alive.
	used, err := sr.SumActiveBytesByOwner("key:se", at)
	if err != nil {
		t.Fatalf("sum at expiry: %v", err)
	}
	if caps.ExpiryFreesQuotaAtReadTime {
		if used != 100 {
			t.Fatalf("read-time expiry: want 100 (only far counts), got %d", used)
		}
	} else {
		if used != 200 {
			t.Fatalf("sweep-time expiry: want 200 (expired-unswept still counts), got %d", used)
		}
	}

	// Delete the expired site; it leaves the listing and frees its bytes.
	if err := sr.Delete("se123456"); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if _, err := sr.Get("se123456"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted site should be gone: %v", err)
	}
	// Delete is idempotent (sweep may re-delete a slug a prior tick removed).
	if err := sr.Delete("se123456"); err != nil {
		t.Fatalf("re-delete missing site should be a no-op, got %v", err)
	}
	// After deleting soon, the owner's live sum is just far (100).
	used, err = sr.SumActiveBytesByOwner("key:se", fixedNow)
	if err != nil {
		t.Fatalf("sum after delete: %v", err)
	}
	if used != 100 {
		t.Fatalf("post-delete site sum: got %d, want 100", used)
	}
}

// conformSiteExpirySubSecondOrdering pins that the site expiry index orders
// by TIME within a shared whole second: the expiry-index key's byte order
// must equal time order even when ExpiresAt carries a sub-second fraction.
//
// This is the regression guard for the variable-width-timestamp bug: with
// time.RFC3339Nano (trailing fractional zeros dropped) a key at "...00.5Z"
// sorts BEFORE a whole-second cutoff "...00Z" (because '.' < 'Z'), so a
// site that expires LATER in the second (e.g. .5s) would be reported expired
// at a cutoff EARLIER in the same second (.0s) - swept up to ~1s early. The
// fixed-width format (zero-padded 9-digit nanos) makes byte order == time
// order, so this subtest fails on the unfixed key format and passes on the
// fixed one.
func conformSiteExpirySubSecondOrdering(t *testing.T, sr conformanceSiteRepo) {
	base := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// A site that expires HALF A SECOND into the whole second.
	late := siteOf("essLate1", "key:ess", 100)
	late.ExpiresAt = base.Add(500 * time.Millisecond) // 12:00:00.5
	insertSite(t, sr, late)

	// A site that expires at the START of the same whole second.
	early := siteOf("essEarly", "key:ess", 100)
	early.ExpiresAt = base // 12:00:00.0
	insertSite(t, sr, early)

	// Cutoff at the start of the whole second (.0s): the .5s site has NOT
	// expired yet, the .0s site has (inclusive boundary). Under the buggy
	// variable-width format the .5s key would sort before this cutoff and be
	// wrongly reported expired.
	atStart := base // 12:00:00.0
	slugs, err := sr.ExpiredSiteSlugs(atStart)
	if err != nil {
		t.Fatalf("expired site slugs at .0s: %v", err)
	}
	if sliceHas(slugs, "essLate1") {
		t.Fatalf("site expiring at .5s must NOT be expired at a .0s cutoff (sub-second ordering bug), got %v", slugs)
	}
	if !sliceHas(slugs, "essEarly") {
		t.Fatalf("site expiring at .0s should be inclusive-expired at a .0s cutoff, got %v", slugs)
	}

	// Cutoff at .5s: now BOTH are expired (the .5s site is at the inclusive
	// boundary, the .0s site is past it).
	atHalf := base.Add(500 * time.Millisecond) // 12:00:00.5
	slugs, err = sr.ExpiredSiteSlugs(atHalf)
	if err != nil {
		t.Fatalf("expired site slugs at .5s: %v", err)
	}
	if !sliceHas(slugs, "essLate1") {
		t.Fatalf("site expiring at .5s should be inclusive-expired at a .5s cutoff, got %v", slugs)
	}
	if !sliceHas(slugs, "essEarly") {
		t.Fatalf("site expiring at .0s should still be expired at a .5s cutoff, got %v", slugs)
	}

	// And a cutoff just BELOW the .5s site (e.g. .4s) leaves it unexpired,
	// proving the boundary is real sub-second time, not whole-second rounding.
	atBelow := base.Add(400 * time.Millisecond) // 12:00:00.4
	slugs, err = sr.ExpiredSiteSlugs(atBelow)
	if err != nil {
		t.Fatalf("expired site slugs at .4s: %v", err)
	}
	if sliceHas(slugs, "essLate1") {
		t.Fatalf("site expiring at .5s must NOT be expired at a .4s cutoff, got %v", slugs)
	}
}

// conformSiteReferencedBlobSHAs pins the site-side referenced-blob set the
// sweep unions into its keep-alive set.
func conformSiteReferencedBlobSHAs(t *testing.T, sr conformanceSiteRepo) {
	// Empty: no sites, no referenced shas.
	refs, err := sr.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas (empty): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("empty repo should reference no site shas, got %v", refs)
	}

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "sha-ref-index", Size: 10, ContentType: "text/html; charset=utf-8"})
	man.Add("app.js", domain.ManifestEntry{SHA: "sha-ref-js", Size: 20, ContentType: "text/javascript; charset=utf-8"})
	// Two paths pointing at the SAME blob: it appears once in the set.
	man.Add("copy.html", domain.ManifestEntry{SHA: "sha-ref-index", Size: 10, ContentType: "text/html; charset=utf-8"})
	s := domain.Site{
		Slug: "rf123456", Identity: "key:rf", Manifest: man,
		CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RetentionWindow),
	}
	insertSite(t, sr, s)

	refs, err = sr.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas: %v", err)
	}
	if !sliceHas(refs, "sha-ref-index") || !sliceHas(refs, "sha-ref-js") {
		t.Fatalf("both distinct site blob shas should be referenced, got %v", refs)
	}
	// Distinct dedup: index sha appears once despite two paths.
	count := 0
	for _, s := range refs {
		if s == "sha-ref-index" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("a deduped blob sha should appear once in the referenced set, got %d", count)
	}
}

// conformSiteDedupedSizeCharged pins that the bytes charged against quota
// are the DEDUPED size (distinct blobs), not the sum over all paths: two
// paths to the same blob count once.
func conformSiteDedupedSizeCharged(t *testing.T, sr conformanceSiteRepo) {
	man := domain.NewManifest()
	man.Add("a.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("b.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("c.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	s := domain.Site{
		Slug: "dd123456", Identity: "key:dd", Manifest: man,
		CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RetentionWindow),
	}
	// DedupedSize is 400 (one distinct blob), not 1200.
	if got := s.Manifest.DedupedSize(); got != 400 {
		t.Fatalf("DedupedSize should be 400 (one distinct blob), got %d", got)
	}
	insertSite(t, sr, s)
	used, err := sr.SumActiveBytesByOwner("key:dd", fixedNow)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if used != 400 {
		t.Fatalf("deduped size charged: got %d, want 400 (three paths, one blob)", used)
	}
}
