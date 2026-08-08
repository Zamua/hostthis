//go:build slatedb

// SERVICE-LEVEL tests for DeploySite.Deploy on the transactional shale path.
// They drive the real service.DeploySite, not just the seam, so a first-time
// Deploy mints its OWN slug internally rather than staging under a slug the
// test already knows.
//
// That distinction is the whole point: a file staged under a different slug
// than the manifest routes to a different shard, so the bind cross-shards
// (backend.ErrCrossShard) and the site is born unreadable. Deploy therefore
// pre-claims the slug before the untar (a metadata-only single-shard claim) and
// stages every file under it, so the pointers co-route with the manifest.

package shaleblob_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
)

// gzipTar builds a gzip-tar archive from path->body, the same shape the SSH
// deploy path feeds DeploySite.Deploy.
func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar hdr %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// newShaleDeploy wires a real service.DeploySite over a blob-capable ShaleRepo
// (slate-over-MinIO metadata, in-memory blobmem byte plane) plus the
// shaleblob.Unit seam: the same backend a shale-collocated deploy runs on.
// Skips when MinIO is absent.
func newShaleDeploy(t *testing.T) (*service.DeploySite, *shaleblob.Unit, *storage.ShaleRepo) {
	t.Helper()
	repo, unit, _ := newBlobRepo(t) // skips when MinIO is absent
	sites := storage.NewSites(repo)
	// ShaleRepo satisfies service.PasteByteSummer.
	d := service.NewDeploySite(sites, repo, unit)
	return d, unit, repo
}

// TestDeploySite_Shale_FirstDeployBindsAndReadsBack pins that a first-time
// Deploy stages every file under the slug it commits the manifest under, so the
// file blobs bind on the same {slug} shard and read back.
func TestDeploySite_Shale_FirstDeployBindsAndReadsBack(t *testing.T) {
	d, unit, _ := newShaleDeploy(t)

	// Only a transactional seam takes the pre-claim branch. A false here means
	// the standalone post-untar-mint path runs instead and nothing below
	// exercises the cross-shard bind.
	if !unit.IsTransactional() {
		t.Fatalf("shale unit IsTransactional() = false, want true")
	}

	files := map[string]string{
		"index.html":    "<!doctype html><h1>shale site</h1>",
		"about.html":    "<!doctype html><p>about</p>",
		"css/style.css": "body{margin:0}",
	}
	arc := gzipTar(t, files)

	res, err := d.Deploy(bytes.NewReader(arc), "owner-deploy-shale")
	if err != nil {
		// A cross-shard bind aborts the insert.
		t.Fatalf("Deploy (first-time): %v (the files staged under the wrong slug shard?)", err)
	}
	if len(res.Site.Manifest.Files) != len(files) {
		t.Fatalf("manifest files = %d, want %d", len(res.Site.Manifest.Files), len(files))
	}
	if res.Site.Slug == "" {
		t.Fatalf("Deploy returned an empty slug")
	}

	slug := string(res.Site.Slug)

	// The load-bearing assertion: every file's blob is bound on the manifest's
	// shard and reads back byte-exact. A bref routed to any other shard 404s
	// here.
	for path, want := range files {
		entry := res.Site.Manifest.Files[path]
		if entry.SHA == "" {
			t.Fatalf("manifest entry %q has no SHA", path)
		}
		out, rerr := readAll(t, unit, slug, entry.SHA)
		if rerr != nil {
			t.Fatalf("read back %q (sha %s): %v (the file blob was not bound on the manifest shard)", path, entry.SHA, rerr)
		}
		if string(out) != want {
			t.Fatalf("read back %q = %q, want %q", path, out, want)
		}
	}
}

// TestDeploySite_Shale_RedeployDropsRemovedFile pins the redeploy
// (DeployToSlug) path: removing a file unbinds that file's blob (a Read of the
// dropped sha 404s) while a retained-but-changed file reads its NEW bytes.
func TestDeploySite_Shale_RedeployDropsRemovedFile(t *testing.T) {
	d, unit, _ := newShaleDeploy(t)

	owner := "owner-redeploy-shale"
	v1 := map[string]string{
		"index.html": "<!doctype html><h1>v1</h1>",
		"about.html": "<!doctype html><p>about v1</p>",
	}
	res1, err := d.Deploy(bytes.NewReader(gzipTar(t, v1)), owner)
	if err != nil {
		t.Fatalf("Deploy v1: %v", err)
	}
	slug := res1.Site.Slug
	aboutSHAv1 := res1.Site.Manifest.Files["about.html"].SHA

	// Keep index.html (changed bytes -> new sha), DROP about.html, add
	// contact.html.
	v2 := map[string]string{
		"index.html":   "<!doctype html><h1>v2</h1>",
		"contact.html": "<!doctype html><p>contact</p>",
	}
	res2, err := d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, v2)), owner)
	if err != nil {
		t.Fatalf("DeployToSlug v2: %v", err)
	}
	if res2.Site.Slug != slug {
		t.Fatalf("redeploy changed the slug: got %q, want %q", res2.Site.Slug, slug)
	}

	for path, want := range v2 {
		entry := res2.Site.Manifest.Files[path]
		out, rerr := readAll(t, unit, string(slug), entry.SHA)
		if rerr != nil {
			t.Fatalf("read back %q after redeploy: %v", path, rerr)
		}
		if string(out) != want {
			t.Fatalf("read back %q = %q, want %q", path, out, want)
		}
	}

	// The dropped file STAYS readable: a redeploy appends a version and the
	// previous one still references the file, which is what makes rolling back
	// to it meaningful. Its bytes are released when that version is deleted,
	// not when the next deploy stops naming it.
	if _, rerr := readAll(t, unit, string(slug), aboutSHAv1); rerr != nil {
		t.Fatalf("dropped about.html (sha %s) must remain readable through v1: %v", aboutSHAv1, rerr)
	}
}

// TestDeploySite_Shale_PreClaimRejectsTakenSlug pins the pre-claim collision
// guard: an already-owned slug is rejected with a slug-taken error, so the
// deploy re-mints rather than staging under (and then failing to insert at) a
// taken slug.
func TestDeploySite_Shale_PreClaimRejectsTakenSlug(t *testing.T) {
	d, _, repo := newShaleDeploy(t)
	sites := storage.NewSites(repo)
	ctx := context.Background()
	now := d.Now().UTC()

	// Re-claiming the SAME slug collides, so the claim is durable and
	// serializing, not advisory.
	slug := domain.NewRandomSlug()
	if err := sites.PreClaimSlug(ctx, slug, "owner-x", now); err != nil {
		t.Fatalf("first PreClaimSlug(%q): %v", slug, err)
	}
	err := sites.PreClaimSlug(ctx, slug, "owner-x", now)
	if err == nil {
		t.Fatalf("second PreClaimSlug(%q) = nil, want slug-taken", slug)
	}
	if !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("second PreClaimSlug(%q) = %v, want ErrSlugTaken", slug, err)
	}

	// A deploy still lands on a fresh slug: the pre-claim loop re-mints past a
	// collision.
	res, derr := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{
		"index.html": "<h1>fresh</h1>",
	})), "owner-fresh")
	if derr != nil {
		t.Fatalf("Deploy after a pre-claim collision elsewhere: %v", derr)
	}
	if res.Site.Slug == slug {
		t.Fatalf("Deploy landed on the already-claimed slug %q", slug)
	}
}
