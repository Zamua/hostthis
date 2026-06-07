package storage

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// newTestDB opens an isolated sqlite file in t.TempDir(). We use an
// on-disk DB (not :memory:) so the connection pool with multiple
// goroutines sees the same data — modernc sqlite ":memory:" creates
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

func TestPasteRepo_InsertAndGet(t *testing.T) {
	repo, _ := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second) // round-trip safely through RFC3339Nano
	p := domain.Paste{
		Slug:          "abc23456",
		Identity:      "key:sha256:test",
		Kind:          domain.KindHTML,
		ContentSHA:    "sha-of-bytes",
		Size:          42,
		Name:          "demo",
		PinnedVersion: 1,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	if err := repo.Insert(p); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Slug != p.Slug || got.Identity != p.Identity || got.Kind != p.Kind ||
		got.ContentSHA != p.ContentSHA || got.Size != p.Size || got.Name != p.Name {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, p)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("time round-trip mismatch:\n got  %v / %v\n want %v / %v",
			got.CreatedAt, got.ExpiresAt, p.CreatedAt, p.ExpiresAt)
	}
}

func TestPasteRepo_GetNotFound(t *testing.T) {
	repo, _ := newTestDB(t)
	_, err := repo.Get("abcdefgh")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestPasteRepo_InsertDuplicateSlugFails(t *testing.T) {
	repo, _ := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	p := domain.Paste{
		Slug:          "abc23456",
		Kind:          domain.KindHTML,
		ContentSHA:    "x",
		Size:          1,
		PinnedVersion: 1,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	if err := repo.Insert(p); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := repo.Insert(p); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("second insert: got %v, want ErrSlugTaken", err)
	}
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
