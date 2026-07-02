package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	crand "crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// deployFixture wires real sqlite repos + a real compressed blob store so
// the test exercises the actual untar → blob → manifest → persist path.
func deployFixture(t *testing.T) (*DeploySite, *storage.SiteRepo, *storage.CompressedBlobStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(disk)
	sites := storage.NewSiteRepo(db)
	pastes := storage.NewPasteRepo(db)
	d := NewDeploySite(sites, pastes, NewStandaloneBlobUnit(blobs))
	return d, sites, blobs
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("hdr %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("body %q: %v", name, err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// incompressible returns n bytes of random data. Quota-fill tests need it
// because sites now charge their COMPRESSED size: repeated bytes would
// compress to ~nothing and no longer near the cap, so a "fill the budget"
// fixture must be incompressible for its compressed charge to be ~n.
func incompressible(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return string(b)
}

func TestDeploySite_HappyPath(t *testing.T) {
	d, sites, blobs := deployFixture(t)
	arc := gzipTar(t, map[string]string{
		"index.html":    "<!doctype html><h1>hi</h1>",
		"css/style.css": "body{margin:0}",
		"app.js":        "console.log(1)",
	})
	res, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(res.Site.Manifest.Files) != 3 {
		t.Fatalf("manifest files: got %d, want 3", len(res.Site.Manifest.Files))
	}
	// Site persisted and readable.
	got, err := sites.Get(res.Site.Slug)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	// Blobs stored + readable + byte-exact.
	e := got.Manifest.Files["index.html"]
	body, err := blobs.Get(e.SHA)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if string(body) != "<!doctype html><h1>hi</h1>" {
		t.Fatalf("blob bytes mismatch: %q", body)
	}
}

func TestDeploySite_Delete(t *testing.T) {
	d, sites, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{"index.html": "<h1>hi</h1>"})
	res, err := d.Deploy(bytes.NewReader(arc), "key:owner")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := res.Site.Slug

	// A foreign identity collapses to ErrNotFound and leaves the site intact.
	if err := d.Delete(slug, "key:other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: got %v, want ErrNotFound", err)
	}
	if _, err := sites.Get(slug); err != nil {
		t.Fatalf("site should survive a foreign delete: %v", err)
	}
	// The owner deletes it.
	if err := d.Delete(slug, "key:owner"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := sites.Get(slug); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("site should be gone: got %v", err)
	}
	// A nonexistent slug is ErrNotFound; an empty owner is ErrEmptyOwner.
	if err := d.Delete(domain.NewRandomSlug(), "key:owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete: got %v, want ErrNotFound", err)
	}
	if err := d.Delete(slug, ""); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("empty owner: got %v, want ErrEmptyOwner", err)
	}
}

func TestDeploySite_RejectsNoWebContent(t *testing.T) {
	d, _, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{
		"data.json": "{}",
		"logo.png":  "\x89PNG",
	})
	_, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if !errors.Is(err, domain.ErrNoWebContent) {
		t.Fatalf("no web content: got %v, want ErrNoWebContent", err)
	}
}

func TestDeploySite_RejectsTraversal(t *testing.T) {
	d, _, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{
		"index.html":     "<h1>ok</h1>",
		"../escape.html": "<h1>bad</h1>",
	})
	_, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Fatalf("traversal: got %v, want ErrUnsafeArchive", err)
	}
}

func TestDeploySite_OverQuota(t *testing.T) {
	d, _, _ := deployFixture(t)
	// A single file larger than the per-identity cap (10 MiB) trips the
	// mid-untar decompression-bomb guard, surfaced as ErrOverQuota.
	big := bytes.Repeat([]byte("A"), int(domain.UserQuotaBytes)+1)
	arc := gzipTar(t, map[string]string{
		"index.html": string(big),
	})
	_, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if !errors.Is(err, ErrOverQuota) {
		t.Fatalf("over quota: got %v, want ErrOverQuota", err)
	}
}

// TestDeploySite_ChargesCompressedSize pins the fix: a site is charged its
// COMPRESSED (post-zstd) size against quota, matching how pastes charge - so a
// compressible site costs a fraction of its raw bytes, and the charged number
// equals the manifest's CompressedDedupedSize (not the uncompressed sum).
func TestDeploySite_ChargesCompressedSize(t *testing.T) {
	d, sites, _ := deployFixture(t)
	owner := "key:compress"
	// Highly compressible content: 200 KB of a repeated byte squashes tiny.
	raw := bytes.Repeat([]byte("A"), 200_000)
	r, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": string(raw)})), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	used, _ := sites.SumActiveBytesByOwner(owner, time.Now().UTC())
	compressed := int64(r.Site.Manifest.CompressedDedupedSize())
	uncompressed := int64(r.Site.Manifest.DedupedSize())
	if used != compressed {
		t.Fatalf("charge should equal compressed size: used %d, compressed %d", used, compressed)
	}
	if uncompressed != 200_000 {
		t.Fatalf("uncompressed manifest size should be the raw 200000, got %d", uncompressed)
	}
	// The whole point: compressed is a small fraction of uncompressed.
	if compressed >= uncompressed/10 {
		t.Fatalf("compressible site should charge <<10%% of raw: compressed %d vs uncompressed %d", compressed, uncompressed)
	}
}

func TestDeploySite_QuotaRespectsExistingUsage(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:test"
	// First deploy a site that uses most of the budget (incompressible so its
	// COMPRESSED charge is ~near-cap, not ~0). Leave ~20 KB headroom - enough
	// that zstd's small block-header expansion of random data can't overrun.
	nearCap := incompressible(t, int(domain.UserQuotaBytes)-20000)
	arc1 := gzipTar(t, map[string]string{"index.html": nearCap})
	if _, err := d.Deploy(bytes.NewReader(arc1), owner); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// A second deploy whose bytes exceed the ~20 KB remaining must be
	// rejected (combined-usage budget computed before the untar).
	arc2 := gzipTar(t, map[string]string{"index.html": incompressible(t, 30000)})
	_, err := d.Deploy(bytes.NewReader(arc2), owner)
	if !errors.Is(err, ErrOverQuota) {
		t.Fatalf("second deploy over remaining budget: got %v, want ErrOverQuota", err)
	}
}

func TestDeploySite_DedupesAcrossDeploys(t *testing.T) {
	d, _, blobs := deployFixture(t)
	shared := "<!doctype html><h1>shared</h1>"
	arc := gzipTar(t, map[string]string{"index.html": shared})
	r1, err := d.Deploy(bytes.NewReader(arc), "key:a")
	if err != nil {
		t.Fatalf("deploy 1: %v", err)
	}
	r2, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": shared})), "key:b")
	if err != nil {
		t.Fatalf("deploy 2: %v", err)
	}
	// Identical file content → identical blob SHA across two sites/owners.
	if r1.Site.Manifest.Files["index.html"].SHA != r2.Site.Manifest.Files["index.html"].SHA {
		t.Fatalf("identical content should share a blob SHA")
	}
	// And the blob is present once, readable.
	if _, err := blobs.Get(r1.Site.Manifest.Files["index.html"].SHA); err != nil {
		t.Fatalf("shared blob: %v", err)
	}
}

func TestDeploySite_EmptyOwner(t *testing.T) {
	d, _, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{"index.html": "<h1>x</h1>"})
	if _, err := d.Deploy(bytes.NewReader(arc), ""); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("empty owner: got %v, want ErrEmptyOwner", err)
	}
}

func TestDeploySite_NowInjectable(t *testing.T) {
	d, sites, _ := deployFixture(t)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return fixed }
	arc := gzipTar(t, map[string]string{"index.html": "<h1>x</h1>"})
	res, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	got, _ := sites.Get(res.Site.Slug)
	if !got.ExpiresAt.Equal(fixed.Add(domain.DefaultRetentionWindow)) {
		t.Fatalf("expiry: got %v, want %v", got.ExpiresAt, fixed.Add(domain.DefaultRetentionWindow))
	}
}

// TestDeployToSlug_ReplacesInPlace is the service-level pin for the bug
// fix: DeployToSlug re-deploys an existing OWNED site at the SAME slug.
// The slug + created_at are preserved, the manifest swaps to the new
// content, and the owner's live bytes reflect the NEW size only (the old
// bytes are freed in the same swap, not double-counted).
func TestDeployToSlug_ReplacesInPlace(t *testing.T) {
	d, sites, _ := deployFixture(t)
	owner := "key:rp"

	// Deploy v1 (one large incompressible file so its COMPRESSED charge
	// clearly exceeds the smaller v2 - with tiny/compressible content the
	// per-file zstd overhead would dominate and mask the shrink).
	v1 := gzipTar(t, map[string]string{"index.html": incompressible(t, 4000)})
	r1, err := d.Deploy(bytes.NewReader(v1), owner)
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	slug := r1.Site.Slug
	created := r1.Site.CreatedAt
	used1, _ := sites.SumActiveBytesByOwner(owner, time.Now().UTC())

	// Re-deploy v2 with DIFFERENT, smaller content to the SAME slug.
	v2 := gzipTar(t, map[string]string{
		"index.html": incompressible(t, 500),
		"about.html": "<h1>about</h1>",
	})
	r2, err := d.DeployToSlug(slug, bytes.NewReader(v2), owner)
	if err != nil {
		t.Fatalf("DeployToSlug should succeed for owned site: %v", err)
	}
	if r2.Site.Slug != slug {
		t.Fatalf("in-place update changed slug: got %q want %q", r2.Site.Slug, slug)
	}
	if !r2.Site.CreatedAt.Equal(created) {
		t.Fatalf("created_at must be preserved across re-deploy: got %v want %v", r2.Site.CreatedAt, created)
	}

	// The persisted manifest is v2's: about.html now present, index.html shrunk.
	got, err := sites.Get(slug)
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	if _, ok := got.Manifest.Files["about.html"]; !ok {
		t.Fatalf("v2 file about.html missing from manifest: %+v", got.Manifest.Files)
	}

	// Quota delta: the owner's bytes reflect v2's COMPRESSED deduped size
	// only (the quota basis), NOT v1+v2.
	used2, _ := sites.SumActiveBytesByOwner(owner, time.Now().UTC())
	wantV2 := int64(r2.Site.Manifest.CompressedDedupedSize())
	if used2 != wantV2 {
		t.Fatalf("replace must charge the new (compressed) size only: owner sum got %d, want %d (used1 was %d)", used2, wantV2, used1)
	}
	if used2 >= used1 {
		t.Fatalf("a smaller re-deploy must FREE bytes: used1 %d, used2 %d", used1, used2)
	}
}

// TestDeployToSlug_FreesOldChargesNewDelta pins the replace-delta quota
// math at the service layer: shrinking a near-cap site frees enough budget
// that a follow-up NEW deploy fits where it would have been rejected at the
// original size.
func TestDeployToSlug_FreesOldChargesNewDelta(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:delta"

	// A site near the cap (leave ~20 KB headroom). Incompressible so the
	// compressed charge actually nears the cap.
	nearCap := incompressible(t, int(domain.UserQuotaBytes)-20000)
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": nearCap})), owner)
	if err != nil {
		t.Fatalf("deploy near-cap site: %v", err)
	}

	// At the original size, a 30 KB NEW deploy would NOT fit (only ~20 KB free).
	tooBig := gzipTar(t, map[string]string{"index.html": incompressible(t, 30000)})
	if _, err := d.Deploy(bytes.NewReader(tooBig), owner); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("a 5000B new deploy should not fit at the original size: got %v", err)
	}

	// Shrink the existing site in place to a few bytes, freeing the budget.
	if _, err := d.DeployToSlug(r1.Site.Slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>tiny</h1>"})), owner); err != nil {
		t.Fatalf("shrink in place: %v", err)
	}

	// Now the same 5000-byte NEW deploy fits.
	if _, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("Z"), 5000))})), owner); err != nil {
		t.Fatalf("5000B new deploy should fit after the shrink freed budget: %v", err)
	}
}

// TestDeployToSlug_ExpiredOldRowNotCredited pins the replace-delta quota
// against an EXPIRED-but-unswept site. Re-deploying such a site resurrects
// it, but the old bytes are already excluded from the owner's active sum,
// so they must NOT be credited back: an over-cap re-deploy must be rejected
// exactly as a fresh deploy would be. Crediting the stale bytes would let
// the owner exceed the per-identity cap by the old site's size.
func TestDeployToSlug_ExpiredOldRowNotCredited(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:expired"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return base }

	// A modest site, comfortably under the cap.
	old := gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("A"), 4000))})
	r, err := d.Deploy(bytes.NewReader(old), owner)
	if err != nil {
		t.Fatalf("initial deploy: %v", err)
	}

	// Advance past the site's retention window: it is now expired but not
	// yet swept (still present, still owned, Get still returns it).
	d.Now = func() time.Time { return base.Add(domain.DefaultRetentionWindow + time.Hour) }

	// Re-deploy at a size OVER the cap. With the expired old bytes correctly
	// NOT credited, the budget is the full empty cap and a >cap archive is
	// rejected. The buggy path credited the 4000 stale bytes and admitted it.
	big := gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("B"), int(domain.UserQuotaBytes)+2000))})
	if _, err := d.DeployToSlug(r.Site.Slug, bytes.NewReader(big), owner); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("re-deploy of an EXPIRED site over the cap must be rejected (stale bytes must not be credited): got %v, want ErrOverQuota", err)
	}
}

// TestDeployToSlug_NonexistentSlug pins that re-deploying to a slug that
// was never deployed returns ErrNotFound (the same "service: not found"
// the SSH layer maps to exit 4) - never silently creating it.
func TestDeployToSlug_NonexistentSlug(t *testing.T) {
	d, _, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{"index.html": "<h1>x</h1>"})
	_, err := d.DeployToSlug(domain.Slug("nosuch12"), bytes.NewReader(arc), "key:test")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-deploy to a missing slug: got %v, want ErrNotFound", err)
	}
}

// TestDeployToSlug_ForeignSlug pins ownership at the service layer: a
// second identity re-deploying onto someone else's site gets ErrNotFound
// (no ownership/existence leak) and the original site is untouched.
func TestDeployToSlug_ForeignSlug(t *testing.T) {
	d, sites, _ := deployFixture(t)

	// alice deploys a site.
	r, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>alice</h1>"})), "key:alice")
	if err != nil {
		t.Fatalf("alice deploy: %v", err)
	}
	slug := r.Site.Slug
	aliceSHA := r.Site.Manifest.Files["index.html"].SHA

	// mallory tries to re-deploy onto alice's slug.
	_, err = d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>mallory</h1>"})), "key:mallory")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign re-deploy: got %v, want ErrNotFound (no ownership leak)", err)
	}

	// alice's site is byte-for-byte unchanged.
	got, err := sites.Get(slug)
	if err != nil {
		t.Fatalf("get alice's site: %v", err)
	}
	if got.Manifest.Files["index.html"].SHA != aliceSHA {
		t.Fatalf("rejected foreign re-deploy mutated the owner's manifest: %+v", got.Manifest.Files)
	}
}
