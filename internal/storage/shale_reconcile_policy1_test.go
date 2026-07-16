//go:build slatedb

package storage_test

// Policy 1 for the reprojection WRITE loop (docs/SPEC.md "Decode tolerance
// is per-scan-semantics"): the reconciler treats a per-record failure as
// SKIP + LOG and CONTINUES the pass, so one bad entry's blast radius stays
// one entry. The write loop must honor that too: a failed index write is
// counted and skipped, every other wanted entry is still written, and the
// pass returns an aggregated error so the next tick retries.
//
// Failure is injected through the guarded-write fault seam (see
// shale_export_test.go) because a healthy single-node cluster offers no
// organic way to fail exactly one {id}-shard CAS.
//
//	go test -tags slatedb -run TestShaleReprojectionWriteLoopSkipsFailedEntries ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleReprojectionWriteLoopSkipsFailedEntries(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale policy-1 write-loop test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	owner := "key:pol1loop"
	slugs := []domain.Slug{"pol1aaaa", "pol1bbbb", "pol1cccc"}
	for i, slug := range slugs {
		p := domain.Paste{
			Slug: slug, Identity: domain.Identity(owner),
			Kind: domain.KindHTML, ContentSHA: fmt.Sprintf("sha-pol1-%d", i), Size: 100,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
		}
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", slug, err)
		}
	}
	repo.WaitPendingConfirms()

	// Drop all three entries so the pass must re-add each (three wanted
	// writes), then fail EVERY write: the loop must still ATTEMPT all
	// three (skip + continue), not abort on the first failure.
	var mu sync.Mutex
	attempted := make(map[string]bool)
	for _, slug := range slugs {
		if err := repo.DeleteRawForTest(storage.IdentityPasteKeyForTest(owner, slug.String())); err != nil {
			t.Fatalf("drop entry %s: %v", slug, err)
		}
	}
	repo.SetGuardedIndexWriteHookForTest(func(key []byte) error {
		mu.Lock()
		attempted[string(key)] = true
		mu.Unlock()
		return fmt.Errorf("injected write failure")
	})
	defer repo.SetGuardedIndexWriteHookForTest(nil)

	err := repo.ReconcileForTest(now)
	if err == nil {
		t.Fatalf("a pass with failed index writes must return an aggregated error")
	}
	if !strings.Contains(err.Error(), "3 identity_pastes") {
		t.Fatalf("the error must aggregate the failure count: got %v", err)
	}
	mu.Lock()
	got := len(attempted)
	mu.Unlock()
	if got != len(slugs) {
		t.Fatalf("the write loop must skip + continue past each failure: attempted %d of %d writes", got, len(slugs))
	}

	// Clear the fault: the next pass heals everything the failed one skipped.
	repo.SetGuardedIndexWriteHookForTest(nil)
	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("healing pass: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("sum after healing pass: got %d, want 300", got)
	}
}
