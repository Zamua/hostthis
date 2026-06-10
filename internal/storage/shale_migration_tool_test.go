//go:build slatedb

package storage_test

// End-to-end proof that the hostthis-shale-migrate transform makes a
// slatedb-shaped bucket correct for ShaleRepo, with the per-owner quota
// un-exceedable afterward.
//
// The flow mirrors a real cutover and exercises the SAME transform the
// hostthis-shale-migrate binary runs (internal/migrate.Run), NOT a copy:
//
//  1. Seed a fresh logical db with the pre-migration slatedb shape via a
//     raw slate.New backend (NOT ShaleRepo's writers): authoritative
//     pastes/*, versions/* (incl. a tombstoned version that must NOT be
//     counted), slug_owner/*, expiry/* + the EMPTY-MARKER identity_pastes
//     index a slatedb deployment wrote, and crucially NO identity_bytes
//     counter.
//  2. Run internal/migrate.Run against that backend (the binary's transform).
//  3. Close the raw backend, open ShaleRepo on the SAME DbName, and assert
//     through the PUBLIC API that the bucket is now quota-safe + listable.
//
// SlateDB is single-writer-per-db, so the raw backend and ShaleRepo are
// never open at once: seed/transform first, close, then read.
//
//	go test -tags slatedb -run TestShaleMigrationTool ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/migrate"
	"github.com/Zamua/hostthis/internal/storage"
)

// seededIdentity is the single owner all the seeded pastes belong to.
const seededIdentity = "key:migrated-owner"

// seedPaste describes one pre-migration paste to write: its slug, head
// size, and the live + tombstoned version sizes. liveBytes is the sum of
// the live version sizes (what the migrated counter must equal); the
// tombstoned size is written but must NOT be counted.
type seedPaste struct {
	slug          domain.Slug
	name          string
	headSize      int   // paste-head Size (latest live version size)
	liveVerSizes  []int // non-deleted version sizes, in version order
	deletedVerLen int   // size of one trailing tombstoned version, 0 = none
	createdAt     time.Time
	expiresAt     time.Time
}

func (s seedPaste) liveBytes() int64 {
	var t int64
	for _, n := range s.liveVerSizes {
		t += int64(n)
	}
	return t
}

// seedFixture returns the three pastes the test seeds + the true live-byte
// total. Paste A has two live versions (an append), paste B one, paste C
// one live version plus a tombstoned one that must be excluded.
func seedFixture(now time.Time) (pastes []seedPaste, trueLiveTotal int64) {
	exp := now.Add(domain.RetentionWindow)
	pastes = []seedPaste{
		{slug: "alpha111", name: "alpha", headSize: 50, liveVerSizes: []int{100, 50}, createdAt: now, expiresAt: exp},
		{slug: "bravo222", name: "bravo", headSize: 200, liveVerSizes: []int{200}, createdAt: now.Add(time.Minute), expiresAt: exp},
		{slug: "charlie3", name: "charlie", headSize: 300, liveVerSizes: []int{300}, deletedVerLen: 999, createdAt: now.Add(2 * time.Minute), expiresAt: exp},
	}
	for _, p := range pastes {
		trueLiveTotal += p.liveBytes()
	}
	return pastes, trueLiveTotal
}

// seedPreMigrationBucket writes the pre-migration slatedb shape straight to
// the slate backend (bypassing ShaleRepo's writers): authoritative rows +
// the EMPTY-MARKER identity_pastes index, with NO identity_bytes counter.
func seedPreMigrationBucket(t *testing.T, be backend.Backend, pastes []seedPaste) {
	t.Helper()
	for _, sp := range pastes {
		// Paste head row.
		head := pasteHeadJSON(t, sp)
		mustBackendPut(t, be, []byte("pastes/"+sp.slug.String()), head)

		// Version rows: live versions numbered 1..N, then an optional
		// trailing tombstoned version.
		verNum := 0
		for _, size := range sp.liveVerSizes {
			verNum++
			mustBackendPut(t, be, versionKey(sp.slug, verNum),
				versionJSON(t, verNum, "sha-"+sp.slug.String(), size, sp.createdAt, false))
		}
		if sp.deletedVerLen > 0 {
			verNum++
			mustBackendPut(t, be, versionKey(sp.slug, verNum),
				versionJSON(t, verNum, "sha-deleted-"+sp.slug.String(), sp.deletedVerLen, sp.createdAt, true))
		}

		// slug_owner pointer + expiry index marker.
		mustBackendPut(t, be, []byte("slug_owner/"+sp.slug.String()), []byte(seededIdentity))
		mustBackendPut(t, be, storage.LegacyExpiryKeyForTest(sp.expiresAt, sp.slug), storage.MarkerValueForTest())

		// The EMPTY-MARKER identity_pastes index a slatedb deployment wrote.
		// shale rejects empty Put values, so the slatedb-era marker is a
		// one-byte placeholder, NOT a value-bearing projection. The
		// transform must REWRITE it.
		mustBackendPut(t, be, storage.IdentityPasteKeyForTest(seededIdentity, sp.slug.String()),
			storage.MarkerValueForTest())
	}
	// NOTE: NO identity_bytes/<id> counter is written. That is precisely
	// the row the transform must create.
}

func pasteHeadJSON(t *testing.T, sp seedPaste) []byte {
	t.Helper()
	v, err := storage.LegacyPasteValueForTest(domain.Paste{
		Slug:       sp.slug,
		Identity:   domain.Identity(seededIdentity),
		Kind:       domain.KindMarkdown,
		ContentSHA: "sha-" + sp.slug.String(),
		Size:       sp.headSize,
		Name:       sp.name,
		CreatedAt:  sp.createdAt,
		UpdatedAt:  sp.createdAt,
		ExpiresAt:  sp.expiresAt,
	})
	if err != nil {
		t.Fatalf("encode paste head %s: %v", sp.slug, err)
	}
	return v
}

func versionKey(slug domain.Slug, verNum int) []byte {
	return storage.LegacyVersionKeyForTest(slug, verNum)
}

func versionJSON(t *testing.T, verNum int, sha string, size int, createdAt time.Time, deleted bool) []byte {
	t.Helper()
	v, err := storage.LegacyVersionValueForTest(verNum, domain.KindMarkdown, sha, size, createdAt, deleted)
	if err != nil {
		t.Fatalf("encode version %d: %v", verNum, err)
	}
	return v
}

func mustBackendPut(t *testing.T, be backend.Backend, key, value []byte) {
	t.Helper()
	if err := be.Put(key, value); err != nil {
		t.Fatalf("backend.Put %s: %v", key, err)
	}
}

// openSlateBackend opens a raw slate backend on cfg's DbName. The caller
// must Close it before opening a ShaleRepo on the same DbName.
func openSlateBackend(t *testing.T, cfg storage.ShaleConfig) backend.Backend {
	t.Helper()
	be, err := slate.New(slate.Config{
		Bucket:    cfg.Bucket,
		DbName:    cfg.DbName,
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseSSL:    cfg.UseSSL,
	})
	if err != nil {
		t.Fatalf("open slate backend: %v", err)
	}
	return be
}

func openShaleRepo(t *testing.T, cfg storage.ShaleConfig) *storage.ShaleRepo {
	t.Helper()
	repo, err := storage.NewShaleRepo(cfg)
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	return repo
}

// quietLogger discards the transform's progress output during tests.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func requireMinio(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; start dev MinIO first")
	}
	return endpoint
}

// TestShaleMigrationTool_QuotaSafeAndListable is the headline test: the
// transform makes a slatedb-shaped bucket quota-safe + listable under
// ShaleRepo, idempotently.
func TestShaleMigrationTool_QuotaSafeAndListable(t *testing.T) {
	endpoint := requireMinio(t)
	cfg := uniqueShaleConfig(endpoint)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	pastes, trueLiveTotal := seedFixture(now)
	if trueLiveTotal != 650 {
		t.Fatalf("fixture sanity: trueLiveTotal=%d want 650", trueLiveTotal)
	}

	// --- Phase 1: seed the pre-migration bucket + RUN THE TRANSFORM. -------
	// All on one raw slate backend handle, which we then close so ShaleRepo
	// can open the same DbName (SlateDB is single-writer-per-db).
	be := openSlateBackend(t, cfg)
	seedPreMigrationBucket(t, be, pastes)

	stats, err := migrate.Run(be, false, quietLogger())
	if err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	if stats.Identities != 1 || stats.Pastes != len(pastes) {
		t.Fatalf("transform stats: %+v; want 1 identity, %d pastes", stats, len(pastes))
	}
	if stats.CountersWritten != 1 || stats.ProjectionsWritten != len(pastes) {
		t.Fatalf("transform wrote counters=%d projections=%d; want 1 / %d",
			stats.CountersWritten, stats.ProjectionsWritten, len(pastes))
	}

	// --- Phase 2 (idempotency): RUN THE TRANSFORM A SECOND TIME. ----------
	// The counter + projections must be recomputed + overwritten to the
	// same bytes, never double-counted.
	stats2, err := migrate.Run(be, false, quietLogger())
	if err != nil {
		t.Fatalf("migrate.Run (2nd): %v", err)
	}
	if stats2 != stats {
		t.Fatalf("2nd transform stats drifted: %+v vs %+v", stats2, stats)
	}
	// Read the counter byte-for-byte off the raw backend after both runs:
	// it must equal the true live total, not double it.
	rawCounter, err := be.Get(storage.IdentityBytesKeyForTest(seededIdentity))
	if err != nil {
		t.Fatalf("read counter after 2nd run: %v", err)
	}
	if string(rawCounter) != "650" {
		t.Fatalf("idempotency: counter=%q after two runs; want \"650\" (a double-count would be \"1300\")", rawCounter)
	}
	if err := be.Close(); err != nil {
		t.Fatalf("close raw backend: %v", err)
	}

	// --- Phase 3: assert through ShaleRepo's PUBLIC API. ------------------
	repo := openShaleRepo(t, cfg)
	defer repo.Close() //nolint:errcheck

	// QUOTA (headline). The counter equals the true non-deleted-version sum
	// (NOT 0, NOT counting the tombstone).
	sum, err := repo.SumActiveBytesByOwner(seededIdentity, now)
	if err != nil {
		t.Fatalf("SumActiveBytesByOwner: %v", err)
	}
	if int64(sum) != trueLiveTotal {
		t.Fatalf("SumActiveBytesByOwner=%d; want %d (counter must exclude the tombstoned 999-byte version)", sum, trueLiveTotal)
	}

	// A migrated user CANNOT exceed quota: with the counter already at 650,
	// an insert that would push (existing + new) over a 700-byte cap is
	// REJECTED, and the counter does NOT move.
	cap := int64(700)
	overflow := domain.Paste{
		Slug: "overflow", Identity: domain.Identity(seededIdentity), Kind: domain.KindMarkdown,
		ContentSHA: "sha-overflow", Size: 100, Name: "overflow",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(overflow, 0, cap, now); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("over-cap insert: got err=%v; want ErrOverUserQuota (650+100 > 700)", err)
	}
	if sumAfter, _ := repo.SumActiveBytesByOwner(seededIdentity, now); int64(sumAfter) != trueLiveTotal {
		t.Fatalf("counter moved after a rejected insert: %d want %d", sumAfter, trueLiveTotal)
	}

	// An insert that FITS under the cap succeeds + advances the counter.
	fits := domain.Paste{
		Slug: "fitsokok", Identity: domain.Identity(seededIdentity), Kind: domain.KindMarkdown,
		ContentSHA: "sha-fits", Size: 40, Name: "fits",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(fits, 0, cap, now); err != nil {
		t.Fatalf("fitting insert (650+40 <= 700): %v", err)
	}
	if sumAfter, _ := repo.SumActiveBytesByOwner(seededIdentity, now); int64(sumAfter) != trueLiveTotal+40 {
		t.Fatalf("counter after fitting insert: %d want %d", sumAfter, trueLiveTotal+40)
	}
	// And now that the counter sits at 690, a further 40-byte insert is
	// rejected (690+40 > 700): the cap is a hard ceiling post-migration.
	tooMuch := domain.Paste{
		Slug: "toomuch1", Identity: domain.Identity(seededIdentity), Kind: domain.KindMarkdown,
		ContentSHA: "sha-toomuch", Size: 40, Name: "toomuch",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(tooMuch, 0, cap, now); !errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("second over-cap insert: got err=%v; want ErrOverUserQuota (690+40 > 700)", err)
	}

	// LISTBYOWNER returns the correct rows for the migrated pastes (the
	// projections were filled by the transform). It also includes the one
	// paste we just inserted, so expect len(pastes)+1.
	list, err := repo.ListByOwner(seededIdentity)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(list) != len(pastes)+1 {
		t.Fatalf("ListByOwner returned %d pastes; want %d (%d migrated + 1 inserted)", len(list), len(pastes)+1, len(pastes))
	}
	byName := map[string]domain.Paste{}
	for _, p := range list {
		byName[p.Name] = p
	}
	for _, sp := range pastes {
		got, ok := byName[sp.name]
		if !ok {
			t.Fatalf("ListByOwner missing migrated paste %q", sp.name)
		}
		if got.Slug != sp.slug {
			t.Fatalf("ListByOwner %q slug=%q want %q", sp.name, got.Slug, sp.slug)
		}
		if got.Size != sp.headSize {
			t.Fatalf("ListByOwner %q size=%d want %d (projection head size)", sp.name, got.Size, sp.headSize)
		}
		if !got.CreatedAt.Equal(sp.createdAt) {
			t.Fatalf("ListByOwner %q created=%v want %v", sp.name, got.CreatedAt, sp.createdAt)
		}
	}
}

// TestShaleMigrationTool_WithoutToolFails demonstrates the assertion bites:
// the SAME quota assertions run against a raw seeded bucket that was NOT
// transformed FAIL. The counter reads 0 (the row is absent) and the
// over-cap insert wrongly succeeds. This is the negative control proving
// the headline test isn't vacuously green.
func TestShaleMigrationTool_WithoutToolFails(t *testing.T) {
	endpoint := requireMinio(t)
	cfg := uniqueShaleConfig(endpoint)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	pastes, trueLiveTotal := seedFixture(now)

	// Seed the pre-migration bucket but DO NOT run the transform.
	be := openSlateBackend(t, cfg)
	seedPreMigrationBucket(t, be, pastes)
	if err := be.Close(); err != nil {
		t.Fatalf("close raw backend: %v", err)
	}

	repo := openShaleRepo(t, cfg)
	defer repo.Close() //nolint:errcheck

	// Without the transform, the counter is absent -> reads 0, NOT the true
	// live total. This is the bug the transform exists to fix.
	sum, err := repo.SumActiveBytesByOwner(seededIdentity, now)
	if err != nil {
		t.Fatalf("SumActiveBytesByOwner: %v", err)
	}
	if int64(sum) != 0 {
		t.Fatalf("pre-transform expectation: counter should read 0 (absent), got %d", sum)
	}
	if int64(sum) == trueLiveTotal {
		t.Fatalf("pre-transform counter unexpectedly already correct (%d); the negative control is meaningless", sum)
	}

	// Because the counter is 0, an insert that SHOULD exceed the 700-byte
	// cap (true usage 650 + new 100 = 750) is WRONGLY ACCEPTED: the quota
	// check only sees 0 + 100 <= 700. A migrated-but-untransformed user can
	// blow past quota. THIS is the data-correctness hole the transform
	// closes.
	cap := int64(700)
	overflow := domain.Paste{
		Slug: "overflow", Identity: domain.Identity(seededIdentity), Kind: domain.KindMarkdown,
		ContentSHA: "sha-overflow", Size: 100, Name: "overflow",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	err = repo.InsertWithQuotaCheck(overflow, 0, cap, now)
	if err != nil {
		t.Fatalf("pre-transform expectation: over-cap insert should WRONGLY succeed (counter=0), but got err=%v", err)
	}
	// The headline test's assertion (errors.Is(err, ErrOverUserQuota)) would
	// FAIL here: prove that explicitly so the negative control is airtight.
	if errors.Is(err, storage.ErrOverUserQuota) {
		t.Fatalf("contradiction: expected the un-transformed bucket to let the over-cap insert through")
	}
	t.Logf("negative control confirmed: without the transform the counter reads %d and the 750-vs-700 over-cap insert was accepted (would exceed quota)", sum)
}

// TestShaleMigrationTool_TombstoneNotCounted is a focused assertion that
// the tombstoned version's bytes are excluded from the migrated counter,
// independent of the headline flow. It seeds ONE paste with one live and
// one tombstoned version and checks the counter equals only the live size.
func TestShaleMigrationTool_TombstoneNotCounted(t *testing.T) {
	endpoint := requireMinio(t)
	cfg := uniqueShaleConfig(endpoint)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	one := []seedPaste{{
		slug: "tomb1234", name: "tomb", headSize: 111,
		liveVerSizes: []int{111}, deletedVerLen: 888,
		createdAt: now, expiresAt: now.Add(domain.RetentionWindow),
	}}

	be := openSlateBackend(t, cfg)
	seedPreMigrationBucket(t, be, one)
	if _, err := migrate.Run(be, false, quietLogger()); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	raw, err := be.Get(storage.IdentityBytesKeyForTest(seededIdentity))
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if err := be.Close(); err != nil {
		t.Fatalf("close raw backend: %v", err)
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("counter %q not a bare integer: %v", raw, err)
	}
	if got != 111 {
		t.Fatalf("counter=%d; want 111 (the 888-byte tombstone must NOT be counted)", got)
	}

	repo := openShaleRepo(t, cfg)
	defer repo.Close() //nolint:errcheck
	sum, err := repo.SumActiveBytesByOwner(seededIdentity, now)
	if err != nil {
		t.Fatalf("SumActiveBytesByOwner: %v", err)
	}
	if sum != 111 {
		t.Fatalf("SumActiveBytesByOwner=%d; want 111", sum)
	}
}
