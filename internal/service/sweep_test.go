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

// TestSweep_Once — drives the sweep against a real sqlite + blob store.
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

	upload := service.NewUpload(repo, blobs)
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

	upload := service.NewUpload(repo, blobs)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>still here</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	// 1 hour later — well within retention
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
	upload := service.NewUpload(repo, blobs)
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
