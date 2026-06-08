package service_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

func newStack(t *testing.T) (*service.Upload, *service.Manage, *storage.PasteRepo) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	repo := storage.NewPasteRepo(db)
	upload := service.NewUpload(repo, blobs)
	manage := service.NewManage(repo, blobs)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }
	manage.Now = func() time.Time { return now }
	return upload, manage, repo
}

// htmlBody returns an HTML-detected payload of approximately `n` bytes
// of HIGH-ENTROPY ASCII so zstd compression barely shrinks it. Quota
// tests need predictable compressed sizes ≈ uncompressed sizes so the
// 10 MiB compressed cap is hit with N-byte inputs as expected.
//
// Deterministic LCG so the test stays reproducible.
func htmlBody(n int) []byte {
	const head = "<!doctype html><body>"
	if n <= len(head) {
		return []byte(head[:n])
	}
	out := make([]byte, n)
	copy(out, head)
	const printableSpan = 0x7e - 0x21 + 1 // '!' through '~'
	seed := uint32(0x12345678)
	for i := len(head); i < n; i++ {
		seed = seed*1103515245 + 12345
		out[i] = byte(0x21 + int(seed>>16)%printableSpan)
	}
	return out
}

// Quota tests are now sized against the 10 MiB compressed cap. htmlBody
// produces high-entropy ASCII so compressed ≈ uncompressed (each byte
// in maps to ~1 byte out), making N-byte input ≈ N-byte quota cost.

func TestQuota_BlocksOversum(t *testing.T) {
	upload, _, _ := newStack(t)
	owner := "key:test-id"

	if _, err := upload.Create(bytes.NewReader(htmlBody(6_000_000)), owner, "", ""); err != nil {
		t.Fatalf("first 6M upload: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(5_000_000)), owner, "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("second 5M should be over quota, got %v", err)
	}
}

func TestQuota_FreedByDelete(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	r1, err := upload.Create(bytes.NewReader(htmlBody(9_000_000)), owner, "", "")
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// At 9M used, a 3M upload would exceed.
	if _, err := upload.Create(bytes.NewReader(htmlBody(3_000_000)), owner, "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("should be over quota before delete, got %v", err)
	}
	// Delete frees the 9M.
	if err := manage.Delete(r1.Paste.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(3_000_000)), owner, "", ""); err != nil {
		t.Fatalf("after delete, 3M should fit: %v", err)
	}
}

func TestQuota_VersionsCount(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	// Upload a 6M paste, then update with another 6M. Each version
	// row counts toward the identity's active bytes, so total = 12M >
	// 10 MiB → second update should fail.
	r, err := upload.Create(bytes.NewReader(htmlBody(6_000_000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(6_000_000)), ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("second 6M version should be over quota, got %v", err)
	}
}

func TestQuota_PerIdentityIndependent(t *testing.T) {
	upload, _, _ := newStack(t)

	if _, err := upload.Create(bytes.NewReader(htmlBody(9_000_000)), "key:alice", "", ""); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(9_000_000)), "key:bob", "", ""); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// Alice can't upload more without freeing.
	if _, err := upload.Create(bytes.NewReader(htmlBody(2_000_000)), "key:alice", "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("alice second should be over quota, got %v", err)
	}
}

func TestQuota_PerPasteCapEqualsIdentityCap(t *testing.T) {
	upload, _, _ := newStack(t)
	// 12 MiB of high-entropy bytes → compresses to >10 MiB → rejected
	// by the compressed-cap gate. Use a size comfortably above the cap
	// to avoid flakiness from rounding.
	if _, err := upload.Create(bytes.NewReader(htmlBody(12<<20)), "key:id", "", ""); !errors.Is(err, service.ErrCompressedTooLarge) {
		t.Fatalf("oversize paste should reject with ErrCompressedTooLarge, got %v", err)
	}
}
