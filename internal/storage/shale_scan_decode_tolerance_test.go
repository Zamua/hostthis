//go:build slatedb

package storage_test

// Decode-tolerance policy tests for the shale background scans, pinned against
// a real ShaleRepo on the shared MinIO test bucket. docs/SPEC.md "Decode
// tolerance is per-scan-semantics" defines the three policies:
//
//   - Policy 1 (idempotent sweeps/reconcile): an undecodable record is SKIPPED
//     and logged, the pass continues over the good records and returns success,
//     and the next tick retries the bad row.
//
//   - Policy 2 (blob-GC ref-set scan): an undecodable record must FAIL CLOSED.
//     ReferencedBlobSHAs returns an error, never a partial keep-set, so the
//     sweep deletes NOTHING that pass. Skipping would under-count references
//     and delete a live blob, which is irreversible.
//
//   - Policy 2 (user-facing reads): a read of a corrupt record HARD FAILS. The
//     user sees an error, not a silent skip.
//
//	go test -tags slatedb -run TestShaleDecodeTolerance ./internal/storage
//
// All skip cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// corruptJSON fails json.Unmarshal into pasteRow, versionRow and the
// reservation marker alike: a bare token that is not a JSON object.
var corruptJSON = []byte("this-is-not-json{")

// --- Policy 1: idempotent reconcile skips + continues -----------------------

// --- Policy 1: reconciler heals an un-indexed undecodable row ---------------

// --- Policy 2: blob-GC ref scan fails closed --------------------------------

// --- Policy 2: user-facing reads still hard-fail ----------------------------

// TestShaleDecodeTolerance_UserReadHardFails pins Policy 2: a user read of a
// corrupt record surfaces an error, so the tolerant background scans cannot
// have leaked their skip behavior into the per-request read path.
func TestShaleDecodeTolerance_UserReadHardFails(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale decode-tolerance test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := domain.Paste{
		Slug: domain.Slug("readpst1"), Identity: domain.Identity("key:read"),
		Kind: domain.KindHTML, ContentSHA: "sha-read", Size: 100,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}

	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(p.Slug), corruptJSON); err != nil {
		t.Fatalf("corrupt paste row: %v", err)
	}

	got, err := repo.Get(p.Slug)
	if err == nil {
		t.Fatalf("Get of a corrupt row must hard-fail; got paste=%+v, nil error", got)
	}
}

// --- helpers ---------------------------------------------------------------
