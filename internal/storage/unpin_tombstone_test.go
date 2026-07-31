package storage

// Pins the rule that "unpinned serves the latest version" means the latest
// LIVE one: rolling the head onto a tombstone would either 404 the URL or
// re-serve bytes the owner deleted.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Truncated to the second so values round-trip through the RFC3339 column
// encoding without sub-second drift.
var testNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

func TestUnpinSkipsTombstonedVersion(t *testing.T) {
	repo, _ := newTestDB(t)

	p := domain.Paste{
		Slug:       "unpin001",
		Identity:   "key:alice",
		Kind:       domain.KindHTML,
		ContentSHA: "sha-v1",
		Size:       10,
		CreatedAt:  testNow,
		UpdatedAt:  testNow,
		ExpiresAt:  testNow.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, testNow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), p.Slug, domain.KindHTML, "sha-v2", 20, 0, testNow); err != nil {
		t.Fatalf("append v2: %v", err)
	}

	v1, err := repo.GetVersion(p.Slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(p.Slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// v2 is no longer served, so the service layer permits tombstoning it.
	if err := repo.DeleteVersion(p.Slug, 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}

	if err := repo.Unpin(p.Slug); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PinnedVersion != 0 {
		t.Fatalf("PinnedVersion after unpin: got %d, want 0", got.PinnedVersion)
	}
	if got.ContentSHA != "sha-v1" {
		t.Fatalf("head rolled onto a tombstoned version: ContentSHA %q, want %q", got.ContentSHA, "sha-v1")
	}
	if got.Size != 10 {
		t.Fatalf("head size: got %d, want 10", got.Size)
	}
}

// A paste whose only versions are tombstoned has nothing live to serve, so
// Unpin refuses rather than pointing the head at a deleted version.
func TestUnpinAllVersionsTombstoned(t *testing.T) {
	repo, _ := newTestDB(t)

	p := domain.Paste{
		Slug:       "unpin002",
		Identity:   "key:alice",
		Kind:       domain.KindHTML,
		ContentSHA: "sha-v1",
		Size:       10,
		CreatedAt:  testNow,
		UpdatedAt:  testNow,
		ExpiresAt:  testNow.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, testNow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.DeleteVersion(p.Slug, 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	if err := repo.Unpin(p.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpin with no live version: got %v, want ErrNotFound", err)
	}
}
