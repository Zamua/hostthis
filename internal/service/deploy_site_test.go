package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	d := NewDeploySite(sites, pastes, blobs)
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

func TestDeploySite_QuotaRespectsExistingUsage(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:test"
	// First deploy a site that uses most of the budget.
	nearCap := bytes.Repeat([]byte("X"), int(domain.UserQuotaBytes)-200)
	arc1 := gzipTar(t, map[string]string{"index.html": string(nearCap)})
	if _, err := d.Deploy(bytes.NewReader(arc1), owner); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// A second deploy whose bytes exceed the ~200 remaining must be
	// rejected (combined-usage budget computed before the untar).
	arc2 := gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("Y"), 500))})
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
	if !got.ExpiresAt.Equal(fixed.Add(domain.RetentionWindow)) {
		t.Fatalf("expiry: got %v, want %v", got.ExpiresAt, fixed.Add(domain.RetentionWindow))
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

	// Deploy v1 (one ~400-byte file).
	v1 := gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("a"), 400))})
	r1, err := d.Deploy(bytes.NewReader(v1), owner)
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	slug := r1.Site.Slug
	created := r1.Site.CreatedAt
	used1, _ := sites.SumActiveBytesByOwner(owner, time.Now().UTC())

	// Re-deploy v2 with DIFFERENT, smaller content to the SAME slug.
	v2 := gzipTar(t, map[string]string{
		"index.html": string(bytes.Repeat([]byte("b"), 100)),
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

	// Quota delta: the owner's bytes reflect v2's deduped size only, NOT v1+v2.
	used2, _ := sites.SumActiveBytesByOwner(owner, time.Now().UTC())
	wantV2 := int64(r2.Site.Manifest.DedupedSize())
	if used2 != wantV2 {
		t.Fatalf("replace must charge the new size only: owner sum got %d, want %d (used1 was %d)", used2, wantV2, used1)
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

	// A site near the cap (leave ~600 bytes of headroom).
	nearCap := bytes.Repeat([]byte("X"), int(domain.UserQuotaBytes)-600)
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": string(nearCap)})), owner)
	if err != nil {
		t.Fatalf("deploy near-cap site: %v", err)
	}

	// At the original size, a 5000-byte NEW deploy would NOT fit (only ~600 free).
	tooBig := gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("Y"), 5000))})
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
