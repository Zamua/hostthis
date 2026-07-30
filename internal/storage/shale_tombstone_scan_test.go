//go:build slatedb

package storage_test

// A DELETED key must be invisible to the scan path, exactly as it already is
// to cluster.Get - while a BARE legacy empty marker must still come through.
//
// At R>1 shale does not remove a key on delete: it writes an empty-payload
// tombstone envelope under the commit stamp. cluster.Get resolves that to
// not-found, but a raw ScanPrefix hands the stored bytes back, so without a
// tombstone check every deleted record reappears to every scan consumer as an
// item whose value is empty - and empty does not read as "absent" to a
// consumer, it reads as CORRUPT (json.Unmarshal gives "unexpected end of JSON
// input"). A deleted paste therefore became a permanent phantom corrupt row
// that no retry could repair, re-found on every reconcile pass for as long as
// the tombstone lived. Deletes are ordinary traffic, so the set only grew.
//
// The tombstone is CONSTRUCTED here rather than produced by calling Delete,
// deliberately: shale only tombstones at R>1 ("At R=1 the raw tx.Put /
// tx.Delete path is unchanged"), and the shared test helper opens an R=1
// cluster. A Delete on an R=1 repo removes the key outright, so a test written
// that way would pass whether or not the fix were present - it would assert
// nothing at all. Writing the exact bytes an R>1 delete leaves keeps the test
// honest on the cheap R=1 fixture, and the setup asserts the shape it depends
// on rather than trusting it.
//
//	go test -tags slatedb -run TestShaleTombstone ./internal/storage

import (
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/shale/pkg/cluster"
)

func TestShaleTombstone_DeletedRowIsInvisibleToScan(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale tombstone test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	owner := "key:tombstone"
	slug := domain.Slug("tombps01")
	sha := "sha-live-version"

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: sha, Size: 100,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode paste: %v", err)
	}
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))

	stamp := cluster.Stamp{TimestampNanos: uint64(now.UnixNano()), NodeID: "node-1"}

	// Version 1: LIVE, wrapped in an envelope as an R>1 write leaves it. Its
	// presence is what distinguishes "the scan skipped the tombstone" from "the
	// scan returned nothing at all" - the latter would satisfy a weaker
	// assertion for entirely the wrong reason.
	liveVal, err := storage.LegacyVersionValueForTest(1, p.Kind, sha, int(p.Size), now, false)
	if err != nil {
		t.Fatalf("encode live version: %v", err)
	}
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1),
		cluster.Encode(cluster.Envelope{Stamp: stamp, Payload: liveVal}))

	// Version 2: a TOMBSTONE. Stamped envelope, no payload.
	tombstone := cluster.Encode(cluster.Envelope{Stamp: stamp, Payload: nil})
	if len(tombstone) == 0 || tombstone[0] != 0xE0 {
		t.Fatalf("test setup: expected an LWW envelope (magic 0xE0), got %#v", tombstone)
	}
	// Pin the two properties the whole test rests on, rather than assuming
	// them: the constructed value really does decode to an empty payload, and
	// it really does carry a stamp (a zero stamp would make it a BARE value,
	// which is legitimately live legacy data and correctly not skipped).
	env, derr := cluster.Decode(tombstone)
	if derr != nil {
		t.Fatalf("test setup: tombstone must decode: %v", derr)
	}
	if len(env.Payload) != 0 {
		t.Fatalf("test setup: tombstone payload must be EMPTY, got %q", env.Payload)
	}
	if env.Stamp == (cluster.Stamp{}) {
		t.Fatal("test setup: tombstone must carry a non-zero stamp, else it is a bare legacy value " +
			"and this test would be asserting the wrong case entirely")
	}
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 2), tombstone)

	// ListVersions walks the version rows through the scan path.
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("ListVersions with a tombstoned version present: %v; a tombstone must be SKIPPED, "+
			"not surfaced as an undecodable row", err)
	}

	var sawLive, sawTombstoned bool
	for _, v := range vers {
		switch v.VerNum {
		case 1:
			sawLive = true
		case 2:
			sawTombstoned = true
		}
	}
	if !sawLive {
		t.Fatalf("the LIVE version is missing from the scan, so this test is not exercising what it "+
			"claims: a scan returning nothing would pass the tombstone assertion vacuously. got %+v", vers)
	}
	if sawTombstoned {
		t.Fatalf("the tombstoned version is still visible to the scan. An empty-payload STAMPED envelope "+
			"is a DELETED key: cluster.Get already reports it not-found, and a scan that surfaces it turns "+
			"every delete into a permanent phantom row that decodes as corrupt and can never be repaired. "+
			"got %+v", vers)
	}
}
