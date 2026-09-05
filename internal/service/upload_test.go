package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

func TestUpload_Create_HTML(t *testing.T) {
	u, _, _ := newStack(t)
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
	// Size is the compressed (stored) byte count; for short input zstd's header
	// overhead can exceed the original, so only assert positive + plausible.
	if res.Paste.Size <= 0 || res.Paste.Size > len(body)*2+64 {
		t.Fatalf("size: got %d, want positive ~within 2x of %d", res.Paste.Size, len(body))
	}
	if res.Paste.ContentSHA != sha256Hex(body) {
		t.Fatalf("sha mismatch")
	}
	if _, err := domain.ParseSlug(string(res.Paste.Slug)); err != nil {
		t.Fatalf("returned slug is invalid: %v", err)
	}
	if !res.Paste.CreatedAt.Equal(fixedNow) {
		t.Fatalf("CreatedAt: got %v, want the injected clock %v", res.Paste.CreatedAt, fixedNow)
	}
}

func TestUpload_Create_Markdown(t *testing.T) {
	u, _, _ := newStack(t)
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
	u, _, _ := newStack(t)
	_, err := u.Create(bytes.NewReader([]byte("\x89PNG\r\n\x1a\n...binary bytes...")), "", "", "")
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("err: got %v, want ErrUnsupportedKind", err)
	}
}

func TestUpload_Create_RejectsEmpty(t *testing.T) {
	u, _, _ := newStack(t)
	_, err := u.Create(bytes.NewReader([]byte{}), "", "", "")
	if err == nil {
		t.Fatalf("empty upload should error")
	}
}

func TestUpload_Create_HonorsHint(t *testing.T) {
	u, _, _ := newStack(t)
	// "anything goes" looks like neither html nor markdown; the hint forces
	// html acceptance.
	res, err := u.Create(bytes.NewReader([]byte("anything goes")), "", "", "html")
	if err != nil {
		t.Fatalf("create with html hint: %v", err)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind: got %q, want html", res.Paste.Kind)
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
// slug-collision retry inside Create logs one line, so silent remints, which
// leave committed orphan row-sets on backends whose insert retry can misread
// its own committed write, are never invisible.
func TestUpload_Create_LogsSlugRemint(t *testing.T) {
	repo := &slugTakenNTimesRepo{PasteRepo: storagetest.NewRepo(t), failures: 2}
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

	if lines := strings.Split(strings.TrimSpace(buf.String()), "\n"); len(lines) != 2 {
		t.Fatalf("remint log lines: got %d, want one per collision (2)\nlog:\n%s", len(lines), buf.String())
	}
}
