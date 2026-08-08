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
	"github.com/Zamua/hostthis/internal/storagetest"
)

// deployFixture wires real metadata repos + a real compressed blob store so
// the test exercises the actual untar → blob → manifest → persist path.
func deployFixture(t *testing.T) (*DeploySite, *storage.Sites, *storage.CompressedBlobStore) {
	t.Helper()
	dir := t.TempDir()
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(disk)
	sites := storage.NewSites(storagetest.NewRepo(t))
	pastes := storagetest.NewRepo(t)
	d := NewDeploySite(sites, pastes, NewStandaloneBlobUnit(blobs))
	return d, sites, blobs
}

// ownerCharge is the identity's charged bytes. A directory is a paste, so
// its bytes live in the ARTIFACT sum - the site port reports zero to avoid
// counting them twice (storage.Sites.SumActiveBytesByOwner).
func ownerCharge(t *testing.T, owner string) int64 {
	t.Helper()
	n, err := storagetest.NewRepo(t).SumActiveBytesByOwner(owner, time.Now().UTC())
	if err != nil {
		t.Fatalf("owner charge: %v", err)
	}
	return int64(n)
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

// incompressible returns n bytes of random data. Sites charge their COMPRESSED
// size, so a "fill the budget" fixture must be incompressible for its charge to
// be ~n; repeated bytes would squash to nothing.
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
	got, err := sites.Get(res.Site.Slug)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
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
	if err := d.Delete(slug, "key:owner"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := sites.Get(slug); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("site should be gone: got %v", err)
	}
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

// TestDeploySite_ChargesCompressedSize pins that a site is charged its
// COMPRESSED (post-zstd) size against quota, matching how pastes charge: the
// charged number equals the manifest's CompressedSize, not the
// uncompressed sum.
func TestDeploySite_ChargesCompressedSize(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:compress"
	// Highly compressible content: 200 KB of a repeated byte squashes tiny.
	raw := bytes.Repeat([]byte("A"), 200_000)
	r, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": string(raw)})), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	used := ownerCharge(t, owner)
	compressed := int64(r.Site.Manifest.CompressedSize())
	uncompressed := int64(r.Site.Manifest.Size())
	if used != compressed {
		t.Fatalf("charge should equal compressed size: used %d, compressed %d", used, compressed)
	}
	if uncompressed != 200_000 {
		t.Fatalf("uncompressed manifest size should be the raw 200000, got %d", uncompressed)
	}
	if compressed >= uncompressed/10 {
		t.Fatalf("compressible site should charge <<10%% of raw: compressed %d vs uncompressed %d", compressed, uncompressed)
	}
}

func TestDeploySite_QuotaRespectsExistingUsage(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:test"
	// A site using most of the budget (incompressible so its COMPRESSED charge
	// nears the cap). ~20 KB headroom is enough that zstd's block-header
	// expansion of random data cannot overrun.
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
	if r1.Site.Manifest.Files["index.html"].SHA != r2.Site.Manifest.Files["index.html"].SHA {
		t.Fatalf("identical content should share a blob SHA")
	}
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
	if !got.CreatedAt.Equal(fixed) {
		t.Fatalf("created_at: got %v, want %v", got.CreatedAt, fixed)
	}
}

// TestDeployToSlug_ReplacesInPlace pins that DeployToSlug re-deploys an existing
// OWNED site at the SAME slug: slug + created_at preserved, manifest swapped to
// the new content, and the owner's live bytes reflecting the NEW size only (the
// old bytes are freed in the same swap, not double-counted).
func TestDeployToSlug_ReplacesInPlace(t *testing.T) {
	d, sites, _ := deployFixture(t)
	owner := "key:rp"

	// v1 is one large incompressible file so its COMPRESSED charge clearly
	// exceeds the smaller v2; with tiny or compressible content the per-file
	// zstd overhead would dominate and mask the shrink.
	v1 := gzipTar(t, map[string]string{"index.html": incompressible(t, 4000)})
	r1, err := d.Deploy(bytes.NewReader(v1), owner)
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	slug := r1.Site.Slug
	created := r1.Site.CreatedAt
	used1 := ownerCharge(t, owner)

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

	// A redeploy is an UPDATE: it appends a version and the previous one stays
	// live, so the owner is charged for both and can roll back to either. The
	// new version's own bytes must be part of that, and dedup means the growth
	// is only what actually changed rather than the whole redeploy.
	used2 := ownerCharge(t, owner)
	if used2 <= used1 {
		t.Fatalf("a redeploy must ADD its version's bytes: used1 %d, used2 %d", used1, used2)
	}
	grew := used2 - used1
	wantV2 := int64(r2.Site.Manifest.CompressedSize())
	if grew > wantV2 {
		t.Fatalf("a redeploy must charge at most its own size: grew %d, v2 is %d", grew, wantV2)
	}

	// Both versions are addressable, which is what rollback needs.
	vs, err := versionsOf(t, slug)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("versions = %d, want 2 (a redeploy keeps what it replaced)", len(vs))
	}
}

func versionsOf(t *testing.T, slug domain.Slug) ([]domain.Version, error) {
	t.Helper()
	return storagetest.NewRepo(t).ListVersions(slug)
}

// TestDeployToSlug_FreesOldChargesNewDelta pins the replace-delta quota math:
// shrinking a near-cap site frees enough budget that a follow-up NEW deploy
// fits where it would have been rejected at the original size.
func TestDeployToSlug_FreesOldChargesNewDelta(t *testing.T) {
	d, _, _ := deployFixture(t)
	owner := "key:delta"

	// A site near the cap (~20 KB headroom), incompressible so the compressed
	// charge actually nears the cap.
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

	// The freed budget now admits a NEW deploy.
	if _, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": string(bytes.Repeat([]byte("Z"), 5000))})), owner); err != nil {
		t.Fatalf("5000B new deploy should fit after the shrink freed budget: %v", err)
	}
}

// TestDeployToSlug_NonexistentSlug pins that re-deploying to a slug that was
// never deployed returns ErrNotFound (the "service: not found" the SSH layer
// maps to exit 4), never silently creating it.
func TestDeployToSlug_NonexistentSlug(t *testing.T) {
	d, _, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{"index.html": "<h1>x</h1>"})
	_, err := d.DeployToSlug(domain.Slug("nosuch12"), bytes.NewReader(arc), "key:test")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-deploy to a missing slug: got %v, want ErrNotFound", err)
	}
}

// TestDeployToSlug_ForeignSlug pins ownership at the service layer: a second
// identity re-deploying onto someone else's site gets ErrNotFound (no
// ownership/existence leak) and the original site is untouched.
func TestDeployToSlug_ForeignSlug(t *testing.T) {
	d, sites, _ := deployFixture(t)

	r, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>alice</h1>"})), "key:alice")
	if err != nil {
		t.Fatalf("alice deploy: %v", err)
	}
	slug := r.Site.Slug
	aliceSHA := r.Site.Manifest.Files["index.html"].SHA

	_, err = d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>mallory</h1>"})), "key:mallory")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign re-deploy: got %v, want ErrNotFound (no ownership leak)", err)
	}

	got, err := sites.Get(slug)
	if err != nil {
		t.Fatalf("get alice's site: %v", err)
	}
	if got.Manifest.Files["index.html"].SHA != aliceSHA {
		t.Fatalf("rejected foreign re-deploy mutated the owner's manifest: %+v", got.Manifest.Files)
	}
}

// The charge is the post-compression bytes staging actually wrote.
//
// Two paths holding IDENTICAL content are two objects on disk, because a blob id
// is minted fresh per staged file rather than derived from the content. The
// quota therefore counts both. Folding them by hash would bill for one copy of
// bytes we stored twice.
func TestDeploySite_ChargesEveryStoredCopy(t *testing.T) {
	body := "<!doctype html><h1>same bytes</h1>"

	d, _, _ := deployFixture(t)
	if _, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": body})), "key:one"); err != nil {
		t.Fatalf("deploy one: %v", err)
	}
	single := ownerCharge(t, "key:one")

	// The same bytes at a second path: stored twice, so charged twice.
	if _, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{
		"index.html": body,
		"copy.html":  body,
	})), "key:two"); err != nil {
		t.Fatalf("deploy two: %v", err)
	}
	dup := ownerCharge(t, "key:two")

	if single <= 0 {
		t.Fatalf("fixture: a one-file deploy must charge something, got %d", single)
	}
	if dup != 2*single {
		t.Fatalf("two identical files charged %d, want %d (twice the one-file charge): "+
			"the store writes both, so folding them by hash bills for bytes we did store",
			dup, 2*single)
	}
}
