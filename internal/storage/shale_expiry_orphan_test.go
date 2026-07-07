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

// TestShaleSweep_VerbatimLegacyEntryKeysDrain pins the drain against REAL
// legacy fixtures: the exact key bytes of stuck production-era entries
// (one paste expiry entry, one site expiry entry), copied VERBATIM from an
// affected deployment's index. The scan must surface each entry with
// IndexRef equal to the stored bytes, and processing the reference must
// remove that exact entry - proving the key FORMAT (variable-width
// RFC3339Nano paste ts / fixed-width site ts) never blocks the drain when
// the entry is reachable by routed mutation. (When it is NOT reachable -
// data physically placed under a different sharding than live routing -
// no key-targeted delete can drain it; that state is handled by the
// sweep's convergence guard, pinned at the service level.)
func TestShaleSweep_VerbatimLegacyEntryKeysDrain(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale verbatim-fixture test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Verbatim stuck-entry fixtures (real observed key shapes; both
	// reference records that no longer exist).
	pasteEntry := "expiry/2026-07-01T03:20:59.990221663Z/8ajitdpm"
	siteEntry := "expiry_sites/2026-07-03T21:22:23.536246341Z/ctimu4qh"
	mustPutRaw(t, repo, []byte(pasteEntry), storage.MarkerValueForTest())
	mustPutRaw(t, repo, []byte(siteEntry), storage.MarkerValueForTest())

	// Paste side: the scan carries the exact stored bytes, and processing
	// the ref drains the entry in one pass.
	refs, err := repo.ExpiredPastes(now)
	if err != nil {
		t.Fatalf("ExpiredPastes: %v", err)
	}
	if len(refs) != 1 || refs[0].IndexRef != pasteEntry || refs[0].Slug != "8ajitdpm" {
		t.Fatalf("scan must surface the verbatim entry (IndexRef = stored bytes), got %+v", refs)
	}
	deleted, err := repo.DeleteExpired(refs[0])
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted {
		t.Fatalf("no paste record exists; DeleteExpired must report false")
	}
	if again, err := repo.ExpiredPastes(now); err != nil || len(again) != 0 {
		t.Fatalf("verbatim paste entry must drain in one pass: refs=%v err=%v", again, err)
	}

	// Site side: same contract over expiry_sites/.
	siteRefs, err := repo.ExpiredSites(now)
	if err != nil {
		t.Fatalf("ExpiredSites: %v", err)
	}
	if len(siteRefs) != 1 || siteRefs[0].IndexRef != siteEntry || siteRefs[0].Slug != "ctimu4qh" {
		t.Fatalf("site scan must surface the verbatim entry (IndexRef = stored bytes), got %+v", siteRefs)
	}
	siteDeleted, err := repo.DeleteExpiredSite(siteRefs[0])
	if err != nil {
		t.Fatalf("DeleteExpiredSite: %v", err)
	}
	if siteDeleted {
		t.Fatalf("no site record exists; DeleteExpiredSite must report false")
	}
	if again, err := repo.ExpiredSites(now); err != nil || len(again) != 0 {
		t.Fatalf("verbatim site entry must drain in one pass: refs=%v err=%v", again, err)
	}
}

// TestShaleSweep_OrphanRoomExpiryEntryDrains is the roomexpiry/ twin of the
// paste test above: a roomexpiry/<ts>/<app>/<uuid> entry whose room record
// is ALREADY GONE must be removed by one expiry pass. ExpiredRooms surfaces
// the entry with its exact stored key as the opaque IndexRef, and
// DeleteExpiredRoom removes that OBSERVED entry regardless of the missing
// record (reporting false - an index cleanup, not a room deletion). Without
// that, the orphan resurfaces on every pass forever: the scan returns its
// (app, id), the idempotent room delete no-ops on the missing record, and
// nothing ever touches the entry itself. A genuinely-expired LIVE room next
// to it pins the true-reporting cascade path in the same pass.
func TestShaleSweep_OrphanRoomExpiryEntryDrains(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale orphan-room-expiry test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	expiresAt := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(time.Hour)
	const app = domain.Slug("appz2345")

	// Plant the orphan: a roomexpiry entry with NO room record behind it.
	orphanID := domain.NewRoomID()
	orphanKey := storage.RoomExpiryKeyForTest(expiresAt, app, orphanID)
	mustPutRaw(t, repo, orphanKey, storage.MarkerValueForTest())

	// And a genuinely-expired LIVE room next to it, so the pass exercises
	// both shapes: real deletion and orphan cleanup.
	live := domain.Room{
		AppSlug:   app,
		ID:        domain.NewRoomID(),
		CreatedAt: expiresAt.Add(-time.Hour),
		UpdatedAt: expiresAt.Add(-time.Hour),
		ExpiresAt: expiresAt,
	}
	if err := repo.CreateRoom(live, "10.0.0.0/24", 0, live.CreatedAt); err != nil {
		t.Fatalf("create live room: %v", err)
	}

	// One expiry pass over the sweep repo surface.
	refs, err := repo.ExpiredRooms(now)
	if err != nil {
		t.Fatalf("ExpiredRooms: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("scan should surface the orphan entry AND the live room, got %v", refs)
	}
	deleted := 0
	for _, ref := range refs {
		// Every surfaced reference carries the EXACT observed entry key.
		if ref.AppSlug == app && ref.ID == orphanID && ref.IndexRef != string(orphanKey) {
			t.Fatalf("orphan ref must carry the verbatim stored key %q, got %q", orphanKey, ref.IndexRef)
		}
		ok, err := repo.DeleteExpiredRoom(ref)
		if err != nil {
			t.Fatalf("DeleteExpiredRoom(%s/%s): %v", ref.AppSlug, ref.ID, err)
		}
		if ok {
			deleted++
		}
	}
	// Only the live room was an actual record deletion; the orphan entry
	// was an index cleanup, not a room deletion.
	if deleted != 1 {
		t.Fatalf("exactly one room record should have been deleted, got %d", deleted)
	}

	// The pass DRAINED the index: a second scan sees zero expired entries.
	again, err := repo.ExpiredRooms(now)
	if err != nil {
		t.Fatalf("ExpiredRooms (second pass): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("room expiry index must drain in one pass; %d entr(ies) remain: %v", len(again), again)
	}

	// The live room's record is really gone too.
	if _, err := repo.GetRoom(live.AppSlug, live.ID); err == nil {
		t.Fatalf("expired live room should have been deleted")
	}
}
