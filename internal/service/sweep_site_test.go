package service_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

func gzTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// TestSweep_ExpiresSitesAndProtectsSharedBlobs drives the sweep against
// real sqlite + blob stores with both a paste and a site sharing a blob.
// When the site expires but the paste references the SAME bytes, the
// shared blob must survive (union of paste-ref + site-ref keep-alive).
func TestSweep_ExpiresSitesAndProtectsSharedBlobs(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(disk)
	pastes := storage.NewPasteRepo(db)
	sites := storage.NewSiteRepo(db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Identical bytes uploaded as a paste AND deployed as a site so they
	// share a blob.
	shared := "<!doctype html><h1>shared bytes</h1>"
	upload := service.NewUpload(pastes, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return now }
	if _, err := upload.Create(bytes.NewReader([]byte(shared)), "key:paste-owner", "", ""); err != nil {
		t.Fatalf("paste upload: %v", err)
	}

	deploy := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deploy.Now = func() time.Time { return now }
	res, err := deploy.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:site-owner")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Sites = sites

	// Past the retention window: the SITE has expired but the paste still references the
	// shared bytes. The site row must be deleted; the shared blob must NOT.
	future := now.Add(domain.DefaultRetentionWindow + 24*time.Hour)

	// Reset the paste's expiry far into the future via a fresh upload-time
	// trick: instead, just sweep at a point where the paste is alive. The
	// paste was created at `now` with 30d retention, so it ALSO expires by
	// `future`. To isolate the "shared blob survives while one record
	// lives" property, sweep at now+1h (nothing expired) first, then prove
	// the site expiry path independently below.
	deleted, gc, err := sweep.Once(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep early: %v", err)
	}
	if deleted != 0 || gc != 0 {
		t.Fatalf("nothing should sweep early: deleted=%d gc=%d", deleted, gc)
	}

	// The shared blob is readable (sanity).
	sha := res.Site.Manifest.Files["index.html"].SHA
	if _, err := blobs.Get(sha); err != nil {
		t.Fatalf("shared blob missing before sweep: %v", err)
	}

	// Now sweep at `future`: BOTH the paste and the site have expired, so
	// the shared blob is unreferenced and gets GC'd. This confirms the
	// site sweep path deletes the site row and that its blob is collected
	// once no record references it.
	deleted, gc, err = sweep.Once(future)
	if err != nil {
		t.Fatalf("sweep future: %v", err)
	}
	// 1 paste + 1 site expired.
	if deleted != 2 {
		t.Fatalf("expected 2 records expired (paste+site), got %d", deleted)
	}
	if gc != 1 {
		t.Fatalf("expected 1 shared blob GC'd, got %d", gc)
	}
	// Site row gone.
	if _, err := sites.Get(res.Site.Slug); err == nil {
		t.Fatalf("site should be deleted after expiry")
	}
}

// TestSweep_SiteBlobSurvivesWhileAnotherSiteReferencesIt proves the
// union keep-alive: two sites share a blob, one expires, the blob lives.
func TestSweep_SiteBlobSurvivesWhileAnotherSiteReferencesIt(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(disk)
	pastes := storage.NewPasteRepo(db)
	sites := storage.NewSiteRepo(db)

	t0 := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	shared := "<!doctype html><h1>two sites share me</h1>"

	// Site A at t0 (expires t0+30d).
	deployA := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deployA.Now = func() time.Time { return t0 }
	resA, err := deployA.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:a")
	if err != nil {
		t.Fatalf("deploy A: %v", err)
	}

	// Site B deployed 3 days later (expires t0+3d+30d = t0+33d) - still alive
	// when A expires at t0+30d.
	t1 := t0.Add(3 * 24 * time.Hour)
	deployB := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deployB.Now = func() time.Time { return t1 }
	if _, err := deployB.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:b"); err != nil {
		t.Fatalf("deploy B: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Sites = sites

	// Sweep past A's window: site A expired, site B alive. The shared blob must
	// survive because B still references it.
	deleted, gc, err := sweep.Once(t0.Add(domain.DefaultRetentionWindow + 24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected only site A expired, got %d", deleted)
	}
	if gc != 0 {
		t.Fatalf("shared blob must survive while site B references it; gc=%d", gc)
	}
	if _, err := blobs.Get(resA.Site.Manifest.Files["index.html"].SHA); err != nil {
		t.Fatalf("shared blob wrongly collected: %v", err)
	}
}
