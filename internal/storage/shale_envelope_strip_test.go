//go:build slatedb

package storage_test

// Envelope-strip regression for the shale backend.
//
// A value written at ReplicationFactor>1 is stored wrapped in an LWW
// envelope (magic byte + Stamp + payload). The cluster layer unwraps on the
// single-key Get path, but NOT on the scan paths (cross-shard Aggregate,
// single-shard ScanPrefix) nor on raw CAS tx.Get reads - those hand back the
// raw stored bytes. hostthis's aggregate / scan / counter consumers decode
// those raw reads as JSON / integers, so a single enveloped record poisoned
// EVERY service-wide quota pre-check and listing the moment a 2-node deploy
// wrote one (the 2026-06-12 R=2 cutover incident).
//
// This pins the fix: the scan paths strip the envelope, so an enveloped
// version row round-trips exactly like a raw one. Revert any of the strips in
// scanPrefix / aggregatePrefix and ListVersions / ReferencedBlobSHAs below
// fail with "invalid character" decode errors.
//
//	go test -tags slatedb -run TestShaleEnvelopeStrip ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set, mirroring the conformance
// + migration files.

import (
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/shale/pkg/cluster"
)

func TestShaleEnvelopeStrip_ScanPathsDecodeEnvelopedValue(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping envelope-strip test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("envel123")
	owner := "key:enveloped"
	sha := "sha-enveloped-v1"

	p := domain.Paste{
		Slug:       slug,
		Identity:   domain.Identity(owner),
		Kind:       domain.KindMarkdown,
		ContentSHA: sha,
		Size:       99,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(domain.DefaultRetentionWindow),
	}
	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode paste: %v", err)
	}
	verVal, err := storage.LegacyVersionValueForTest(1, p.Kind, sha, int(p.Size), now, false)
	if err != nil {
		t.Fatalf("encode version: %v", err)
	}

	// Wrap the version row in an LWW envelope - exactly the shape an R>1 write
	// leaves on the backend.
	enveloped := cluster.Encode(cluster.Envelope{
		Stamp:   cluster.Stamp{TimestampNanos: uint64(now.UnixNano()), NodeID: "node-1"},
		Payload: verVal,
	})
	if len(enveloped) == 0 || enveloped[0] != 0xE0 {
		t.Fatalf("test setup: expected an LWW envelope (magic 0xE0), got %#v", enveloped[:1])
	}

	// Paste + slug-owner rows raw (denormalized head); the VERSION row
	// enveloped (the poison shape). All authoritative, no derived counters.
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), enveloped)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))

	// scanPrefix path: ListVersions decodes the enveloped version row.
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("ListVersions over an enveloped version row: %v", err)
	}
	if len(vers) != 1 || vers[0].VerNum != 1 || vers[0].ContentSHA != sha || vers[0].Size != p.Size {
		t.Fatalf("enveloped version did not round-trip via scanPrefix: %+v", vers)
	}

	// aggregatePrefix path: ReferencedBlobSHAs is the cross-shard scan the
	// blob GC + service-wide quota pre-check run on; it must decode it too.
	refs, err := repo.ReferencedBlobSHAs()
	if err != nil {
		t.Fatalf("ReferencedBlobSHAs over an enveloped version row: %v", err)
	}
	if !sliceHasMig(refs, sha) {
		t.Fatalf("enveloped version sha must be referenced: %v should contain %q", refs, sha)
	}
}
