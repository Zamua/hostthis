//go:build slatedb

package storage_test

// Orphan-prune TOCTOU (docs/SPEC.md "Derived indexes and repair-on-read" /
// the reconciler's prune): the prune confirms a candidate entry's
// authoritative row is gone before dropping it, but a same-slug
// delete-then-redeploy can land a fresh row + entry BETWEEN the confirm
// and the delete. The delete is therefore value-compared: it drops the
// entry only while it still holds the value the pass's snapshot read, so
// the fresh entry survives; the stale entry then lingers at most one more
// pass (a Window-A-shaped residual the next pass heals).
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

// TestShaleOrphanPruneKeepsFreshRedeployEntry pins F5's TOCTOU: the orphan
// prune confirms the authoritative row is gone, then a same-slug redeploy
// lands a fresh row + entry before the delete. The delete must be
// value-compared (drop only the entry the snapshot read) so the fresh
// entry survives.
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

	// The stale orphan entry: its authoritative row is gone (a crash
	// mid-delete left it), so the prune will confirm NotFound and drop it.
	writeIndexEntryJSON(t, repo, idxKey, 777, now.Add(domain.DefaultRetentionWindow))

	// The same-slug redeploy lands between the prune's confirm and its
	// delete: a fresh authoritative row + a fresh enumeration entry (300).
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
	// The fresh redeploy's entry survives the prune (value-compared delete
	// skipped: the entry no longer holds the snapshot's value).
	if got := readCachedIndexSize(t, repo, idxKey); got != 300 {
		t.Fatalf("prune dropped the fresh redeploy's entry: cached size got %d, want 300", got)
	}
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("sum after raced prune: got %d, want 300", got)
	}
}
