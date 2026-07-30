//go:build slatedb

package storage_test

// Orphan-prune TOCTOU (docs/SPEC.md "Derived indexes and repair-on-read"): the
// prune confirms a candidate entry's authoritative row is gone before dropping
// it, but a same-slug delete-then-redeploy can land a fresh row and entry
// BETWEEN the confirm and the delete. The delete is therefore value-compared:
// it drops the entry only while it still holds the value the pass's snapshot
// read, so the fresh entry survives and a stale one lingers at most one more
// pass.
//
//	go test -tags slatedb -run TestShaleOrphanPruneKeepsFreshRedeployEntry ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestShaleOrphanPruneKeepsFreshRedeployEntry pins that a same-slug redeploy
// landing between the prune's confirm and its delete survives, because the
// delete drops only the entry value the snapshot read.
func TestShaleOrphanPruneKeepsFreshRedeployEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale prune-TOCTOU test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	owner := "key:prunerace"
	slug := domain.Slug("prunerc1")
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())

	// A stale orphan entry, as a crash mid-delete leaves: its authoritative
	// row is gone, so the prune confirms NotFound and drops it.
	writeIndexEntryJSON(t, repo, idxKey, 777, now.Add(domain.DefaultRetentionWindow))

	// The same-slug redeploy lands between the prune's confirm and its
	// delete: a fresh authoritative row plus a fresh enumeration entry.
	var once sync.Once
	repo.SetBeforeOrphanPruneDeleteHookForTest(func(key []byte) {
		if !bytes.Equal(key, idxKey) {
			return
		}
		once.Do(func() {
			fresh := domain.Paste{
				Slug: slug, Identity: domain.Identity(owner),
				Kind: domain.KindHTML, ContentSHA: "sha-prunerace-v1", Size: 300,
				CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
			}
			if err := repo.InsertWithQuotaCheck(context.Background(), fresh, 0, now); err != nil {
				t.Errorf("racing redeploy: %v", err)
			}
			repo.WaitPendingConfirms()
		})
	})
	defer repo.SetBeforeOrphanPruneDeleteHookForTest(nil)

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The entry no longer holds the snapshot's value, so the value-compared
	// delete skips it.
	if got := readCachedIndexSize(t, repo, idxKey); got != 300 {
		t.Fatalf("prune dropped the fresh redeploy's entry: cached size got %d, want 300", got)
	}
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("sum after raced prune: got %d, want 300", got)
	}
}
