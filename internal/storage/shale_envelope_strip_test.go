//go:build slatedb

package storage_test

// A value written at ReplicationFactor>1 is stored wrapped in an LWW envelope
// (magic byte + Stamp + payload). The cluster layer unwraps only on the
// single-key Get path: single-shard ScanPrefix and raw CAS tx.Get both hand
// back the raw stored bytes, which their consumers decode as JSON or integers.
// One enveloped record therefore poisons every listing and quota figure unless
// the scan paths strip it.
//
// Pins that they do: drop the strip in scanPrefix and ListVersions / the
// cached-size repair fail with "invalid character" decode errors.
//
//	go test -tags slatedb -run TestShaleEnvelopeStrip ./internal/storage
//
// Skips unless MINIO_TEST_ENDPOINT is set.

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
	}
	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode paste: %v", err)
	}
	verVal, err := storage.LegacyVersionValueForTest(1, p.Kind, sha, int(p.Size), now, false)
	if err != nil {
		t.Fatalf("encode version: %v", err)
	}

	// The shape an R>1 write leaves on the backend.
	enveloped := cluster.Encode(cluster.Envelope{
		Stamp:   cluster.Stamp{TimestampNanos: uint64(now.UnixNano()), NodeID: "node-1"},
		Payload: verVal,
	})
	if len(enveloped) == 0 || enveloped[0] != 0xE0 {
		t.Fatalf("test setup: expected an LWW envelope (magic 0xE0), got %#v", enveloped[:1])
	}

	// Paste + slug-owner rows raw; the version row enveloped (the shape
	// under test). All authoritative, no derived counters.
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

	// The repair-on-read path: ListByOwner reads the enveloped version rows to
	// recompute the live sum, so a missing strip surfaces as a WRONG cached
	// size rather than a decode error the caller sees. Seed a deliberately
	// wrong cached size first, or the repair has nothing to correct and the
	// assertion passes whether or not the strip works.
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	writeIndexEntryJSON(t, repo, idxKey, 1, now)
	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list over an enveloped version row: %v", err)
	}
	if got := readCachedIndexSize(t, repo, idxKey); got != p.Size {
		t.Fatalf("the repair must decode the enveloped version row and correct the cached sum: got %d, want %d", got, p.Size)
	}
}
