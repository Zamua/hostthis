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

// The GC keep-set is the UNION across kinds: a blob shared by a paste and a
// site is collected only once NEITHER references it.
func TestSweep_SharedBlobSurvivesUntilBothKindsRelease(t *testing.T) {
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

	// A survivor keeps the keep-set non-empty: on an empty set the sweep's
	// abort-on-zero-refs guard refuses to GC anything, so the fixture could not
	// observe a collection.
	survivor := service.NewUpload(pastes, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(survivor.WaitFinalize)
	survivor.Now = func() time.Time { return now }
	if _, err := survivor.Create(bytes.NewReader([]byte("<!doctype html><p>survivor</p>")), "key:survivor", "", ""); err != nil {
		t.Fatalf("survivor upload: %v", err)
	}
	survivor.WaitFinalize()

	// Identical bytes uploaded as a paste AND deployed as a site, so the two
	// records share one blob.
	shared := "<!doctype html><h1>shared bytes</h1>"
	upload := service.NewUpload(pastes, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return now }
	up, err := upload.Create(bytes.NewReader([]byte(shared)), "key:paste-owner", "", "")
	if err != nil {
		t.Fatalf("paste upload: %v", err)
	}

	deploy := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deploy.Now = func() time.Time { return now }
	res, err := deploy.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:site-owner")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	sha := res.Site.Manifest.Files["index.html"].SHA

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Sites = sites

	// Both alive: nothing to collect.
	if gc, err := sweep.Once(now); err != nil || gc != 0 {
		t.Fatalf("both records alive: gc=%d err=%v, want 0/nil", gc, err)
	}

	// Drop the paste. The site still references the blob, so it survives.
	if err := pastes.Delete(up.Paste.Slug); err != nil {
		t.Fatalf("delete paste: %v", err)
	}
	if gc, err := sweep.Once(now); err != nil || gc != 0 {
		t.Fatalf("site still references the blob: gc=%d err=%v, want 0/nil", gc, err)
	}
	if _, err := blobs.Get(sha); err != nil {
		t.Fatalf("shared blob wrongly collected while the site referenced it: %v", err)
	}

	// Drop the site too. Now nothing references the blob.
	if err := sites.Delete(res.Site.Slug); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if gc, err := sweep.Once(now); err != nil || gc != 1 {
		t.Fatalf("blob unreferenced by both kinds: gc=%d err=%v, want 1/nil", gc, err)
	}
}

// Two sites sharing one blob: deleting one leaves the blob referenced by the
// other. Pins that the site-side keep-set is a union across sites, not a
// last-writer-wins single reference.
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

	deployA := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deployA.Now = func() time.Time { return t0 }
	resA, err := deployA.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:a")
	if err != nil {
		t.Fatalf("deploy A: %v", err)
	}

	deployB := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deployB.Now = func() time.Time { return t0 }
	if _, err := deployB.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:b"); err != nil {
		t.Fatalf("deploy B: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Sites = sites

	if err := sites.Delete(resA.Site.Slug); err != nil {
		t.Fatalf("delete site A: %v", err)
	}
	gc, err := sweep.Once(t0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if gc != 0 {
		t.Fatalf("shared blob must survive while site B references it; gc=%d", gc)
	}
	if _, err := blobs.Get(resA.Site.Manifest.Files["index.html"].SHA); err != nil {
		t.Fatalf("shared blob wrongly collected: %v", err)
	}
}
