//go:build slatedb

// SERVICE-LEVEL tests for DeploySite.Deploy on the transactional shale path.
// These drive the real service.DeploySite (NOT just the seam) so they catch the
// P0 the seam-only TestSites_BindAllAndRedeployDrops masked: that test always
// stages files under the SAME real slug it later inserts under ("siteslug"), so
// the stage-and-bind co-routed by construction. The real first-time Deploy mints
// its OWN slug internally - and before the fix it staged every file under the
// EMPTY slug (the slug was only minted in the post-untar insert loop), so the
// staged BlobRef's route shard derived from RouteKeyForSlug("") while the
// manifest pinned the real slug's shard. The bind then cross-shards
// (backend.ErrCrossShard) and every brand-new site is born unreadable.
//
// The fix pre-claims the slug BEFORE the untar (a metadata-only single-shard
// claim) and stages every file under it, so the pointers co-route with the
// manifest. These tests prove a first Deploy READS A FILE BACK (the manifest and
// the bound file blob both resolve) and that a redeploy (DeployToSlug) drops the
// removed file's blob.

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

// gzipTar builds a gzip-tar archive from path->body, the SAME shape the SSH
// deploy path feeds DeploySite.Deploy. Order-independent (a map), since the
// safe-untar produces a path-keyed manifest.
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
// (real slate-over-MinIO metadata, in-memory blobmem byte plane) + the
// shaleblob.Unit seam: the SAME backend a production shale-collocated deploy
// runs on. Returns the deploy service, the seam unit (for read-back), and the
// repo. Skips when MinIO is absent.
func newShaleDeploy(t *testing.T) (*service.DeploySite, *shaleblob.Unit, *storage.ShaleRepo) {
	t.Helper()
	repo, unit, _ := newBlobRepo(t) // skips when MinIO is absent
	sites := storage.NewShaleSiteRepo(repo)
	// repo (ShaleRepo) satisfies service.PasteByteSummer (its paste-side
	// SumActiveBytesByOwner returns (int, error)).
	d := service.NewDeploySite(sites, repo, unit)
	return d, unit, repo
}

// TestDeploySite_Shale_FirstDeployBindsAndReadsBack is the regression for the
// P0: a first-time Deploy must stage every file under the slug it commits the
// manifest under, so the file blobs bind on the same {slug} shard and read back.
// Before the fix the files staged under the empty slug, the bind cross-sharded
// (backend.ErrCrossShard), and the deploy failed (or - had it landed - the read
// 404'd because the bref routed to a different shard than the manifest).
func TestDeploySite_Shale_FirstDeployBindsAndReadsBack(t *testing.T) {
	d, unit, _ := newShaleDeploy(t)

	// Sanity: the wired seam IS transactional, so Deploy takes the pre-claim
	// branch (the whole point of the fix). A false here means the standalone
	// post-untar-mint path would run and the assertions would not exercise the
	// cross-shard bind at all.
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
		// The raw failure mode of the bug: the cross-shard bind aborts the insert.
		t.Fatalf("Deploy (first-time): %v (the files staged under the wrong slug shard?)", err)
	}
	if len(res.Site.Manifest.Files) != len(files) {
		t.Fatalf("manifest files = %d, want %d", len(res.Site.Manifest.Files), len(files))
	}
	if res.Site.Slug == "" {
		t.Fatalf("Deploy returned an empty slug")
	}

	slug := string(res.Site.Slug)

	// THE LOAD-BEARING ASSERTION: every file's blob is bound on the manifest's
	// shard and reads back byte-exact through the seam. ResolveBlobID(slug, sha)
	// finds the site row's FileBlobs[sha] -> the blob id -> GetBlob on the {slug}
	// shard. If the file had staged under the empty slug, either the bind would
	// have cross-sharded (Deploy errored above) or the bref would route to a
	// different shard and this Read would 404.
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

// TestDeploySite_Shale_RedeployDropsRemovedFile pins the redeploy (DeployToSlug)
// path: a re-deploy that removes a file unbinds that file's blob (a Read of the
// dropped sha 404s) while a retained-but-changed file reads its NEW bytes. This
// also exercises that DeployToSlug stages under the REAL slug (it always has -
// that path was correct - so it is the cheap-to-keep cross-check the brief asks
// for, run against the same backend as the first-deploy assertion).
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

	// Redeploy: keep index.html (changed bytes -> new sha), DROP about.html, add
	// contact.html. The dropped file's blob must unbind; the new manifest's files
	// must read back.
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

	// The new manifest's files read back.
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

	// The dropped file's blob is unbound: its sha no longer resolves.
	if _, rerr := readAll(t, unit, string(slug), aboutSHAv1); !isNotFound(rerr) {
		t.Fatalf("read dropped about.html (sha %s) after redeploy = %v, want not-found", aboutSHAv1, rerr)
	}
}

// TestDeploySite_Shale_PreClaimRejectsTakenSlug pins the pre-claim collision
// guard directly: a slug already owned (by a paste) is rejected by PreClaimSlug
// with a slug-taken error, so the deploy re-mints a fresh slug rather than
// staging under (and then failing to insert at) a taken slug. The deploy still
// succeeds on a fresh slug.
func TestDeploySite_Shale_PreClaimRejectsTakenSlug(t *testing.T) {
	d, _, repo := newShaleDeploy(t)
	sites := storage.NewShaleSiteRepo(repo)
	ctx := context.Background()
	now := d.Now().UTC()

	// A free slug claims cleanly; re-claiming the SAME slug now collides (the
	// claim wrote slug_owner/<slug>), proving the claim is durable + serializing.
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

	// And an actual deploy still lands on a fresh slug (the pre-claim loop re-mints
	// past any collision; with a trillion-slug space the first mint is free here).
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
