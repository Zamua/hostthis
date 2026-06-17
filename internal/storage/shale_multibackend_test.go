//go:build slatedb

package storage_test

// Multi-backend (sharded) ShaleRepo test: pins that UnitCount > 0 opens the
// sharded path and round-trips pastes across the units. Needs the slatedb build
// tag + MinIO (MINIO_TEST_ENDPOINT); skips cleanly otherwise. The full
// multi-node + 16-shard footprint validation is the staging dry-run.

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleMultiBackend_ShardedRoundTrip(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale multi-backend test")
	}

	cfg := uniqueShaleConfig(endpoint)
	cfg.UnitCount = 4 // sharded: 4 independent slatedb units, routed per key
	repo, err := storage.NewShaleRepo(cfg)
	if err != nil {
		t.Fatalf("NewShaleRepo (multi-backend UnitCount=4): %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	slugs := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		slug := fmt.Sprintf("mb-%03d", i)
		// insertPending inserts + Gets + asserts Pending; many slugs hash to
		// different units, so this exercises the sharded routing end to end.
		insertPending(t, repo, fmt.Sprintf("owner-%d", i%5), slug, 100+i, now)
		slugs = append(slugs, slug)
	}

	// Re-read every paste: confirms they coexist across the unit databases
	// (not just readable immediately after their own insert).
	for _, slug := range slugs {
		got, err := repo.Get(domain.Slug(slug))
		if err != nil {
			t.Fatalf("re-get %q from sharded repo: %v", slug, err)
		}
		if got.Status != domain.PasteStatusPending {
			t.Fatalf("re-get %q status = %v, want pending", slug, got.Status)
		}
	}
	t.Logf("multi-backend (UnitCount=4): %d pastes round-tripped across the shards", len(slugs))
}
