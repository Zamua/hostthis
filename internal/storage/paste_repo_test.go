package storage

// Blob-store tests for the disk backend. Paste CRUD lives in the
// backend-agnostic conformance suite (conformance_test.go), which covers
// metadata backends only; the disk blob store has no counterpart there.

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// newTestDB opens an isolated sqlite file in t.TempDir(). On-disk, not
// ":memory:": modernc sqlite gives each connection its own in-memory db, so a
// pooled multi-goroutine test would not see the same data.
func newTestDB(t *testing.T) (*PasteRepo, *BlobStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return NewPasteRepo(db), blobs
}

func TestBlobStore_PutAndGet(t *testing.T) {
	_, blobs := newTestDB(t)
	content := []byte("<!doctype html><p>hello")
	sha := domain.HashContent(content)
	if err := blobs.Put(sha, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := blobs.Get(sha)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("get content: got %q, want %q", got, content)
	}
}

func TestBlobStore_PutIdempotent(t *testing.T) {
	_, blobs := newTestDB(t)
	content := []byte("same bytes")
	sha := domain.HashContent(content)
	for range 3 {
		if err := blobs.Put(sha, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
}

func TestBlobStore_GetNotFound(t *testing.T) {
	_, blobs := newTestDB(t)
	_, err := blobs.Get(domain.HashContent([]byte("never written")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
