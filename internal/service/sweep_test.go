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

// TestSweep_Once - drives the sweep against a real sqlite + blob store.
// Uploads two pastes, marks one expired by setting Now to the future,
// asserts the expired one is gone and the still-active one survives.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
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
	// The blob writes finalize in the background; drain them so both blobs
	// are on disk before the sweep walks them.
	upload.WaitFinalize()

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(repo, blobs, logger)

	// At the moment of upload, nothing expired.
	pastes, gcBlobs, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if pastes != 0 || gcBlobs != 0 {
		t.Fatalf("nothing should sweep yet: pastes=%d blobs=%d", pastes, gcBlobs)
	}

	// 8 days later, both have expired (retention is 7d).
	future := now.Add(8 * 24 * time.Hour)
	pastes, gcBlobs, err = sweep.Once(future)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if pastes != 2 {
		t.Fatalf("expected 2 expired pastes, got %d", pastes)
	}
	if gcBlobs != 2 {
		t.Fatalf("expected 2 blobs GC'd, got %d", gcBlobs)
	}

	// Both records are gone.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>still here</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	// 1 hour later - well within retention
	pastes, _, _ := sweep.Once(now.Add(time.Hour))
	if pastes != 0 {
		t.Fatalf("active paste should not be swept, got %d", pastes)
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

	// Write a referenced blob via the upload path.
	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }
	_, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>ref</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Write an orphan blob directly.
	orphanSHA := domain.HashContent([]byte("orphan"))
	if err := blobs.Put(orphanSHA, bytes.NewReader([]byte("orphan")), int64(len("orphan"))); err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	_, gc, err := sweep.Once(time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if gc != 1 {
		t.Fatalf("expected 1 orphan GC, got %d", gc)
	}
	// Orphan gone, referenced blob still there.
	if _, err := blobs.Get(orphanSHA); err == nil {
		t.Fatalf("orphan should be gone")
	}
}

// TestSweep_GuardsAgainstBuggyRepoZeroRefs pins the data-loss
// guard: if a buggy metadata-repo impl returns zero referenced shas
// while blobs exist AND no pastes were just deleted, the sweep MUST
// refuse to GC instead of wiping the bucket. We model the bug with
// a fake repo whose ReferencedBlobSHAs always returns nil.
func TestSweep_GuardsAgainstBuggyRepoZeroRefs(t *testing.T) {
	dir := t.TempDir()
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))

	// Two real blobs in the store.
	sha1 := domain.HashContent([]byte("aaa"))
	sha2 := domain.HashContent([]byte("bbb"))
	for sha, body := range map[string][]byte{sha1: []byte("aaa"), sha2: []byte("bbb")} {
		if err := blobs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}

	repo := &buggyRepo{} // returns 0 referenced, 0 expired
	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))

	_, gc, err := sweep.Once(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep should not error, just refuse: %v", err)
	}
	if gc != 0 {
		t.Fatalf("guard MUST refuse GC, got gc=%d", gc)
	}
	// Both blobs must still exist.
	for _, sha := range []string{sha1, sha2} {
		if _, err := blobs.Get(sha); err != nil {
			t.Fatalf("blob %s should survive a buggy repo: %v", sha, err)
		}
	}
}

// buggyRepo simulates the failure mode this test guards against:
// ReferencedBlobSHAs always returns nil (i.e. "no shas are
// referenced") even when paste rows exist. Only methods the sweep
// actually invokes are stubbed; everything else panics so the test
// surfaces unexpected calls.
type buggyRepo struct{}

func (buggyRepo) ExpiredSlugs(_ time.Time) ([]string, error) { return nil, nil }
func (buggyRepo) Delete(_ domain.Slug) error                 { panic("not expected") }
func (buggyRepo) ReferencedBlobSHAs() ([]string, error)      { return nil, nil }

// TestSweep_DryRun - in dry-run the sweep COMPUTES + LOGS what it would
// expire/GC but mutates nothing: the expired record and every blob survive.
// This is the "disabled = log what it would clean" mode. (The blob-GC count
// reflects blobs already orphaned, e.g. by a prior delete; a blob freed only
// by THIS tick's would-be expiry stays referenced because dry-run doesn't
// actually delete the paste - it's counted once the expiry is live.)
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
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

	// Delete B so its blob is already orphaned (a blob the GC would reclaim).
	if err := repo.Delete(rB.Paste.Slug); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, blobs, log.New(&logbuf, "", 0))
	sweep.DryRun = true

	// 8 days later A has expired (retention 7d). Dry-run: A would expire, B's
	// blob would be GC'd - but nothing is actually touched.
	future := now.Add(8 * 24 * time.Hour)
	pastes, gcBlobs, err := sweep.Once(future)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if pastes != 1 {
		t.Fatalf("dry-run should report 1 would-expire record, got %d", pastes)
	}
	if gcBlobs != 1 {
		t.Fatalf("dry-run should report 1 would-gc orphan blob, got %d", gcBlobs)
	}

	// NOTHING was mutated: A still exists, and BOTH blobs survive on disk.
	if _, err := repo.Get(rA.Paste.Slug); err != nil {
		t.Fatalf("dry-run must NOT delete the expired paste; Get err %v", err)
	}
	blobCount := 0
	if err := blobs.WalkBlobs(func(string) error { blobCount++; return nil }); err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	if blobCount != 2 {
		t.Fatalf("dry-run must NOT remove any blob; want 2 on disk, got %d", blobCount)
	}
	// And it logged what it WOULD do.
	if !bytes.Contains(logbuf.Bytes(), []byte("would expire paste")) {
		t.Fatalf("dry-run should log 'would expire paste'; got:\n%s", logbuf.String())
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("would gc orphan blob")) {
		t.Fatalf("dry-run should log 'would gc orphan blob'; got:\n%s", logbuf.String())
	}
}
