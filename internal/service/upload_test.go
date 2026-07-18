package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
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
	u := NewUpload(storage.NewPasteRepo(db), NewStandaloneBlobUnit(blobs))
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
	// Expiry should be the default retention window from now.
	if res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt) != domain.DefaultRetentionWindow {
		t.Fatalf("expiry: got %v, want %v", res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt), domain.DefaultRetentionWindow)
	}
}

// TestUpload_Retention pins the configurable-retention behavior: the Upload
// service stamps ExpiresAt from its Retention policy, so a custom window and
// the no-expiry policy both flow through to the created paste.
func TestUpload_Retention(t *testing.T) {
	t.Run("custom window", func(t *testing.T) {
		u := newRealStack(t)
		u.Retention = domain.Retention{Window: 7 * 24 * time.Hour}
		res, err := u.Create(bytes.NewReader([]byte("<p>hi</p>")), "k", "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt); got != 7*24*time.Hour {
			t.Fatalf("expiry delta: got %v, want 7 days", got)
		}
	})

	t.Run("no expiry stamps the NeverExpires sentinel", func(t *testing.T) {
		u := newRealStack(t)
		u.Retention = domain.Retention{Window: 0}
		res, err := u.Create(bytes.NewReader([]byte("<p>hi</p>")), "k", "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !res.Paste.ExpiresAt.Equal(domain.NeverExpires) {
			t.Fatalf("expiry: got %v, want NeverExpires %v", res.Paste.ExpiresAt, domain.NeverExpires)
		}
	})
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
	if !res.Paste.ExpiresAt.Equal(now.Add(domain.DefaultRetentionWindow)) {
		t.Fatalf("ExpiresAt: got %v, want %v",
			res.Paste.ExpiresAt, now.Add(domain.DefaultRetentionWindow))
	}
}

// slugTakenNTimesRepo wraps a repo, failing the first `failures` inserts
// with a slug-collision error so the remint loop runs deterministically.
type slugTakenNTimesRepo struct {
	PasteRepo
	mu       sync.Mutex
	failures int
}

func (r *slugTakenNTimesRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	r.mu.Lock()
	fail := r.failures > 0
	if fail {
		r.failures--
	}
	r.mu.Unlock()
	if fail {
		// Wrapped, not bare: the remint loop must detect the sentinel
		// through wrapping (errors.Is), the way a backend may surface it.
		return fmt.Errorf("insert: %w", storage.ErrSlugTaken)
	}
	return r.PasteRepo.InsertWithQuotaCheck(ctx, p, userCap, now)
}

// TestUpload_Create_LogsSlugRemint pins the remint observability: each
// slug-collision retry inside Create logs one line (slug + attempt number),
// so silent remints - which leave committed orphan row-sets on backends
// whose insert retry can misread its own committed write - are never
// invisible. The remint semantics themselves are unchanged.
func TestUpload_Create_LogsSlugRemint(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &slugTakenNTimesRepo{PasteRepo: storage.NewPasteRepo(db), failures: 2}
	u := NewUpload(repo, NewStandaloneBlobUnit(newFakeBlobs()))
	var buf bytes.Buffer
	u.Logger = log.New(&buf, "", 0)

	res, err := u.Create(bytes.NewReader([]byte("# remint me")), "key:owner", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Slug == "" {
		t.Fatal("create returned no slug")
	}
	// Reading the log buffer races the background finalizer's logf; drain it.
	u.WaitFinalize()

	logged := buf.String()
	if got := strings.Count(logged, "re-minting"); got != 2 {
		t.Fatalf("remint log lines: got %d, want 2\nlog:\n%s", got, logged)
	}
	for _, want := range []string{"attempt 1/5", "attempt 2/5"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("remint log missing %q\nlog:\n%s", want, logged)
		}
	}
}
