package storage_test

// A deleted key must be invisible to the scan path, as it already is to
// cluster.Get, while a bare legacy empty marker must still come through.
//
// The tombstone is CONSTRUCTED rather than produced by Delete: shale only
// tombstones at R>1, and the shared helper opens an R=1 cluster where Delete
// removes the key outright, so a Delete-based test would assert nothing.

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/shale/pkg/cluster"
)

func TestShaleTombstone_DeletedRowIsInvisibleToScan(t *testing.T) {
	repo := newShaleRepoForTest(t)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	owner := "key:tombstone"
	slug := domain.Slug("tombps01")
	sha := "sha-live-version"

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: sha, Size: 100,
		CreatedAt: now, UpdatedAt: now}
	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode paste: %v", err)
	}
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))

	stamp := cluster.Stamp{TimestampNanos: uint64(now.UnixNano()), NodeID: "node-1"}

	// Version 1: LIVE, enveloped as an R>1 write leaves it. Its presence is what
	// keeps the tombstone assertion from passing vacuously on an empty scan.
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
	// A zero stamp would make this a BARE value - legitimately live legacy data,
	// correctly not skipped - so pin both the empty payload and the stamp.
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

	// ListVersions is the vehicle: it walks the version rows through the scan path.
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
