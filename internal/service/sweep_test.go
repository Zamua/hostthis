package service_test

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// The sweep GCs blobs no live record references.
//
// Uploads go through NewCompressedBlobStore, as the composition root wires
// them: that wrapper owns the at-rest encoding, and handing the raw disk store
// to the blob unit would store encoded bytes it cannot decode on read. The
// sweep itself takes the raw store, which is also production's shape - it walks
// and removes by sha and never reads a body.
func TestSweep_Once(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r1, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>a</p>")), "owner-a", "", "")
	if err != nil {
		t.Fatalf("upload 1: %v", err)
	}
	r2, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>b</p>")), "owner-b", "", "")
	if err != nil {
		t.Fatalf("upload 2: %v", err)
	}
	// A third paste survives every pass. Without a survivor the keep-set goes
	// empty and the abort-on-zero-refs guard (correctly) refuses to GC at all,
	// so the fixture could not observe a collection.
	if _, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>c</p>")), "owner-c", "", ""); err != nil {
		t.Fatalf("upload 3: %v", err)
	}
	// Blob writes finalize in the background; drain before the sweep walks them.
	upload.WaitFinalize()

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(repo, blobs, logger)

	gcBlobs, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if gcBlobs != 0 {
		t.Fatalf("every blob is referenced: blobs=%d", gcBlobs)
	}

	for _, p := range []domain.Slug{r1.Paste.Slug, r2.Paste.Slug} {
		if err := repo.Delete(p); err != nil {
			t.Fatalf("delete %s: %v", p, err)
		}
	}
	gcBlobs, err = sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if gcBlobs != 2 {
		t.Fatalf("expected 2 blobs GC'd, got %d", gcBlobs)
	}

	if _, err := repo.Get(r1.Paste.Slug); err == nil {
		t.Fatalf("paste 1 should be deleted")
	}
	if _, err := repo.Get(r2.Paste.Slug); err == nil {
		t.Fatalf("paste 2 should be deleted")
	}
}

func TestSweep_KeepsActive(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>still here</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	gc, _ := sweep.Once(now)
	if gc != 0 {
		t.Fatalf("a live paste's blob must not be collected, got %d", gc)
	}
	if _, err := repo.Get(r.Paste.Slug); err != nil {
		t.Fatalf("paste should still exist: %v", err)
	}
}

func TestSweep_GCsOrphanBlobOnly(t *testing.T) {
	dir := t.TempDir()
	db, _ := storage.Open(filepath.Join(dir, "g.db"))
	t.Cleanup(func() { _ = db.Close() })
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	repo := storage.NewPasteRepo(db)

	// A referenced blob, via the upload path.
	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }
	_, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>ref</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// An orphan blob, written straight to the store.
	orphanSHA := domain.HashContent([]byte("orphan"))
	if err := blobs.Put(orphanSHA, bytes.NewReader([]byte("orphan")), int64(len("orphan"))); err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	gc, err := sweep.Once(time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if gc != 1 {
		t.Fatalf("expected 1 orphan GC, got %d", gc)
	}
	if _, err := blobs.Get(orphanSHA); err == nil {
		t.Fatalf("orphan should be gone")
	}
}

// A repo reporting zero referenced shas while blobs exist and nothing was just
// deleted must make the sweep REFUSE to GC rather than wipe the store.
func TestSweep_GuardsAgainstBuggyRepoZeroRefs(t *testing.T) {
	dir := t.TempDir()
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))

	sha1 := domain.HashContent([]byte("aaa"))
	sha2 := domain.HashContent([]byte("bbb"))
	for sha, body := range map[string][]byte{sha1: []byte("aaa"), sha2: []byte("bbb")} {
		if err := blobs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}

	repo := &buggyRepo{} // returns 0 referenced shas
	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))

	gc, err := sweep.Once(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep should not error, just refuse: %v", err)
	}
	if gc != 0 {
		t.Fatalf("guard MUST refuse GC, got gc=%d", gc)
	}
	for _, sha := range []string{sha1, sha2} {
		if _, err := blobs.Get(sha); err != nil {
			t.Fatalf("blob %s should survive a buggy repo: %v", sha, err)
		}
	}
}

// buggyRepo's ReferencedBlobSHAs always reports no shas referenced, even when
// paste rows exist. Unused methods panic so an unexpected call surfaces.
type buggyRepo struct{}

func (buggyRepo) ReferencedBlobSHAs() ([]string, error) { return nil, nil }

// In dry-run the sweep computes and logs what it would GC while mutating
// nothing.
func TestSweep_DryRun(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	rA, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>a</p>")), "owner-a", "", "")
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	rB, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>b</p>")), "owner-b", "", "")
	if err != nil {
		t.Fatalf("upload B: %v", err)
	}
	upload.WaitFinalize()

	// Delete B so its blob is already orphaned.
	if err := repo.Delete(rB.Paste.Slug); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, blobs, log.New(&logbuf, "", 0))
	sweep.DryRun = true

	gcBlobs, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if gcBlobs != 1 {
		t.Fatalf("dry-run should report 1 would-gc orphan blob, got %d", gcBlobs)
	}

	// Nothing was mutated: A still exists and both blobs survive on disk.
	if _, err := repo.Get(rA.Paste.Slug); err != nil {
		t.Fatalf("dry-run must NOT delete a live paste; Get err %v", err)
	}
	blobCount := 0
	if err := blobs.WalkBlobs(func(string) error { blobCount++; return nil }); err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	if blobCount != 2 {
		t.Fatalf("dry-run must NOT remove any blob; want 2 on disk, got %d", blobCount)
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("would gc orphan blob")) {
		t.Fatalf("dry-run should log 'would gc orphan blob'; got:\n%s", logbuf.String())
	}
}
