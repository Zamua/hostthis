package storage

// Blob-store tests for the sqlite/disk backend. The paste CRUD tests that
// used to live here (insert+get round-trip, get-not-found, duplicate slug)
// were folded into the backend-agnostic conformance suite, which runs the
// same assertions against sqlite on every default `go test` run
// (conformInsertAndGet / conformGetNotFound / conformDuplicateSlug in
// conformance_test.go). The BlobStore tests below have no conformance
// counterpart (the suite covers metadata backends, not the disk blob
// store), so they stay.

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// newTestDB opens an isolated sqlite file in t.TempDir(). We use an
// on-disk DB (not :memory:) so the connection pool with multiple
// goroutines sees the same data - modernc sqlite ":memory:" creates
// a fresh in-memory db per connection by default.
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
