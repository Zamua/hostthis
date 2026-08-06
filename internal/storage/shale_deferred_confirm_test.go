//go:build slatedb

package storage_test

// Confirm-step tests for the shale backend.
//
// InsertWithQuotaCheck commits the authoritative {slug} write, then runs the
// confirm CAS (the value-bearing identity_pastes entry + first-seen)
// synchronously on the response path, because that entry is the quota's
// accounting record (docs/SPEC.md "Scan-derived quota"). Pins the observable
// consequences: the paste is Get-readable on return, so the URL never 404s;
// its bytes are counted on return; and the derived entry backing ListByOwner /
// CountByOwner is present after WaitPendingConfirms (a no-op drain, kept as the
// seam should a write move off the response path).
//
//	go test -tags slatedb -run TestShaleDeferredConfirm ./internal/storage
//
// Skips unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleDeferredConfirm_ReadableImmediately_IndexAppearsAfterDrain(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale deferred-confirm test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:deferred"
	p := domain.Paste{
		Slug: domain.Slug("deferab1"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-deferred", Size: 300,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Get resolves without waiting on confirm, so a public URL never 404s.
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("Get immediately after insert (URL must not 404): %v", err)
	}
	if got.Slug != p.Slug || got.ContentSHA != p.ContentSHA || got.Size != p.Size {
		t.Fatalf("Get round-trip mismatch: got %+v want slug=%q sha=%q size=%d", got, p.Slug, p.ContentSHA, p.Size)
	}

	// The bytes are counted the instant Insert returns: the index entry whose
	// cached size the quota scan sums is written before the response.
	if sum, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("sum active bytes: %v", err)
	} else if sum != p.Size {
		t.Fatalf("active bytes immediately after insert = %d, want %d (reserve runs before the response)", sum, p.Size)
	}

	// After the drain the derived entry is written, so the paste appears in
	// the owner's list, count, and first-seen.
	repo.WaitPendingConfirms()

	if n, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("count by owner: %v", err)
	} else if n != 1 {
		t.Fatalf("CountByOwner after drain = %d, want 1 (deferred confirm wrote the index)", n)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(list) != 1 || list[0].Slug != p.Slug {
		t.Fatalf("ListByOwner after drain = %+v, want just %q", list, p.Slug)
	}
	if first, err := repo.OwnerFirstSeen(owner); err != nil {
		t.Fatalf("owner first seen: %v", err)
	} else if !first.Equal(now) {
		t.Fatalf("OwnerFirstSeen after drain = %v, want %v (confirm sets first-seen)", first, now)
	}
}

// TestShaleDeferredConfirm_ReconcilerHealsLostConfirm pins that a lost confirm
// is healed: the reconciler rebuilds the missing identity_pastes entry. The
// lost confirm is modeled by dropping the entry after a successful insert,
// which leaves the same end state (authoritative paste present, no entry).
func TestShaleDeferredConfirm_ReconcilerHealsLostConfirm(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale deferred-confirm reconcile test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:lostconfirm"
	p := domain.Paste{
		Slug: domain.Slug("lostcfm1"), Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-lostconfirm", Size: 250,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// Drop the entry: the state a crash between the authoritative write and
	// the confirm leaves.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, p.Slug.String())); err != nil {
		t.Fatalf("drop index entry to model lost confirm: %v", err)
	}
	if n, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("count after drop: %v", err)
	} else if n != 0 {
		t.Fatalf("post-drop count = %d, want 0 (index entry gone)", n)
	}

	// The rebuild is from the authoritative row.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("count after reconcile: %v", err)
	} else if n != 1 {
		t.Fatalf("post-reconcile count = %d, want 1 (reconciler rebuilt the lost-confirm index entry)", n)
	}
}
