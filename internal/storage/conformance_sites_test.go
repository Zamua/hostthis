package storage_test

// Site operations in the backend-agnostic conformance suite: the OBSERVABLE
// static-site contract every site-supporting metadata backend must hold
// identically. Subtests run only when the backend's factory supplies a non-nil
// site repo (newSites).
//
// The site repo and the paste repo from one factory call MUST share the same
// backing store, so the cross-quota subtests (a site's bytes affect a paste's
// quota check and vice versa) and the cross-family slug-collision subtest
// exercise the real interaction, not two independent stores.

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

// conformanceSiteRepo is the union of the two site-side service interfaces a
// static-site backend must satisfy: deploy/read/per-owner byte sum, plus the
// sweep's delete and referenced-blob set.
type conformanceSiteRepo interface {
	service.SiteRepo
	service.SweepSites
}

// siteOf builds a Site with one slug-derived file, stamped at fixedNow with the
// size is that file's, and so the deduped, total.
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
	}
}

// siteOfV folds a version tag into the file's SHA, so two deploys of the SAME
// slug at different v produce DISTINCT manifests. That is what lets the replace
// conformance prove the row's manifest swaps, not just its timestamps.
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
	}
}

// insertSite deploys a site with no caps (caps=0 -> no quota enforcement).
func insertSite(t *testing.T, sr conformanceSiteRepo, s domain.Site) {
	t.Helper()
	if err := sr.InsertWithQuotaCheck(context.Background(), s, s.Manifest.DedupedSize(), 0, fixedNow); err != nil {
		t.Fatalf("insert site %q: %v", s.Slug, err)
	}
}

// runSiteConformance runs the site contract subtests. newSites must produce a
// FRESH paste+site pair sharing one backing store per subtest, or the
// empty-store assertions do not hold. caps declares the backend's by-design
// behavior exceptions.
func runSiteConformance(t *testing.T, name string, caps conformCaps, newSites func(t *testing.T) (conformanceRepo, conformanceSiteRepo)) {
	t.Helper()
	t.Run(name+"/Sites/DeployAndReadBack", func(t *testing.T) { _, sr := newSites(t); conformSiteDeployAndReadBack(t, sr) })
	t.Run(name+"/Sites/GetNotFound", func(t *testing.T) { _, sr := newSites(t); conformSiteGetNotFound(t, sr) })
	t.Run(name+"/Sites/SumByIdentity", func(t *testing.T) { _, sr := newSites(t); conformSiteSumByIdentity(t, sr) })
	t.Run(name+"/Sites/QuotaCountsSiteBytes", func(t *testing.T) { _, sr := newSites(t); conformSiteQuotaCountsSiteBytes(t, sr) })
	t.Run(name+"/Sites/PerOwnerCapCountsBoth", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapCountsBoth(t, r, sr) })
	t.Run(name+"/Sites/PerOwnerCapConcurrentCeiling", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapConcurrentCeiling(t, caps, r, sr) })
	t.Run(name+"/Sites/SlugCollisionVsPaste", func(t *testing.T) { r, sr := newSites(t); conformSiteSlugCollisionVsPaste(t, r, sr) })
	t.Run(name+"/Sites/ReferencedBlobSHAs", func(t *testing.T) { _, sr := newSites(t); conformSiteReferencedBlobSHAs(t, sr) })
	t.Run(name+"/Sites/DedupedSizeCharged", func(t *testing.T) { _, sr := newSites(t); conformSiteDedupedSizeCharged(t, sr) })
	t.Run(name+"/Sites/ReplaceInPlace", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceInPlace(t, sr) })
	t.Run(name+"/Sites/ReplaceNotFoundShape", func(t *testing.T) { r, sr := newSites(t); conformSiteReplaceNotFoundShape(t, r, sr) })
	t.Run(name+"/Sites/ReplaceDeltaQuota", func(t *testing.T) { _, sr := newSites(t); conformSiteReplaceDeltaQuota(t, sr) })
	t.Run(name+"/Sites/ListByOwner", func(t *testing.T) { _, sr := newSites(t); conformSiteListByOwner(t, sr) })
}

// conformSiteListByOwner pins ListSitesByOwner: exactly the owner's active
// sites, never another owner's, tracking the enumeration index across the
// deploy/delete lifecycle. It is what makes sites visible in `ssh <apex> list`,
// so the shared quota stays legible and reclaimable.
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

	// A delete must drop out of the listing, leaving A's other site and B
	// untouched.
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
// owned site swaps the manifest while keeping the slug and created_at stable,
// so rollback and history ride the same identity.
func conformSiteReplaceInPlace(t *testing.T, sr conformanceSiteRepo) {
	const slug = "rp123456"
	v1 := siteOfV(slug, "key:rp", 100, "v1")
	insertSite(t, sr, v1)

	later := fixedNow.Add(2 * time.Hour)
	v2 := siteOfV(slug, "key:rp", 250, "v2")
	v2.CreatedAt = later // a hostile caller can't move created_at via the row
	v2.UpdatedAt = later
	if err := sr.ReplaceWithQuotaCheck(context.Background(), v2, v2.Manifest.DedupedSize(), 0, later); err != nil {
		t.Fatalf("replace in place: %v", err)
	}

	got, err := sr.Get(slug)
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	e, ok := got.Manifest.Files["index.html"]
	if !ok || e.SHA != "sha-"+slug+"-v2" || e.Size != 250 {
		t.Fatalf("manifest not swapped: got %+v", got.Manifest.Files)
	}
	// created_at is the slug's birth time, unchanged by the re-deploy.
	if !got.CreatedAt.Equal(fixedNow) {
		t.Fatalf("created_at must be stable across re-deploy: got %v, want %v", got.CreatedAt, fixedNow)
	}
	// updated_at restarts from the re-deploy.
	if !got.UpdatedAt.Equal(later) {
		t.Fatalf("updated_at should be the re-deploy time: got %v, want %v", got.UpdatedAt, later)
	}
	// The owner's live site bytes are the NEW size only, not 100+250.
	used, err := sr.SumActiveBytesByOwner("key:rp", later)
	if err != nil {
		t.Fatalf("sum after replace: %v", err)
	}
	if used != 250 {
		t.Fatalf("replace must not double-count: owner sum got %d, want 250", used)
	}
}

// conformSiteReplaceNotFoundShape pins that a replace the identity may not
// perform collapses to ErrNotFound, the SAME sentinel a missing slug yields, so
// neither existence nor ownership leaks: foreign-owned, missing, and
// paste-only slugs are indistinguishable.
func conformSiteReplaceNotFoundShape(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// Missing slug: never deployed.
	miss := siteOfV("rmiss123", "key:owner", 50, "v1")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), miss, miss.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace of missing slug: got %v, want ErrNotFound", err)
	}

	// Foreign-owned: alice's site, replaced as mallory.
	insertSite(t, sr, siteOfV("rfor1234", "key:alice", 100, "v1"))
	foreign := siteOfV("rfor1234", "key:mallory", 100, "v2")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), foreign, foreign.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace of foreign-owned site: got %v, want ErrNotFound (no ownership leak)", err)
	}
	// The rejected replace left the owner's row untouched.
	got, err := sr.Get("rfor1234")
	if err != nil {
		t.Fatalf("owner's site after rejected foreign replace: %v", err)
	}
	if e := got.Manifest.Files["index.html"]; e.SHA != "sha-rfor1234-v1" {
		t.Fatalf("foreign replace must not mutate the owner's row: got %+v", got.Manifest.Files)
	}

	// A slug that exists only as a PASTE: a site replace must not find it.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("rpaste12", "key:owner", 10), 0, fixedNow); err != nil {
		t.Fatalf("seed paste: %v", err)
	}
	asPaste := siteOfV("rpaste12", "key:owner", 10, "v1")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), asPaste, asPaste.Manifest.DedupedSize(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace targeting a paste-only slug: got %v, want ErrNotFound", err)
	}
}

// conformSiteReplaceDeltaQuota pins that a replace charges the DELTA (old bytes
// credited, new bytes charged), not the full new size: same-size nets zero,
// smaller frees the difference, larger is rejected when the post-swap total
// would breach the cap.
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
	// A LARGER re-deploy breaches the cap: 800 - 800 + 1100 = 1100 > 1000.
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

// conformSiteDeployAndReadBack pins that a multi-file manifest round-trips
// through the backend's encoding identically: sha, size and content-type per
// path, plus the site's timestamps.
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
	}
	insertSite(t, sr, s)

	got, err := sr.Get(s.Slug)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	if got.Slug != s.Slug || got.Identity != s.Identity {
		t.Fatalf("site round-trip mismatch: got slug=%q id=%q", got.Slug, got.Identity)
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
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq123456", "key:sq", 600), 600, cap, fixedNow); err != nil {
		t.Fatalf("first site (600 under 1000): %v", err)
	}
	// 600+500=1100 > 1000 -> reject.
	err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq223456", "key:sq", 500), 500, cap, fixedNow)
	if !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap site deploy: got %v, want ErrOverUserQuota", err)
	}
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("sq323456", "key:sq", 300), 300, cap, fixedNow); err != nil {
		t.Fatalf("site within cap (600+300=900): %v", err)
	}
}

// conformSitePerOwnerCapCountsBoth pins the cross-kind PER-OWNER cap in BOTH
// directions: a deploy of either kind is rejected when the owner's COMBINED
// paste+site bytes plus the new body exceed userCap. The asymmetry this guards
// against is a paste check that sees only paste bytes, which admits an
// 800-byte site plus a 300-byte paste under a 1000-byte cap.
func conformSitePerOwnerCapCountsBoth(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	const cap = 1000

	// Direction 1: a SITE fills most of the cap, then a PASTE that would
	// overflow the COMBINED total is rejected.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb1site1", "key:pb1", 800), 800, cap, fixedNow); err != nil {
		t.Fatalf("site 800 under cap: %v", err)
	}
	// 800 (site) + 300 (paste) = 1100 > 1000 -> the paste MUST be rejected.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst1", "key:pb1", 300), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste over combined cap (site bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// At/under cap fits: 800+200=1000.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst2", "key:pb1", 200), cap, fixedNow); err != nil {
		t.Fatalf("paste within combined cap (800+200=1000): %v", err)
	}
	// The owner is now full: another byte is rejected.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb1pst3", "key:pb1", 1), cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("paste at full combined cap should be rejected: got %v", err)
	}

	// Direction 2: a PASTE fills most of the cap, then a SITE that would
	// overflow the COMBINED total is rejected.
	if err := r.InsertWithQuotaCheck(context.Background(), pasteOf("pb2pst1", "key:pb2", 800), cap, fixedNow); err != nil {
		t.Fatalf("paste 800 under cap: %v", err)
	}
	// 800 (paste) + 300 (site) = 1100 > 1000 -> the site MUST be rejected.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb2site1", "key:pb2", 300), 300, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("site over combined cap (paste bytes must count): got %v, want ErrOverUserQuota", err)
	}
	// At/under cap fits: 800+200=1000.
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOf("pb2site2", "key:pb2", 200), 200, cap, fixedNow); err != nil {
		t.Fatalf("site within combined cap (800+200=1000): %v", err)
	}

	// The append path counts site bytes too: at cap, any append is rejected.
	if _, err := r.AppendVersionWithQuotaCheck(context.Background(), "pb2pst1", domain.KindHTML, "sha-pb2-v2", 1, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("append at full combined cap should be rejected (site bytes must count): got %v", err)
	}
}

// conformSitePerOwnerCapConcurrentCeiling pins the COMBINED per-owner ceiling
// under concurrent CROSS-KIND deploys: however the interleaving falls, the
// bytes that land never exceed the cap. A backend that checked paste and site
// quota on separate, non-serialized paths could overshoot the combined total
// while each kind alone held its ceiling. Each backend serializes both kinds on
// one thing: sqlite a serializable tx, shale the same {id} shard CAS, slatedb
// the per-identity lockQuota stripe.
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
			// Alternate kinds so the race is genuinely cross-kind, with
			// every record under the same owner.
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

// conformSiteSlugCollisionVsPaste pins that a slug is EITHER a site or a paste,
// never both, in both directions, and that the refusal is ErrSlugTaken.
func conformSiteSlugCollisionVsPaste(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// A site deploy onto a paste's slug.
	insert(t, r, pasteOf("col12345", "key:c", 10))
	err := sr.InsertWithQuotaCheck(context.Background(), siteOf("col12345", "key:c", 10), 10, 0, fixedNow)
	if err == nil {
		t.Fatalf("site deploy onto a paste's slug must be rejected")
	}
	if !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("site-vs-paste collision error must be storage.ErrSlugTaken (errors.Is), got %v", err)
	}

	// A paste insert onto a site's slug.
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

// conformSiteReferencedBlobSHAs pins the site-side referenced-blob set the
// sweep unions into its keep-alive set, including its deduplication.
func conformSiteReferencedBlobSHAs(t *testing.T, sr conformanceSiteRepo) {
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
		CreatedAt: fixedNow, UpdatedAt: fixedNow}
	insertSite(t, sr, s)

	refs, err = sr.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas: %v", err)
	}
	if !sliceHas(refs, "sha-ref-index") || !sliceHas(refs, "sha-ref-js") {
		t.Fatalf("both distinct site blob shas should be referenced, got %v", refs)
	}
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

// conformSiteDedupedSizeCharged pins that quota is charged the DEDUPED size
// (distinct blobs), not the sum over all paths.
func conformSiteDedupedSizeCharged(t *testing.T, sr conformanceSiteRepo) {
	man := domain.NewManifest()
	man.Add("a.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("b.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("c.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	s := domain.Site{
		Slug: "dd123456", Identity: "key:dd", Manifest: man,
		CreatedAt: fixedNow, UpdatedAt: fixedNow}
	// Three paths, one distinct blob: 400, not 1200.
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
