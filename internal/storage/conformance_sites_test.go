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
		ExpiresAt: fixedNow.Add(domain.DefaultRetentionWindow),
	}
}

// siteOfV is siteOf with a version tag folded into the file's SHA, so two
// deploys of the SAME slug at different v produce DISTINCT manifests (a fresh
// content SHA). Used by the replace conformance to prove the row's manifest
// actually swaps, not just its timestamps.
func siteOfV(slug, identity string, size int, v string) domain.Site {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA:         "sha-" + slug + "-" + v,
		Size:        size,
		ContentType: "text/html; charset=utf-8",
	})
	return domain.Site{
		Slug:      domain.Slug(slug),
		Identity:  domain.Identity(identity),
		Manifest:  man,
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
		ExpiresAt: fixedNow.Add(domain.DefaultRetentionWindow),
	}
}

// insertSite deploys a site with no caps (caps=0 -> no quota enforcement).
func insertSite(t *testing.T, sr conformanceSiteRepo, s domain.Site) {
	t.Helper()
	if err := sr.InsertWithQuotaCheck(context.Background(), s, s.Manifest.DedupedSize(), 0, fixedNow); err != nil {
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
	t.Run(name+"/Sites/SlugCollisionVsPaste", func(t *testing.T) { r, sr := newSites(t); conformSiteSlugCollisionVsPaste(t, r, sr) })
	t.Run(name+"/Sites/ExpiryAndSweep", func(t *testing.T) { _, sr := newSites(t); conformSiteExpiryAndSweep(t, caps, sr) })
	t.Run(name+"/Sites/DeleteExpiredSite", func(t *testing.T) { _, sr := newSites(t); conformDeleteExpiredSite(t, sr) })
	t.Run(name+"/Sites/ExpirySubSecondOrdering", func(t *testing.T) { _, sr := newSites(t); conformSiteExpirySubSecondOrdering(t, sr) })
	t.Run(name+"/Sites/ReferencedBlobSHAs", func(t *testing.T) { _, sr := newSites(t); conformSiteReferencedBlobSHAs(t, sr) })
	t.Run(name+"/Sites/DedupedSizeCharged", func(t *testing.T) { _, sr := newSites(t); conformSiteDedupedSizeCharged(t, sr) })
	t.Run(name+"/Sites/ReplaceInPlace", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceInPlace(t, sr) })
	t.Run(name+"/Sites/ReplaceNotFoundShape", func(t *testing.T) { r, sr := newSites(t); conformSiteReplaceNotFoundShape(t, r, sr) })
	t.Run(name+"/Sites/ReplaceDeltaQuota", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceDeltaQuota(t, sr) })
	t.Run(name+"/Sites/ReplaceRevivesExpiredChargesFull", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceRevivesExpiredChargesFull(t, sr) })
	t.Run(name+"/Sites/ReplaceRestartsExpiry", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceRestartsExpiry(t, sr) })
	t.Run(name+"/Sites/ListByOwner", func(t *testing.T) { _, sr := newSites(t); conformSiteListByOwner(t, sr) })
}

// conformSiteListByOwner pins ListSitesByOwner: it returns exactly the
// owner's active sites (owner-scoped, not another owner's), reflects a
// delete, and stays consistent with the enumeration index through the
// deploy/delete lifecycle. This is the contract that makes static sites
// visible in `ssh <apex> list` so the shared quota is legible + reclaimable.
func conformSiteListByOwner(t *testing.T, sr conformanceSiteRepo) {
	ownerA, ownerB := "key:AAAA", "key:BBBB"
	insertSite(t, sr, siteOf("aone1111", ownerA, 100))
	insertSite(t, sr, siteOf("atwo2222", ownerA, 200))
	insertSite(t, sr, siteOf("bone3333", ownerB, 300))

	slugsOf := func(owner string) map[string]bool {
		t.Helper()
		got, err := sr.ListSitesByOwner(owner, fixedNow)
		if err != nil {
			t.Fatalf("list sites %s: %v", owner, err)
		}
		m := make(map[string]bool, len(got))
		for _, s := range got {
			if string(s.Identity) != owner {
				t.Fatalf("owner leak: %s listed under %s", s.Identity, owner)
			}
			m[string(s.Slug)] = true
		}
		return m
	}

	if a := slugsOf(ownerA); len(a) != 2 || !a["aone1111"] || !a["atwo2222"] {
		t.Fatalf("ownerA sites: got %v want {aone1111, atwo2222}", a)
	}
	if b := slugsOf(ownerB); len(b) != 1 || !b["bone3333"] {
		t.Fatalf("ownerB sites: got %v want {bone3333}", b)
	}

	// Delete one of A's sites: it must drop out of the listing (index stays
	// consistent), the other survives, B is untouched.
	if err := sr.Delete("aone1111"); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if a := slugsOf(ownerA); len(a) != 1 || !a["atwo2222"] || a["aone1111"] {
		t.Fatalf("after delete, ownerA sites: got %v want {atwo2222}", a)
	}
	if b := slugsOf(ownerB); len(b) != 1 || !b["bone3333"] {
		t.Fatalf("after delete, ownerB sites changed: got %v", b)
	}

	// An owner with no sites lists empty (not an error).
	if none := slugsOf("key:NOBODY"); len(none) != 0 {
		t.Fatalf("owner with no sites should list empty, got %v", none)
	}
}

// conformSiteReplaceInPlace pins the core re-deploy contract: replacing an
// existing owned site swaps the manifest (new content SHA) while keeping the
// slug + created_at stable, so rollback/history ride the same identity.
func conformSiteReplaceInPlace(t *testing.T, sr conformanceSiteRepo) {
	const slug = "rp123456"
	v1 := siteOfV(slug, "key:rp", 100, "v1")
	insertSite(t, sr, v1)

	// Re-deploy the SAME slug with distinct content at a later updated_at.
	later := fixedNow.Add(2 * time.Hour)
	v2 := siteOfV(slug, "key:rp", 250, "v2")
	v2.CreatedAt = later // a hostile caller can't move created_at via the row
	v2.UpdatedAt = later
	v2.ExpiresAt = later.Add(domain.DefaultRetentionWindow)
	if err := sr.ReplaceWithQuotaCheck(context.Background(), v2, v2.Manifest.DedupedSize(), 0, later); err != nil {
		t.Fatalf("replace in place: %v", err)
	}

	got, err := sr.Get(slug)
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	// The manifest swapped to v2's content.
	e, ok := got.Manifest.Files["index.html"]
	if !ok || e.SHA != "sha-"+slug+"-v2" || e.Size != 250 {
		t.Fatalf("manifest not swapped: got %+v", got.Manifest.Files)
	}
	// created_at is the slug's birth time, unchanged by the re-deploy.
	if !got.CreatedAt.Equal(fixedNow) {
		t.Fatalf("created_at must be stable across re-deploy: got %v, want %v", got.CreatedAt, fixedNow)
	}
	// updated_at + expires_at restarted from the re-deploy.
	if !got.UpdatedAt.Equal(later) {
		t.Fatalf("updated_at should be the re-deploy time: got %v, want %v", got.UpdatedAt, later)
	}
	if !got.ExpiresAt.Equal(later.Add(domain.DefaultRetentionWindow)) {
		t.Fatalf("expires_at should restart from re-deploy: got %v", got.ExpiresAt)
	}
	// The owner's live site bytes reflect the NEW size only (250), not 100+250.
	used, err := sr.SumActiveBytesByOwner("key:rp", later)
	if err != nil {
		t.Fatalf("sum after replace: %v", err)
	}
	if used != 250 {
		t.Fatalf("replace must not double-count: owner sum got %d, want 250", used)
	}
}

// conformSiteReplaceNotFoundShape pins that a replace targeting a slug the
// identity does not own collapses to ErrNotFound - the SAME sentinel a
// missing slug yields - for both the foreign-owner and the missing-row cases,
// AND for a slug that exists only as a PASTE (never a site). No existence or
// ownership leak.
func conformSiteReplaceNotFoundShape(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// Missing slug: never deployed.
	miss := siteOfV("rmiss123", "key:owner", 50, "v1")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), miss, miss.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace of missing slug: got %v, want ErrNotFound", err)
	}

	// Foreign-owned: owned by key:alice, a replace as key:mallory is NotFound.
	insertSite(t, sr, siteOfV("rfor1234", "key:alice", 100, "v1"))
	foreign := siteOfV("rfor1234", "key:mallory", 100, "v2")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), foreign, foreign.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace of foreign-owned site: got %v, want ErrNotFound (no ownership leak)", err)
	}
	// The legitimate owner's site is untouched by the rejected foreign replace.
	got, err := sr.Get("rfor1234")
	if err != nil {
		t.Fatalf("owner's site after rejected foreign replace: %v", err)
	}
	if e := got.Manifest.Files["index.html"]; e.SHA != "sha-rfor1234-v1" {
		t.Fatalf("foreign replace must not mutate the owner's row: got %+v", got.Manifest.Files)
	}

	// Slug that exists only as a PASTE: a site replace must not find it.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("rpaste12", "key:owner", 10), 0, fixedNow); err != nil {
		t.Fatalf("seed paste: %v", err)
	}
	asPaste := siteOfV("rpaste12", "key:owner", 10, "v1")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), asPaste, asPaste.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace targeting a paste-only slug: got %v, want ErrNotFound", err)
	}
}

// conformSiteReplaceDeltaQuota pins that the replace charges the DELTA, not
// the full new size: a same-size re-deploy nets zero against the cap, a
// smaller one frees the difference (admitting a follow-up that the original
// size would have blocked), and a larger one is rejected when the post-swap
// total would breach the cap (the old bytes credited, the new charged).
func conformSiteReplaceDeltaQuota(t *testing.T, sr conformanceSiteRepo) {
	const cap = 1000
	// Owner sits at 800 of a 1000 cap.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 800, "v1"), 800, cap, fixedNow); err != nil {
		t.Fatalf("seed 800 under cap: %v", err)
	}
	// Same-size re-deploy nets zero: 800 - 800 + 800 = 800 <= 1000 -> ok.
	if err := sr.ReplaceWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 800, "v2"), 800, cap, fixedNow); err != nil {
		t.Fatalf("same-size re-deploy should net zero: %v", err)
	}
	// A LARGER re-deploy that would breach the cap is rejected: 800 - 800 +
	// 1100 = 1100 > 1000. The old bytes are credited and the new charged.
	if err := sr.ReplaceWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 1100, "v3"), 1100, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap re-deploy: got %v, want ErrOverUserQuota", err)
	}
	// The rejected replace left the row at v2/800 (no partial mutation).
	used, err := sr.SumActiveBytesByOwner("key:rq", fixedNow)
	if err != nil {
		t.Fatalf("sum after rejected replace: %v", err)
	}
	if used != 800 {
		t.Fatalf("rejected over-cap replace must not change bytes: got %d, want 800", used)
	}
	// A SMALLER re-deploy frees the diff: 800 - 800 + 300 = 300, then a fresh
	// 600-byte site fits (300 + 600 = 900 <= 1000) where it would NOT have at
	// the original 800 (800 + 600 = 1400 > 1000).
	if err := sr.ReplaceWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 300, "v4"), 300, cap, fixedNow); err != nil {
		t.Fatalf("smaller re-deploy should free the diff: %v", err)
	}
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOfV("rq223456", "key:rq", 600, "v1"), 600, cap, fixedNow); err != nil {
		t.Fatalf("follow-up 600 should fit after the shrink (300+600=900): %v", err)
	}
}

// conformSiteReplaceRevivesExpiredChargesFull pins that re-deploying an
// EXPIRED-but-unswept site charges the FULL new size, never a delta that
// credits the expired old row. Because the scan filters expires_at > now, an
// expired site is NOT in the owner's `used`; crediting its old bytes against
// the replace delta would DOUBLE-SUBTRACT and admit an over-cap re-deploy,
// leaving a live site durably over the cap. Every backend gates the old-bytes
// credit on the old row being LIVE. docs/SPEC.md "Reviving an
// expired-but-unswept record charges its FULL post-revival size".
func conformSiteReplaceRevivesExpiredChargesFull(t *testing.T, sr conformanceSiteRepo) {
	const cap = 1000
	const slug = "revx1234"
	// Deploy a 900-byte site that expires in an hour.
	v1 := siteOfV(slug, "key:revx", 900, "v1")
	v1.ExpiresAt = fixedNow.Add(time.Hour)
	if err := sr.InsertWithQuotaCheck(context.Background(), v1, 900, cap, fixedNow); err != nil {
		t.Fatalf("seed 900 under cap: %v", err)
	}

	// Past expiry, before any sweep: the site is expired-but-unswept, so its
	// bytes no longer count (read-time exclusion). The owner's site sum is 0.
	after := fixedNow.Add(2 * time.Hour)
	if used, err := sr.SumActiveBytesByOwner("key:revx", after); err != nil {
		t.Fatalf("sum after expiry: %v", err)
	} else if used != 0 {
		t.Fatalf("expired-unswept site must not count toward quota: got %d, want 0", used)
	}

	// Re-deploy the SAME slug at 1500 bytes (over the 1000 cap). A delta credit
	// of the EXPIRED old row would compute used(0) - old(900) + new(1500) = 600
	// <= 1000 and WRONGLY ADMIT, resurrecting a live 1500-byte site over a 1000
	// cap. The correct behavior does NOT credit the expired old bytes, charging
	// the full new size: 0 + 1500 = 1500 > 1000 -> reject.
	over := siteOfV(slug, "key:revx", 1500, "v2")
	over.CreatedAt = fixedNow
	over.UpdatedAt = after
	over.ExpiresAt = after.Add(domain.DefaultRetentionWindow)
	if err := sr.ReplaceWithQuotaCheck(context.Background(), over, 1500, cap, after); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("reviving an expired site OVER the cap must be rejected (full new size charged, expired old bytes NOT credited): got %v, want ErrOverUserQuota", err)
	}
	// The rejected replace left the site untouched (still expired v1/900).
	if used, err := sr.SumActiveBytesByOwner("key:revx", after); err != nil {
		t.Fatalf("sum after rejected revive: %v", err)
	} else if used != 0 {
		t.Fatalf("rejected over-cap revive must not mutate the row: got %d, want 0 (still expired)", used)
	}

	// A revive that FITS the cap succeeds and charges the full new size: replace
	// the expired 900 with 800. If the expired old bytes were (wrongly) counted
	// the post-revival total would be 900+800 and the point would be moot; the
	// correct math is simply 0 + 800 = 800 <= 1000 -> admit, and the revived
	// site now counts its full new size.
	fit := siteOfV(slug, "key:revx", 800, "v3")
	fit.CreatedAt = fixedNow
	fit.UpdatedAt = after
	fit.ExpiresAt = after.Add(domain.DefaultRetentionWindow)
	if err := sr.ReplaceWithQuotaCheck(context.Background(), fit, 800, cap, after); err != nil {
		t.Fatalf("reviving an expired site WITHIN the cap should succeed (full new size 800 <= 1000): %v", err)
	}
	if used, err := sr.SumActiveBytesByOwner("key:revx", after); err != nil {
		t.Fatalf("sum after revive: %v", err)
	} else if used != 800 {
		t.Fatalf("revived site must count its full new size: got %d, want 800", used)
	}
}

// conformSiteReplaceRestartsExpiry pins that a re-deploy re-keys the expiry
// index: after a replace at a later clock, the site is NOT reported expired at
// a cutoff the ORIGINAL expires_at would have crossed, and the OLD expiry key
// no longer fires the sweep.
func conformSiteReplaceRestartsExpiry(t *testing.T, sr conformanceSiteRepo) {
	const slug = "re123456"
	v1 := siteOfV(slug, "key:re", 100, "v1")
	v1.ExpiresAt = fixedNow.Add(time.Hour) // expires soon
	insertSite(t, sr, v1)

	// Re-deploy at +30m, pushing expiry a full retention window past that.
	at := fixedNow.Add(30 * time.Minute)
	v2 := siteOfV(slug, "key:re", 100, "v2")
	v2.CreatedAt = fixedNow
	v2.UpdatedAt = at
	v2.ExpiresAt = at.Add(domain.DefaultRetentionWindow)
	if err := sr.ReplaceWithQuotaCheck(context.Background(), v2, v2.Manifest.DedupedSize(), 0, at); err != nil {
		t.Fatalf("replace restarting expiry: %v", err)
	}

	// At +2h (past the ORIGINAL +1h expiry, before the new one) the site must
	// NOT be swept: the old expiry key was re-keyed, not left dangling.
	cutoff := fixedNow.Add(2 * time.Hour)
	refs, err := sr.ExpiredSites(cutoff)
	if err != nil {
		t.Fatalf("expired sites: %v", err)
	}
	if siteRefsHaveSlug(refs, slug) {
		t.Fatalf("re-deployed site must NOT be expired at %v (expiry restarted): got %v", cutoff, refs)
	}
}

// siteRefsHaveSlug reports whether any expired-site reference names slug.
func siteRefsHaveSlug(refs []domain.ExpiredSite, slug string) bool {
	for _, ref := range refs {
		if ref.Slug.String() == slug {
			return true
		}
	}
	return false
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
		ExpiresAt: fixedNow.Add(domain.DefaultRetentionWindow),
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
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq123456", "key:sq", 600), 600, cap, fixedNow); err != nil {
		t.Fatalf("first site (600 under 1000): %v", err)
	}
	// Second would be 600+500=1100 > 1000 -> reject.
	err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq223456", "key:sq", 500), 500, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap site deploy: got %v, want ErrOverUserQuota", err)
	}
	// A smaller one that keeps the sum under cap succeeds.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq323456", "key:sq", 300), 300, cap, fixedNow); err != nil {
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
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb1site1", "key:pb1", 800), 800, cap, fixedNow); err != nil {
		t.Fatalf("site 800 under cap: %v", err)
	}
	// 800 (site) + 300 (paste) = 1100 > 1000 -> the paste MUST be rejected.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst1", "key:pb1", 300), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste over combined cap (site bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// A paste that keeps the combined total at/under cap fits: 800+200=1000.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst2", "key:pb1", 200), cap, fixedNow); err != nil {
		t.Fatalf("paste within combined cap (800+200=1000): %v", err)
	}
	// And now the owner is full: another byte is rejected.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst3", "key:pb1", 1), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste at full combined cap should be rejected: got %v", err)
	}

	// Direction 2 (the reverse): a PASTE fills most of the cap, then a SITE
	// that would overflow the COMBINED total is rejected. This direction was
	// already correct (the site path summed both), pinned here for symmetry.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb2pst1", "key:pb2", 800), cap, fixedNow); err != nil {
		t.Fatalf("paste 800 under cap: %v", err)
	}
	// 800 (paste) + 300 (site) = 1100 > 1000 -> the site MUST be rejected.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb2site1", "key:pb2", 300), 300, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("site over combined cap (paste bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// A site that keeps the combined total at/under cap fits: 800+200=1000.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb2site2", "key:pb2", 200), 200, cap, fixedNow); err != nil {
		t.Fatalf("site within combined cap (800+200=1000): %v", err)
	}

	// An append also counts site bytes: the owner is at cap, so any append is
	// rejected (covers the AppendVersionWithQuotaCheck cross-kind path).
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "pb2pst1", domain.KindHTML, "sha-pb2-v2", 1, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
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
// if each kind alone held its ceiling. All three backends hold it: sqlite
// (serializable insert tx, one identityActiveBytes spanning both kinds),
// shale (both reserve steps CAS the same {id} shard, each reading both
// counters), and slatedb (the per-identity lockQuota stripe shared by the
// paste and site inserts spans both kinds), so all three assert the ceiling.
func conformSitePerOwnerCapConcurrentCeiling(t *testing.T, caps conformCaps, r conformanceRepo, sr conformanceSiteRepo) {
	const (
		body = 100
		k    = 3
		n    = 8
	)
	cap := int64(k * body) // admits exactly k records of `body`, any mix of kinds
	var landed int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Alternate kinds so the race is genuinely cross-kind: even i ->
			// paste, odd i -> site, all for the same owner "key:ccx".
			var err error
			if i%2 == 0 {
				err = r.InsertWithQuotaCheck(context.Background(), pasteOf(fmt.Sprintf("ccp%05d", i), "key:ccx", body), cap, fixedNow)
			} else {
				err = sr.InsertWithQuotaCheck(context.Background(), siteOf(fmt.Sprintf("ccs%05d", i), "key:ccx", body), body, cap, fixedNow)
			}
			if err == nil {
				atomic.AddInt64(&landed, 1)
			}
		}(i)
	}
	wg.Wait()
	if !caps.StrictIdentityQuotaUnderConcurrency {
		t.Logf("backend does not guarantee strict cross-kind per-identity quota under concurrency (scan-based over-admit): %d records x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
		return
	}
	if landed*body > cap {
		t.Fatalf("combined per-owner quota ceiling breached under cross-kind concurrency: %d records x %dB = %dB landed, cap %dB",
			landed, body, landed*body, cap)
	}
}

// conformSiteSlugCollisionVsPaste pins the both-directions slug collision:
// a slug is EITHER a site or a paste, never both.
func conformSiteSlugCollisionVsPaste(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// A paste owns "col12345"; a site deploy onto the same slug is rejected.
	insert(t, r, pasteOf("col12345", "key:c", 10))
	err := sr.InsertWithQuotaCheck(context.Background(), siteOf("col12345", "key:c", 10), 10, 0, fixedNow)
	if err == nil {
		t.Fatalf("site deploy onto a paste's slug must be rejected")
	}
	if !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("site-vs-paste collision error must be storage.ErrSlugTaken (errors.Is), got %v", err)
	}

	// A site owns "col22345"; a paste insert onto the same slug is rejected.
	insertSite(t, sr, siteOf("col22345", "key:c", 10))
	perr := r.InsertWithQuotaCheck(context.Background(), pasteOf("col22345", "key:c", 10), 0, fixedNow)
	if perr == nil {
		t.Fatalf("paste insert onto a site's slug must be rejected")
	}
	if !errors.Is(perr, storage.ErrSlugTaken) {
		t.Fatalf("paste-vs-site collision error must be storage.ErrSlugTaken (errors.Is), got %v", perr)
	}

	// Two sites cannot share a slug either.
	insertSite(t, sr, siteOf("col32345", "key:c", 10))
	derr := sr.InsertWithQuotaCheck(context.Background(), siteOf("col32345", "key:c", 10), 10, 0, fixedNow)
	if derr == nil {
		t.Fatalf("duplicate site slug must be rejected")
	}
	if !errors.Is(derr, storage.ErrSlugTaken) {
		t.Fatalf("duplicate-site-slug error must be storage.ErrSlugTaken (errors.Is), got %v", derr)
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
	refs, err := sr.ExpiredSites(at)
	if err != nil {
		t.Fatalf("expired sites: %v", err)
	}
	if !siteRefsHaveSlug(refs, "se123456") {
		t.Fatalf("se123456 should be expired at %v, got %v", at, refs)
	}
	if siteRefsHaveSlug(refs, "se223456") {
		t.Fatalf("se223456 should NOT be expired at %v, got %v", at, refs)
	}
	// Inclusive boundary: expires_at == now counts as expired.
	atBoundary := fixedNow.Add(time.Hour)
	refs, err = sr.ExpiredSites(atBoundary)
	if err != nil {
		t.Fatalf("expired sites at boundary: %v", err)
	}
	if !siteRefsHaveSlug(refs, "se123456") {
		t.Fatalf("expires_at == now should be inclusive-expired, got %v", refs)
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

// conformDeleteExpiredSite pins the site half of the expiry-pass delete
// contract (docs/SPEC.md "Static-site storage", sweep path), mirroring the
// paste conformDeleteExpired: processing a reference the scan surfaced
// deletes the site record (reporting true), leaves not-yet-expired sites
// untouched, and DRAINS the scan - a re-scan after the pass sees zero
// references, and a re-processed reference is an idempotent no-op
// reporting false (no record was deleted by it).
func conformDeleteExpiredSite(t *testing.T, sr conformanceSiteRepo) {
	dead := siteOf("ds123456", "key:ds", 100)
	dead.ExpiresAt = fixedNow.Add(time.Hour)
	insertSite(t, sr, dead)

	alive := siteOf("ds223456", "key:ds", 100)
	alive.ExpiresAt = fixedNow.Add(48 * time.Hour)
	insertSite(t, sr, alive)

	at := fixedNow.Add(2 * time.Hour)
	refs, err := sr.ExpiredSites(at)
	if err != nil {
		t.Fatalf("expired sites: %v", err)
	}
	if len(refs) != 1 || refs[0].Slug != "ds123456" {
		t.Fatalf("only ds123456 should be expired at %v, got %v", at, refs)
	}

	// Processing the reference deletes the record and reports true.
	deleted, err := sr.DeleteExpiredSite(refs[0])
	if err != nil {
		t.Fatalf("delete expired site: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteExpiredSite must report true for a live site record")
	}
	if _, err := sr.Get("ds123456"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired site should be gone after DeleteExpiredSite: %v", err)
	}

	// One pass drains what it scanned: a re-scan sees zero references.
	again, err := sr.ExpiredSites(at)
	if err != nil {
		t.Fatalf("expired sites (re-scan): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-scan after the pass must see zero expired site references, got %v", again)
	}

	// Re-processing the same reference is an idempotent no-op reporting
	// false (the sweep's deleted-count only reflects real record deletions).
	deleted, err = sr.DeleteExpiredSite(refs[0])
	if err != nil {
		t.Fatalf("re-processed site reference must no-op, got: %v", err)
	}
	if deleted {
		t.Fatalf("DeleteExpiredSite must report false when the site record was already gone")
	}

	// The not-yet-expired site is untouched throughout.
	if _, err := sr.Get("ds223456"); err != nil {
		t.Fatalf("active site must survive the expiry pass: %v", err)
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
	refs, err := sr.ExpiredSites(atStart)
	if err != nil {
		t.Fatalf("expired sites at .0s: %v", err)
	}
	if siteRefsHaveSlug(refs, "essLate1") {
		t.Fatalf("site expiring at .5s must NOT be expired at a .0s cutoff (sub-second ordering bug), got %v", refs)
	}
	if !siteRefsHaveSlug(refs, "essEarly") {
		t.Fatalf("site expiring at .0s should be inclusive-expired at a .0s cutoff, got %v", refs)
	}

	// Cutoff at .5s: now BOTH are expired (the .5s site is at the inclusive
	// boundary, the .0s site is past it).
	atHalf := base.Add(500 * time.Millisecond) // 12:00:00.5
	refs, err = sr.ExpiredSites(atHalf)
	if err != nil {
		t.Fatalf("expired sites at .5s: %v", err)
	}
	if !siteRefsHaveSlug(refs, "essLate1") {
		t.Fatalf("site expiring at .5s should be inclusive-expired at a .5s cutoff, got %v", refs)
	}
	if !siteRefsHaveSlug(refs, "essEarly") {
		t.Fatalf("site expiring at .0s should still be expired at a .5s cutoff, got %v", refs)
	}

	// And a cutoff just BELOW the .5s site (e.g. .4s) leaves it unexpired,
	// proving the boundary is real sub-second time, not whole-second rounding.
	atBelow := base.Add(400 * time.Millisecond) // 12:00:00.4
	refs, err = sr.ExpiredSites(atBelow)
	if err != nil {
		t.Fatalf("expired sites at .4s: %v", err)
	}
	if siteRefsHaveSlug(refs, "essLate1") {
		t.Fatalf("site expiring at .5s must NOT be expired at a .4s cutoff, got %v", refs)
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
		CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.DefaultRetentionWindow),
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
		CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.DefaultRetentionWindow),
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
