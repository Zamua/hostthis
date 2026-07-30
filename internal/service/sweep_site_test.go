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

// TestSweep_ExpiresSitesAndProtectsSharedBlobs pins the union keep-alive
// across kinds: a blob shared by a paste and a site is collected only once
// NEITHER references it.
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

	// Identical bytes uploaded as a paste AND deployed as a site, so the two
	// records share one blob.
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

	future := now.Add(domain.DefaultRetentionWindow + 24*time.Hour)

	// Both records were created at `now` with the same retention, so both are
	// alive here and both expire by `future`.
	deleted, gc, err := sweep.Once(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep early: %v", err)
	}
	if deleted != 0 || gc != 0 {
		t.Fatalf("nothing should sweep early: deleted=%d gc=%d", deleted, gc)
	}

	sha := res.Site.Manifest.Files["index.html"].SHA
	if _, err := blobs.Get(sha); err != nil {
		t.Fatalf("shared blob missing before sweep: %v", err)
	}

	// At `future` both records have expired, so nothing references the shared
	// blob and it is collected.
	deleted, gc, err = sweep.Once(future)
	if err != nil {
		t.Fatalf("sweep future: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 records expired (paste+site), got %d", deleted)
	}
	if gc != 1 {
		t.Fatalf("expected 1 shared blob GC'd, got %d", gc)
	}
	if _, err := sites.Get(res.Site.Slug); err == nil {
		t.Fatalf("site should be deleted after expiry")
	}
}

// TestSweep_SiteIndexNoOpsNotCountedAsDeleted pins the site half of the
// expiry-pass contract: an expired index entry whose site record is ALREADY
// GONE is an index cleanup, not a record deletion, so the deleted-count must
// not include it. Counting it reports a constant nonzero deletion every cycle
// forever.
func TestSweep_SiteIndexNoOpsNotCountedAsDeleted(t *testing.T) {
	sites := &orphanSiteSweep{ref: domain.ExpiredSite{
		Slug:     "ctimu4qh",
		IndexRef: "expiry_sites/2026-07-03T21:22:23.536246341Z/ctimu4qh",
	}}
	sweep := service.NewSweep(noopSweepRepo{}, nil, log.New(io.Discard, "", 0))
	sweep.Sites = sites

	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	deleted, _, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("a site-index no-op (record already gone) must not count as a deletion: got %d, want 0", deleted)
	}
	// The cleanup must target the surfaced index entry, not a re-derivation.
	if len(sites.gotRefs) != 1 || sites.gotRefs[0] != sites.ref {
		t.Fatalf("DeleteExpiredSite must receive the exact scanned ref; got %+v", sites.gotRefs)
	}

	deleted, _, err = sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if deleted != 0 || len(sites.gotRefs) != 1 {
		t.Fatalf("second pass must see zero expired sites: deleted=%d calls=%d", deleted, len(sites.gotRefs))
	}
}

// orphanSiteSweep is a SweepSites whose expiry scan surfaces one entry
// referencing a site record that is already gone, so the record delete is an
// idempotent no-op and the entry cleanup drains the scan.
type orphanSiteSweep struct {
	ref     domain.ExpiredSite
	drained bool
	gotRefs []domain.ExpiredSite
}

func (s *orphanSiteSweep) ExpiredSites(_ time.Time) ([]domain.ExpiredSite, error) {
	if s.drained {
		return nil, nil
	}
	return []domain.ExpiredSite{s.ref}, nil
}

func (s *orphanSiteSweep) DeleteExpiredSite(ref domain.ExpiredSite) (bool, error) {
	s.gotRefs = append(s.gotRefs, ref)
	s.drained = true
	return false, nil
}

func (s *orphanSiteSweep) ReferencedSiteBlobSHAs() ([]string, error) { return nil, nil }

// TestSweep_SiteBlobSurvivesWhileAnotherSiteReferencesIt: two sites share a
// blob, one expires, the blob lives.
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

	// Site B is deployed 3 days later, so it is still alive when A expires.
	t1 := t0.Add(3 * 24 * time.Hour)
	deployB := service.NewDeploySite(sites, pastes, service.NewStandaloneBlobUnit(blobs))
	deployB.Now = func() time.Time { return t1 }
	if _, err := deployB.Deploy(bytes.NewReader(gzTar(t, map[string]string{"index.html": shared})), "key:b"); err != nil {
		t.Fatalf("deploy B: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Sites = sites

	// Past A's window only: B still references the shared blob.
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
