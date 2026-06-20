//go:build slatedb

// ============================================================================
// TEMPORARY phase-4 migration test - DELETE THIS FILE alongside
// internal/storage/shale_blob_migrate_TEMP.go after the prod blob migration
// completes. It pins RebindLegacyBlob's contract: a legacy row (no blob_id) +
// a staged ref -> the row carries the blob_id, the pointer binds, the read
// path serves the bytes, and a re-run is a no-op.
// ============================================================================

package storage_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/blob/blobmem"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

var rebindTestSeq atomic.Int64

// newRebindRepo opens a fresh single-node blob-capable ShaleRepo on a unique
// logical metadata db (real slate-over-MinIO for the METADATA plane), with an
// in-memory blobmem store for the BYTE plane. Skips when MinIO is absent.
func newRebindRepo(t *testing.T) (*storage.ShaleRepo, *blobmem.Store) {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale-blob rebind test (start dev MinIO first: make dev-minio-up)")
	}
	bs := blobmem.New()
	seq := rebindTestSeq.Add(1)
	cfg := storage.ShaleConfig{
		NodeID:            fmt.Sprintf("rebind-node-%d", seq),
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
		AccessKey:         envOrDefault("MINIO_TEST_ACCESS_KEY", "admin"),
		SecretKey:         envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret"),
		UseSSL:            false,
		DbName:            fmt.Sprintf("shale-rebind-%d-%d", time.Now().UnixNano(), seq),
		ReplicationFactor: 1,
		BlobStore:         bs,
	}
	repo, err := storage.NewShaleRepo(cfg)
	if err != nil {
		t.Fatalf("NewShaleRepo (rebind): %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo, bs
}

// TestRebindLegacyBlob_PasteHead: a legacy paste row (no blob_id) plus a staged
// ref re-keys via RebindLegacyBlob so ResolveBlobID returns the id and
// GetBlobStream serves the bytes; re-running is a no-op.
func TestRebindLegacyBlob_PasteHead(t *testing.T) {
	repo, _ := newRebindRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	raw := []byte("<!doctype html><h1>legacy paste</h1>")
	sha := "sha-rebind-paste"
	body, err := storage.EncodeCompressedBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("EncodeCompressedBody: %v", err)
	}
	slug := domain.Slug("rebindpaste")

	// 1. Insert a LEGACY paste: no blob staged, so the row carries an EMPTY
	//    blob_id (exactly the pre-phase-4 shape the migrator finds in prod).
	p := domain.Paste{
		Slug:       slug,
		Identity:   domain.Identity("owner-legacy"),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: sha,
		Size:       len(body),
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now); err != nil {
		t.Fatalf("InsertWithQuotaCheck (legacy): %v", err)
	}

	// Sanity: the legacy row has no resolvable blob id yet.
	if _, rerr := repo.ResolveBlobID(slug, sha); rerr == nil {
		t.Fatalf("ResolveBlobID before rebind returned a blob id, want not-found (legacy row has empty blob_id)")
	}

	// 2. Stage the old bytes into the collocated bucket (mints the blob id +
	//    the routed ref), then rebind.
	routeKey := repo.RouteKeyForSlug(slug.String())
	ref, serr := repo.StageBlobStream(ctx, routeKey, bytes.NewReader(body), int64(len(body)), sha)
	if serr != nil {
		t.Fatalf("StageBlobStream: %v", serr)
	}
	if ref.BlobID == "" {
		t.Fatalf("StageBlobStream returned an empty blob id")
	}
	if err := repo.RebindLegacyBlob(ctx, slug, storage.LegacyBlobPaste, sha, ref); err != nil {
		t.Fatalf("RebindLegacyBlob: %v", err)
	}

	// 3. ResolveBlobID now returns the staged id, and GetBlobStream serves the
	//    original (decoded) bytes.
	gotID, rerr := repo.ResolveBlobID(slug, sha)
	if rerr != nil {
		t.Fatalf("ResolveBlobID after rebind: %v", rerr)
	}
	if gotID != ref.BlobID {
		t.Fatalf("ResolveBlobID = %q, want %q", gotID, ref.BlobID)
	}
	assertServesBytes(t, repo, routeKey, gotID, raw)

	// 4. Re-run is a no-op: the same ref rebinds without error and the read
	//    still serves the same bytes (idempotency).
	if err := repo.RebindLegacyBlob(ctx, slug, storage.LegacyBlobPaste, sha, ref); err != nil {
		t.Fatalf("RebindLegacyBlob (re-run): %v", err)
	}
	gotID2, rerr2 := repo.ResolveBlobID(slug, sha)
	if rerr2 != nil || gotID2 != ref.BlobID {
		t.Fatalf("ResolveBlobID after re-run = (%q, %v), want (%q, nil)", gotID2, rerr2, ref.BlobID)
	}
	assertServesBytes(t, repo, routeKey, gotID2, raw)
}

// TestRebindLegacyBlob_SiteFile: a legacy site row (empty FileBlobs) plus a
// staged file ref re-keys so the file sha resolves to its blob id and serves.
func TestRebindLegacyBlob_SiteFile(t *testing.T) {
	repo, _ := newRebindRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	raw := []byte("<!doctype html><h1>legacy site file</h1>")
	sha := "sha-rebind-sitefile"
	body, err := storage.EncodeCompressedBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("EncodeCompressedBody: %v", err)
	}
	slug := domain.Slug("rebindsite")

	// Insert a LEGACY site: a manifest referencing the file by sha, but with an
	// EMPTY FileBlobs side-table (the pre-phase-4 shape).
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: sha, Size: len(raw), ContentType: "text/html"})
	site := domain.Site{
		Slug:      slug,
		Identity:  domain.Identity("owner-legacy-site"),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := repo.InsertSiteWithQuotaCheck(ctx, site, man.DedupedSize(), int64(domain.UserQuotaBytes), now); err != nil {
		t.Fatalf("InsertSiteWithQuotaCheck (legacy): %v", err)
	}
	if _, rerr := repo.ResolveBlobID(slug, sha); rerr == nil {
		t.Fatalf("ResolveBlobID before rebind returned a blob id, want not-found (legacy site has empty FileBlobs)")
	}

	routeKey := repo.RouteKeyForSlug(slug.String())
	ref, serr := repo.StageBlobStream(ctx, routeKey, bytes.NewReader(body), int64(len(body)), sha)
	if serr != nil {
		t.Fatalf("StageBlobStream (site): %v", serr)
	}
	if err := repo.RebindLegacyBlob(ctx, slug, storage.LegacyBlobSiteFile, sha, ref); err != nil {
		t.Fatalf("RebindLegacyBlob (site): %v", err)
	}

	gotID, rerr := repo.ResolveBlobID(slug, sha)
	if rerr != nil {
		t.Fatalf("ResolveBlobID after site rebind: %v", rerr)
	}
	if gotID != ref.BlobID {
		t.Fatalf("ResolveBlobID (site) = %q, want %q", gotID, ref.BlobID)
	}
	assertServesBytes(t, repo, routeKey, gotID, raw)

	// Re-run is a no-op.
	if err := repo.RebindLegacyBlob(ctx, slug, storage.LegacyBlobSiteFile, sha, ref); err != nil {
		t.Fatalf("RebindLegacyBlob site (re-run): %v", err)
	}
	if gotID2, rerr2 := repo.ResolveBlobID(slug, sha); rerr2 != nil || gotID2 != ref.BlobID {
		t.Fatalf("ResolveBlobID after site re-run = (%q, %v), want (%q, nil)", gotID2, rerr2, ref.BlobID)
	}
}

// assertServesBytes reads blobid through the cluster, decodes the magic+zstd
// body, and asserts it equals want (the original uncompressed bytes).
func assertServesBytes(t *testing.T, repo *storage.ShaleRepo, routeKey []byte, blobid string, want []byte) {
	t.Helper()
	rc, _, err := repo.GetBlobStream(context.Background(), routeKey, blobid)
	if err != nil {
		t.Fatalf("GetBlobStream(%s): %v", blobid, err)
	}
	dec, derr := storage.DecodeCompressedStream(rc, blobid)
	if derr != nil {
		_ = rc.Close()
		t.Fatalf("DecodeCompressedStream: %v", derr)
	}
	defer dec.Close() //nolint:errcheck
	got, rerr := io.ReadAll(dec)
	if rerr != nil {
		t.Fatalf("read decoded body: %v", rerr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("served bytes = %q, want %q", got, want)
	}
}
