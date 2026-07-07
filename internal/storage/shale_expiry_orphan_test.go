//go:build slatedb

package storage_test

// Orphaned expiry-index entry drain test for the shale backend.
//
// An expiry/<ts>/<slug> entry whose paste record is ALREADY GONE (the state
// a legacy TTL-era deployment can leave behind: the entry was written at
// paste-create, but the record later vanished without the index cleanup
// that normally rides the paste-delete cascade) must be REMOVED by one
// expiry pass. Without that, the entry resurfaces on every pass forever:
// the scan returns its slug, the paste-delete no-ops on the missing record,
// and nothing ever touches the entry itself.
//
// This pins the drain against the REAL backend: the orphan entry is planted
// through the raw-write path (no paste row), then one pass over the sweep
// repo surface must leave the index empty.
//
//	go test -tags slatedb -run TestShaleSweep_OrphanExpiryIndexEntryDrains ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set, mirroring the
// conformance files.

import (
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleSweep_OrphanExpiryIndexEntryDrains(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale orphan-expiry test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	expiresAt := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(time.Hour)
	orphan := domain.Slug("gone1234")

	// Plant the orphan: an expiry-index entry with NO paste record behind it.
	mustPutRaw(t, repo, storage.LegacyExpiryKeyForTest(expiresAt, orphan), storage.MarkerValueForTest())

	// And a genuinely-expired LIVE paste next to it, so the pass exercises
	// both shapes: real deletion and orphan cleanup.
	live := pasteOf("live1234", "key:orph", 10)
	live.ExpiresAt = expiresAt
	insert(t, repo, live)

	// One expiry pass over the sweep repo surface.
	refs, err := repo.ExpiredPastes(now)
	if err != nil {
		t.Fatalf("ExpiredPastes: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("scan should surface the orphan entry AND the live paste, got %v", refs)
	}
	deleted := 0
	for _, ref := range refs {
		ok, err := repo.DeleteExpired(ref)
		if err != nil {
			t.Fatalf("DeleteExpired(%q): %v", ref.Slug, err)
		}
		if ok {
			deleted++
		}
	}
	// Only the live paste was an actual record deletion; the orphan entry
	// was an index cleanup, not a paste deletion.
	if deleted != 1 {
		t.Fatalf("exactly one paste record should have been deleted, got %d", deleted)
	}

	// The pass DRAINED the index: a second scan sees zero expired entries.
	again, err := repo.ExpiredPastes(now)
	if err != nil {
		t.Fatalf("ExpiredPastes (second pass): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("expiry index must drain in one pass; %d entr(ies) remain: %v", len(again), again)
	}

	// The live paste's record is really gone too.
	if _, err := repo.Get(live.Slug); err == nil {
		t.Fatalf("expired live paste should have been deleted")
	}
}
