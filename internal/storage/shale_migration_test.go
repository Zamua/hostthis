//go:build slatedb

package storage_test

// Migration round-trip test for the shale backend.
//
// docs/SPEC.md "Migration" claims an in-place cutover: the shale backend
// reads an existing slatedb deployment's keys as-is, no key renamed or
// rewritten, and a raw (un-enveloped) value decodes gracefully. At
// ReplicationFactor=1 there is no last-write-wins envelope at all, so a
// legacy value written by a plain SlateDB.Put must round-trip verbatim
// through ShaleRepo's typed reads.
//
// This test pins that claim against the REAL slate library, not a mock:
// it writes the legacy paste/version/owner/expiry rows straight to the
// bucket via the non-CAS, non-envelope raw-write path (the same shape a
// pre-shale deployment left behind), then reads them back through the
// public ShaleRepo surface (Get / GetVersion / ListVersions / ExpiredSlugs)
// and asserts every field survives.
//
//	go test -tags slatedb -run TestShaleMigration ./internal/storage
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

func TestShaleMigration_RawValueRoundTrips(t *testing.T) {
	repo := newShaleRepoForMigration(t)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("legacy12")
	owner := "key:legacy"

	// The legacy paste row exactly as a slatedb deployment stored it:
	// a single-version paste with the head fields denormalized onto the
	// paste row, a v1 version row, the slug_owner pointer, and the expiry
	// index marker. NO identity_bytes counter, NO identity_pastes index
	// (those are shale-era derived families an old deployment never wrote;
	// the migration claim is that the AUTHORITATIVE rows decode raw).
	p := domain.Paste{
		Slug:          slug,
		Identity:      domain.Identity(owner),
		Kind:          domain.KindMarkdown,
		ContentSHA:    "sha-legacy-v1",
		Size:          1234,
		Name:          "legacy label",
		PinnedVersion: 0,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}

	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode legacy paste: %v", err)
	}
	v1Val, err := storage.LegacyVersionValueForTest(1, p.Kind, p.ContentSHA, p.Size, p.CreatedAt, false)
	if err != nil {
		t.Fatalf("encode legacy version: %v", err)
	}

	// Write the four authoritative rows straight to the bucket, bypassing
	// the Insert CAS transaction + the envelope path (raw legacy bytes).
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), v1Val)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))
	mustPutRaw(t, repo, storage.LegacyExpiryKeyForTest(p.ExpiresAt, slug), storage.MarkerValueForTest())

	// Read the paste back through the typed Get: every field must survive.
	got, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("Get legacy paste: %v", err)
	}
	if got.Slug != p.Slug || got.Identity != p.Identity || got.Kind != p.Kind ||
		got.ContentSHA != p.ContentSHA || got.Size != p.Size || got.Name != p.Name ||
		got.PinnedVersion != p.PinnedVersion {
		t.Fatalf("legacy paste did not round-trip:\n got  %+v\n want %+v", got, p)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || !got.UpdatedAt.Equal(p.UpdatedAt) || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("legacy paste timestamps drifted: got %v/%v/%v want %v/%v/%v",
			got.CreatedAt, got.UpdatedAt, got.ExpiresAt, p.CreatedAt, p.UpdatedAt, p.ExpiresAt)
	}

	// The legacy version row decodes through GetVersion.
	gotV, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("GetVersion legacy v1: %v", err)
	}
	if gotV.VerNum != 1 || gotV.ContentSHA != p.ContentSHA || gotV.Size != p.Size ||
		gotV.Kind != p.Kind || gotV.Deleted {
		t.Fatalf("legacy version did not round-trip: %+v", gotV)
	}

	// ListVersions sees the legacy version too.
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(vers) != 1 || vers[0].VerNum != 1 {
		t.Fatalf("ListVersions over legacy rows: got %+v, want one v1", vers)
	}

	// The legacy expiry index marker is honored by the sweep's scan.
	expired, err := repo.ExpiredSlugs(p.ExpiresAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("ExpiredSlugs: %v", err)
	}
	if !sliceHasMig(expired, slug.String()) {
		t.Fatalf("legacy expiry index not honored: %v should contain %q", expired, slug)
	}

	// The legacy paste's content sha is in the referenced (GC allow-list)
	// set, so its blob is NOT garbage-collected after migration.
	refs, err := repo.ReferencedBlobSHAs()
	if err != nil {
		t.Fatalf("ReferencedBlobSHAs: %v", err)
	}
	if !sliceHasMig(refs, p.ContentSHA) {
		t.Fatalf("legacy paste sha must be referenced post-migration: %v should contain %q", refs, p.ContentSHA)
	}
}

// --- helpers ---------------------------------------------------------------

// newShaleRepoForMigration opens a fresh single-node shale repo on a
// unique logical db within the shared test bucket, skipping cleanly when
// no live MinIO endpoint is configured.
func newShaleRepoForMigration(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale migration test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	return repo
}

func mustPutRaw(t *testing.T, repo *storage.ShaleRepo, key, value []byte) {
	t.Helper()
	if err := repo.PutRawForTest(key, value); err != nil {
		t.Fatalf("raw put %s: %v", key, err)
	}
}

func sliceHasMig(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
