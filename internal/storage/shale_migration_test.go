package storage_test

// Migration round-trip for the shale backend, against the REAL slate library.
//
// docs/SPEC.md "Migration" claims an in-place cutover: the shale backend reads
// an existing slatedb deployment's keys as-is, no key renamed or rewritten,
// and a raw un-enveloped value decodes. At ReplicationFactor=1 there is no
// last-write-wins envelope at all, so a value written by a plain SlateDB.Put
// must round-trip verbatim through ShaleRepo's typed reads.
//
//	go test -tags slatedb -run TestShaleMigration ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"slices"
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

	// The paste row exactly as a slatedb deployment stores it: a
	// single-version paste with the head fields denormalized onto the paste
	// row, a v1 version row and the slug_owner pointer. No identity_bytes
	// counter and no identity_pastes index, since
	// those are shale-era derived families and the claim under test is that
	// the AUTHORITATIVE rows decode raw.
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
	}

	pasteVal, err := storage.LegacyPasteValueForTest(p)
	if err != nil {
		t.Fatalf("encode legacy paste: %v", err)
	}
	v1Val, err := storage.LegacyVersionValueForTest(1, p.Kind, p.ContentSHA, p.Size, p.CreatedAt, false)
	if err != nil {
		t.Fatalf("encode legacy version: %v", err)
	}

	// Straight to the bucket, bypassing the Insert CAS transaction and the
	// envelope path.
	mustPutRaw(t, repo, storage.LegacyPasteKeyForTest(slug), pasteVal)
	mustPutRaw(t, repo, storage.LegacyVersionKeyForTest(slug, 1), v1Val)
	mustPutRaw(t, repo, storage.LegacySlugOwnerKeyForTest(slug), []byte(owner))

	got, gerr := repo.Get(slug)
	if gerr != nil {
		t.Fatalf("Get legacy paste: %v", err)
	}
	if got.Slug != p.Slug || got.Identity != p.Identity || got.Kind != p.Kind ||
		got.ContentSHA != p.ContentSHA || got.Size != p.Size || got.Name != p.Name ||
		got.PinnedVersion != p.PinnedVersion {
		t.Fatalf("legacy paste did not round-trip:\n got  %+v\n want %+v", got, p)
	}

	gotV, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("GetVersion legacy v1: %v", err)
	}
	if gotV.VerNum != 1 || gotV.ContentSHA != p.ContentSHA || gotV.Size != p.Size ||
		gotV.Kind != p.Kind || gotV.Deleted {
		t.Fatalf("legacy version did not round-trip: %+v", gotV)
	}

	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(vers) != 1 || vers[0].VerNum != 1 {
		t.Fatalf("ListVersions over legacy rows: got %+v, want one v1", vers)
	}

	// The head row's content sha survives the migration: the read path resolves
	// bytes through it, so losing it strands the paste's content.
	head, gerr := repo.Get(slug)
	if gerr != nil {
		t.Fatalf("Get over legacy rows: %v", gerr)
	}
	if head.ContentSHA != p.ContentSHA {
		t.Fatalf("legacy paste sha must survive migration: got %q, want %q", head.ContentSHA, p.ContentSHA)
	}
}

// --- helpers ---------------------------------------------------------------

// newShaleRepoForMigration opens a fresh single-node shale repo on a unique
// logical db in the shared test bucket, skipping when no MinIO endpoint is
// configured.
func newShaleRepoForMigration(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	repo := newShaleRepoForTest(t)
	return repo
}

func mustPutRaw(t *testing.T, repo *storage.ShaleRepo, key, value []byte) {
	t.Helper()
	if err := repo.PutRawForTest(key, value); err != nil {
		t.Fatalf("raw put %s: %v", key, err)
	}
}

func sliceHasMig(ss []string, want string) bool {
	return slices.Contains(ss, want)
}
