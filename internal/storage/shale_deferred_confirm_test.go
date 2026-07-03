//go:build slatedb

package storage_test

// Deferred-confirm test for the shale backend.
//
// docs/SPEC.md "Reservation-pattern quota", step 3: InsertWithQuotaCheck
// returns success as soon as the reserve (step 1, bytes accounted) and the
// authoritative write (step 2, paste row exists) commit, and runs the
// confirm CAS (step 3: identity_pastes index entry + first-seen) in a
// background goroutine OFF the response path. This pins the observable
// consequences:
//
//   - the paste is Get-readable immediately on return (the authoritative
//     row exists, so the URL never 404s),
//   - the bytes are counted immediately (the reserve ran before return), so
//     quota is strict the instant Create returns,
//   - the derived index entry (which backs ListByOwner / CountByOwner)
//     appears once the deferred confirm runs; WaitPendingConfirms drains it
//     so a subsequent list is deterministic.
//
//	go test -tags slatedb -run TestShaleDeferredConfirm ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

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
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The authoritative paste row exists the instant Insert returns: Get
	// resolves it (so a public URL never 404s) WITHOUT waiting on confirm.
	got, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("Get immediately after insert (URL must not 404): %v", err)
	}
	if got.Slug != p.Slug || got.ContentSHA != p.ContentSHA || got.Size != p.Size {
		t.Fatalf("Get round-trip mismatch: got %+v want slug=%q sha=%q size=%d", got, p.Slug, p.ContentSHA, p.Size)
	}

	// The bytes are counted the instant Insert returns: the reserve (step 1)
	// committed before the response, so quota is strict immediately. This
	// reads identity_bytes, which is written synchronously (NOT deferred).
	if sum, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("sum active bytes: %v", err)
	} else if sum != p.Size {
		t.Fatalf("active bytes immediately after insert = %d, want %d (reserve runs before the response)", sum, p.Size)
	}

	// Drain the deferred confirm: the derived index entry is now written, so
	// the paste appears in the owner's list + count + first-seen.
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

// TestShaleDeferredConfirm_ReconcilerHealsLostConfirm pins that a LOST
// deferred confirm (the goroutine never ran / crashed) is healed by the
// reconciler: the missing identity_pastes index entry is rebuilt and the
// leaked reservation marker is what the grace-windowed pass would later
// drop. We model "confirm never ran" by inserting, draining, then dropping
// the index entry the confirm wrote (the same end state a lost confirm
// leaves: authoritative paste present, no index entry), and asserting the
// reconciler rebuilds it.
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
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// Model a confirm that never wrote its index entry: drop it. The
	// authoritative paste still exists, exactly the state a crash between
	// the authoritative write and the (deferred) confirm leaves.
	if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, p.Slug.String())); err != nil {
		t.Fatalf("drop index entry to model lost confirm: %v", err)
	}
	if n, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("count after drop: %v", err)
	} else if n != 0 {
		t.Fatalf("post-drop count = %d, want 0 (index entry gone)", n)
	}

	// The reconciler rebuilds the missing index entry from the authoritative
	// row, so the paste reappears in the owner's list.
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("count after reconcile: %v", err)
	} else if n != 1 {
		t.Fatalf("post-reconcile count = %d, want 1 (reconciler rebuilt the lost-confirm index entry)", n)
	}
}
