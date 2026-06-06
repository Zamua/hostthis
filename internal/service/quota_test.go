package service_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
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
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)
	upload := service.NewUpload(repo, blobs)
	manage := service.NewManage(repo, blobs)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }
	manage.Now = func() time.Time { return now }
	return upload, manage, repo
}

// htmlBody returns an HTML-detected payload of approximately `n` bytes.
func htmlBody(n int) []byte {
	const head = "<!doctype html><body>"
	if n <= len(head) {
		return []byte(head[:n])
	}
	return []byte(head + strings.Repeat("x", n-len(head)))
}

func TestQuota_BlocksOversum(t *testing.T) {
	upload, _, _ := newStack(t)
	owner := "key:test-id"

	if _, err := upload.Create(htmlBody(600_000), owner, "", ""); err != nil {
		t.Fatalf("first 600k upload: %v", err)
	}
	if _, err := upload.Create(htmlBody(500_000), owner, "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("second 500k should be over quota, got %v", err)
	}
}

func TestQuota_FreedByDelete(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	r1, err := upload.Create(htmlBody(900_000), owner, "", "")
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// At 900k used, a 300k upload would exceed.
	if _, err := upload.Create(htmlBody(300_000), owner, "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("should be over quota before delete, got %v", err)
	}
	// Delete frees the 900k.
	if err := manage.Delete(r1.Paste.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := upload.Create(htmlBody(300_000), owner, "", ""); err != nil {
		t.Fatalf("after delete, 300k should fit: %v", err)
	}
}

func TestQuota_VersionsCount(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	// Upload a 600k paste, then update with another 600k. Each version
	// row counts toward the identity's active bytes, so total = 1.2M >
	// 1 MiB → second update should fail. (Today's bug — pre-fix, this
	// silently let updates accumulate version disk.)
	r, err := upload.Create(htmlBody(600_000), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, htmlBody(600_000), ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("second 600k version should be over quota, got %v", err)
	}
}

func TestQuota_PerIdentityIndependent(t *testing.T) {
	upload, _, _ := newStack(t)

	if _, err := upload.Create(htmlBody(900_000), "key:alice", "", ""); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := upload.Create(htmlBody(900_000), "key:bob", "", ""); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// Alice can't upload more without freeing.
	if _, err := upload.Create(htmlBody(200_000), "key:alice", "", ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("alice second should be over quota, got %v", err)
	}
}

func TestQuota_PerPasteCapEqualsIdentityCap(t *testing.T) {
	upload, _, _ := newStack(t)
	if _, err := upload.Create(htmlBody(domain.MaxPasteBytes+1), "key:id", "", ""); err == nil {
		t.Fatalf("oversize paste should reject")
	}
}
