package service

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// newRealStack builds the upload service backed by real sqlite and
// real blob store under t.TempDir(). DDD payoff: this is the same
// stack that production runs; no mocks.
func newRealStack(t *testing.T) *Upload {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	u := NewUpload(storage.NewPasteRepo(db), blobs)
	// The blob write now finalizes in a background goroutine. Drain any
	// in-flight finalizers before the TempDir is torn down so the async
	// write does not race the cleanup (and so blob assertions are stable).
	t.Cleanup(u.WaitFinalize)
	return u
}

func TestUpload_Create_HTML(t *testing.T) {
	u := newRealStack(t)
	body := []byte("<!doctype html><p>hi</p>")
	res, err := u.Create(bytes.NewReader(body), "owner-key-hash", "demo", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind: got %q, want html", res.Paste.Kind)
	}
	if string(res.Paste.Identity) != "owner-key-hash" {
		t.Fatalf("identity: got %q, want %q", res.Paste.Identity, "owner-key-hash")
	}
	if res.Paste.Name != "demo" {
		t.Fatalf("name: got %q, want %q", res.Paste.Name, "demo")
	}
	// After compression, Size is the compressed (stored) byte count.
	// For short input zstd's header overhead can make it larger than
	// the original; only assert "positive + plausible".
	if res.Paste.Size <= 0 || res.Paste.Size > len(body)*2+64 {
		t.Fatalf("size: got %d, want positive ~within 2x of %d", res.Paste.Size, len(body))
	}
	if res.Paste.ContentSHA != domain.HashContent(body) {
		t.Fatalf("sha mismatch")
	}
	// Slug should be valid per the alphabet rules.
	if _, err := domain.ParseSlug(string(res.Paste.Slug)); err != nil {
		t.Fatalf("returned slug is invalid: %v", err)
	}
	// Expiry should be RetentionWindow from now.
	if res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt) != domain.RetentionWindow {
		t.Fatalf("expiry: got %v, want %v", res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt), domain.RetentionWindow)
	}
}

func TestUpload_Create_Markdown(t *testing.T) {
	u := newRealStack(t)
	res, err := u.Create(bytes.NewReader([]byte("# Title\n\nbody")), "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Kind != domain.KindMarkdown {
		t.Fatalf("kind: got %q, want markdown", res.Paste.Kind)
	}
	if res.Paste.Identity != "" {
		t.Fatalf("anonymous should have empty Identity, got %q", res.Paste.Identity)
	}
}

func TestUpload_Create_RejectsUnsupportedKind(t *testing.T) {
	u := newRealStack(t)
	_, err := u.Create(bytes.NewReader([]byte("\x89PNG\r\n\x1a\n...binary bytes...")), "", "", "")
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("err: got %v, want ErrUnsupportedKind", err)
	}
}

func TestUpload_Create_RejectsEmpty(t *testing.T) {
	u := newRealStack(t)
	_, err := u.Create(bytes.NewReader([]byte{}), "", "", "")
	if err == nil {
		t.Fatalf("empty upload should error")
	}
}

func TestUpload_Create_RejectsOversize(t *testing.T) {
	u := newRealStack(t)
	body := make([]byte, domain.MaxPasteBytes+1)
	body[0] = '<' // doesn't really matter, we should reject before sniffing
	_, err := u.Create(bytes.NewReader(body), "", "", "")
	if err == nil {
		t.Fatalf("oversize upload should error")
	}
}

func TestUpload_Create_HonorsHint(t *testing.T) {
	u := newRealStack(t)
	// "anything" doesn't look like html or markdown, but the hint
	// should force html acceptance.
	res, err := u.Create(bytes.NewReader([]byte("anything goes")), "", "", "html")
	if err != nil {
		t.Fatalf("create with html hint: %v", err)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind: got %q, want html", res.Paste.Kind)
	}
}

func TestUpload_Create_DedupsBlobOnSameBytes(t *testing.T) {
	u := newRealStack(t)
	body := []byte("<!doctype html><p>same</p>")
	r1, err := u.Create(bytes.NewReader(body), "", "", "")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	r2, err := u.Create(bytes.NewReader(body), "", "", "")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if r1.Paste.Slug == r2.Paste.Slug {
		t.Fatalf("expected distinct slugs, got %q twice", r1.Paste.Slug)
	}
	if r1.Paste.ContentSHA != r2.Paste.ContentSHA {
		t.Fatalf("same bytes should produce same content sha")
	}
}

func TestUpload_Create_TimestampStable(t *testing.T) {
	u := newRealStack(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	u.Now = func() time.Time { return now }
	res, err := u.Create(bytes.NewReader([]byte("<p>x")), "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Paste.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt: got %v, want %v", res.Paste.CreatedAt, now)
	}
	if !res.Paste.ExpiresAt.Equal(now.Add(domain.RetentionWindow)) {
		t.Fatalf("ExpiresAt: got %v, want %v",
			res.Paste.ExpiresAt, now.Add(domain.RetentionWindow))
	}
}
