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
	if err := sr.InsertWithQuotaCheck(context.Background(), s, s.Manifest.Size(), 0, fixedNow); err != nil {
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
	t.Run(name+"/Sites/SumByIdentity", func(t *testing.T) { r, sr := newSites(t); conformSiteSumByIdentity(t, r, sr) })
	t.Run(name+"/Sites/QuotaCountsSiteBytes", func(t *testing.T) { _, sr := newSites(t); conformSiteQuotaCountsSiteBytes(t, sr) })
	t.Run(name+"/Sites/PerOwnerCapCountsBoth", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapCountsBoth(t, r, sr) })
	t.Run(name+"/Sites/PerOwnerCapConcurrentCeiling", func(t *testing.T) { r, sr := newSites(t); conformSitePerOwnerCapConcurrentCeiling(t, caps, r, sr) })
	t.Run(name+"/Sites/SlugCollisionVsPaste", func(t *testing.T) { r, sr := newSites(t); conformSiteSlugCollisionVsPaste(t, r, sr) })
	t.Run(name+"/Sites/EveryPathCharged", func(t *testing.T) { r, sr := newSites(t); conformSiteEveryPathCharged(t, r, sr) })
	t.Run(name+"/Sites/ReplaceInPlace", func(t *testing.T) { r, sr := newSites(t); conformSiteReplaceInPlace(t, r, sr) })
	t.Run(name+"/Sites/ReplaceNotFoundShape", func(t *testing.T) { r, sr := newSites(t); conformSiteReplaceNotFoundShape(t, r, sr) })
	t.Run(name+"/Sites/ReplaceChargesEachVersion", func(t *testing.T) { r, sr := newSites(t); conformSiteReplaceChargesEachVersion(t, r, sr) })
	t.Run(name+"/Sites/ListByOwner", func(t *testing.T) { r, sr := newSites(t); conformSiteListByOwner(t, r, sr) })
}

// conformSiteListByOwner pins the owner's listing: exactly their active
// directories, never another owner's, tracked across the deploy/delete
// lifecycle. It is what makes a directory visible in `ssh <apex> list`, so the
// shared quota stays legible and reclaimable.
//
// Read through the ARTIFACT listing, because that is where a directory lives:
// there is no second enumeration index to consult.
func conformSiteListByOwner(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	ownerA, ownerB := "key:AAAA", "key:BBBB"
	insertSite(t, sr, siteOf("aone1111", ownerA, 100))
	insertSite(t, sr, siteOf("atwo2222", ownerA, 200))
	insertSite(t, sr, siteOf("bone3333", ownerB, 300))

	slugsOf := func(owner string) map[string]bool {
		t.Helper()
		got, err := r.ListByOwner(owner)
		if err != nil {
			t.Fatalf("list %s: %v", owner, err)
		}
		m := make(map[string]bool, len(got))
		for _, p := range got {
			if p.Kind != domain.KindSite {
				continue
			}
			if string(p.Identity) != owner {
				t.Fatalf("owner leak: %s listed under %s", p.Identity, owner)
			}
			m[string(p.Slug)] = true
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
	if err := sr.Delete("aone1111", "key:AAAA", fixedNow); err != nil {
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
func conformSiteReplaceInPlace(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	const slug = "rp123456"
	v1 := siteOfV(slug, "key:rp", 100, "v1")
	insertSite(t, sr, v1)

	later := fixedNow.Add(2 * time.Hour)
	v2 := siteOfV(slug, "key:rp", 250, "v2")
	v2.CreatedAt = later // a hostile caller can't move created_at via the row
	v2.UpdatedAt = later
	if err := sr.ReplaceWithQuotaCheck(context.Background(), v2, v2.Manifest.Size(), 0, later); err != nil {
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
	// BOTH versions are charged. A re-deploy appends rather than replacing, so
	// the prior version stays live and rollable-back, and its bytes are still
	// on disk: 100 + 250. This is the contract version retention chose - the
	// old family credited the displaced bytes because it destroyed them.
	used, err := r.SumActiveBytesByOwner("key:rp", later)
	if err != nil {
		t.Fatalf("sum after replace: %v", err)
	}
	if used != 350 {
		t.Fatalf("both live versions must be charged: owner sum got %d, want 350", used)
	}
}

// conformSiteReplaceNotFoundShape pins that a replace the identity may not
// perform collapses to ErrNotFound, the SAME sentinel a missing slug yields, so
// neither existence nor ownership leaks: foreign-owned, missing, and
// paste-only slugs are indistinguishable.
func conformSiteReplaceNotFoundShape(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	// Missing slug: never deployed.
	miss := siteOfV("rmiss123", "key:owner", 50, "v1")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), miss, miss.Manifest.Size(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace of missing slug: got %v, want ErrNotFound", err)
	}

	// Foreign-owned: alice's site, replaced as mallory.
	insertSite(t, sr, siteOfV("rfor1234", "key:alice", 100, "v1"))
	foreign := siteOfV("rfor1234", "key:mallory", 100, "v2")
	if err := sr.ReplaceWithQuotaCheck(context.Background(), foreign, foreign.Manifest.Size(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
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
	if err := sr.ReplaceWithQuotaCheck(context.Background(), asPaste, asPaste.Manifest.Size(), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace targeting a paste-only slug: got %v, want ErrNotFound", err)
	}
}

// conformSiteReplaceChargesEachVersion pins what version retention costs: a
// re-deploy APPENDS, so the prior version stays live and its bytes stay on
// disk, and the cap is enforced against the running total rather than against
// the newest version alone.
//
// This replaces an older contract where a re-deploy credited the bytes it
// displaced. That was correct when a re-deploy destroyed the old manifest; it
// is wrong now that the old version is still there to roll back to.
func conformSiteReplaceChargesEachVersion(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	const cap = 1000
	if err := sr.InsertWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 300, "v1"), 300, cap, fixedNow); err != nil {
		t.Fatalf("seed 300 under cap: %v", err)
	}
	// A re-deploy fits while the TOTAL fits: 300 + 300 <= 1000.
	if err := sr.ReplaceWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 300, "v2"), 300, cap, fixedNow); err != nil {
		t.Fatalf("re-deploy within cap: %v", err)
	}
	used, err := r.SumActiveBytesByOwner("key:rq", fixedNow)
	if err != nil {
		t.Fatalf("sum after re-deploy: %v", err)
	}
	if used != 600 {
		t.Fatalf("both versions charged: got %d, want 600", used)
	}
	// And is refused once the total would breach it: 600 + 500 > 1000.
	if err := sr.ReplaceWithQuotaCheck(context.Background(), siteOfV("rq123456", "key:rq", 500, "v3"), 500, cap, fixedNow); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap re-deploy: got %v, want ErrOverUserQuota", err)
	}
	// The refusal left nothing behind.
	used, err = r.SumActiveBytesByOwner("key:rq", fixedNow)
	if err != nil {
		t.Fatalf("sum after refusal: %v", err)
	}
	if used != 600 {
		t.Fatalf("a refused re-deploy must not change bytes: got %d, want 600", used)
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

func conformSiteSumByIdentity(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	insertSite(t, sr, siteOf("ss123456", "key:ss", 100))
	insertSite(t, sr, siteOf("ss223456", "key:ss", 250))
	// A different owner's site must not leak into the sum.
	insertSite(t, sr, siteOf("ss323456", "key:other", 500))

	used, err := r.SumActiveBytesByOwner("key:ss", fixedNow)
	if err != nil {
		t.Fatalf("sum active bytes: %v", err)
	}
	if used != 350 {
		t.Fatalf("sum active site bytes by owner: got %d, want 350", used)
	}
	// Unknown owner -> zero, no error.
	used, err = r.SumActiveBytesByOwner("key:nobody", fixedNow)
	if err != nil {
		t.Fatalf("sum active bytes (unknown): %v", err)
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
// one thing: slatedb a serializable tx, shale the same {id} shard CAS, slatedb
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

// conformSiteEveryPathCharged pins that quota counts every PATH, not every
// distinct hash.
//
// Nothing in the store deduplicates - a blob id is minted fresh per staged file
// - so three paths holding the same bytes are three objects on disk. Charging
// once would bill for a third of what was written.
func conformSiteEveryPathCharged(t *testing.T, r conformanceRepo, sr conformanceSiteRepo) {
	man := domain.NewManifest()
	man.Add("a.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("b.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	man.Add("c.html", domain.ManifestEntry{SHA: "sha-dd", Size: 400, ContentType: "text/html; charset=utf-8"})
	s := domain.Site{
		Slug: "dd123456", Identity: "key:dd", Manifest: man,
		CreatedAt: fixedNow, UpdatedAt: fixedNow}
	// Three paths of identical content: three copies stored, so 1200.
	if got := s.Manifest.Size(); got != 1200 {
		t.Fatalf("Size should be 1200 (three paths), got %d", got)
	}
	insertSite(t, sr, s)
	used, err := r.SumActiveBytesByOwner("key:dd", fixedNow)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if used != 1200 {
		t.Fatalf("every stored path charged: got %d, want 1200 (three paths, three objects)", used)
	}
}
