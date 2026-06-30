//go:build slatedb

// End-to-end tests for the transactional shale-blob path. The METADATA plane
// is a real single-node shale cluster over the slate backend (needs MinIO via
// MINIO_TEST_ENDPOINT + the slatedb build tag + the dylib); the BLOB plane is
// an in-memory blobmem.Store (NO MinIO, NO network for the bytes). The blobmem
// store's settable clock + ModTime drive the SweepOrphans age-gate
// deterministically.
//
// What these pin (docs/design/shale-blobs-phase3.md section 8 step 2 + 3):
//   - reader-atomic create: a staged-but-not-committed blob is invisible (Read
//     -> blob.ErrNotFound); after Commit it serves.
//   - pending-collapse: a committed paste is READY directly (no pending row).
//   - atomic delete: the metadata delete + the blob unbind are one transaction;
//     after delete a Read -> blob.ErrNotFound.
//   - stage-without-commit -> SweepOrphans reclaims the orphan AFTER the grace,
//     not before.
//   - versions: each version binds its own blob; a non-head version reads back.
//   - sites: bind-all-with-manifest; a redeploy drops the old files' blobs.

package shaleblob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/blob/blobmem"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
)

var blobTestSeq atomic.Int64

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newBlobRepo opens a fresh single-node blob-capable ShaleRepo on a unique
// logical metadata db (real slate-over-MinIO), with an in-memory blobmem store
// for the byte plane. Returns the repo, the seam unit, and the blobmem store
// (so a test can drive the SweepOrphans age-gate). Skips when MinIO is absent.
func newBlobRepo(t *testing.T) (*storage.ShaleRepo, *shaleblob.Unit, *blobmem.Store) {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale-blob tests (start dev MinIO first: make dev-minio-up)")
	}
	bs := blobmem.New()
	seq := blobTestSeq.Add(1)
	cfg := storage.ShaleConfig{
		NodeID:            fmt.Sprintf("blob-node-%d", seq),
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            envOr("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
		AccessKey:         envOr("MINIO_TEST_ACCESS_KEY", "admin"),
		SecretKey:         envOr("MINIO_TEST_SECRET_KEY", "supersecret"),
		UseSSL:            false,
		DbName:            fmt.Sprintf("shale-blob-%d-%d", time.Now().UnixNano(), seq),
		ReplicationFactor: 1,
		BlobStore:         bs,
	}
	repo, err := storage.NewShaleRepo(cfg)
	if err != nil {
		t.Fatalf("NewShaleRepo (blob): %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	unit, err := shaleblob.New(repo)
	if err != nil {
		t.Fatalf("shaleblob.New: %v", err)
	}
	return repo, unit, bs
}

// encode runs the same magic+zstd encode the upload pipeline produces, so the
// staged body decodes back through the seam's Read.
func encode(t *testing.T, raw []byte) []byte {
	t.Helper()
	body, err := storage.EncodeCompressedBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("EncodeCompressedBody: %v", err)
	}
	return body
}

func readAll(t *testing.T, unit *shaleblob.Unit, slug, sha string) ([]byte, error) {
	t.Helper()
	rc, _, err := unit.Read(context.Background(), slug, sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	out, rerr := io.ReadAll(rc)
	return out, rerr
}

func mkPaste(slug, owner, sha string, size int, now time.Time) domain.Paste {
	return domain.Paste{
		Slug:       domain.Slug(slug),
		Identity:   domain.Identity(owner),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: sha,
		Size:       size,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(domain.DefaultRetentionWindow),
	}
}

// TestReaderAtomicCreate: a staged blob is invisible until Commit binds it.
func TestReaderAtomicCreate(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	raw := []byte("<!doctype html><h1>atomic</h1>")
	sha := "sha-atomic-1"
	body := encode(t, raw)

	// Stage WITHOUT committing: the bytes are durable but unreferenced.
	h, err := unit.Stage(ctx, "atomicslug", sha, body)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// A read before the bind must 404 (no metadata row resolves the blob id).
	if _, rerr := readAll(t, unit, "atomicslug", sha); !isNotFound(rerr) {
		t.Fatalf("read before commit = %v, want not-found", rerr)
	}

	// Commit: the metadata row + the bind co-commit.
	p := mkPaste("atomicslug", "owner-a", sha, len(body), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Pending-collapse: the committed paste is READY directly (no pending row).
	got, gerr := repo.Get(domain.Slug("atomicslug"))
	if gerr != nil {
		t.Fatalf("Get after commit: %v", gerr)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status after commit = %q, want ready (pending-collapse)", got.Status)
	}

	// Now the read serves the original bytes.
	out, rerr := readAll(t, unit, "atomicslug", sha)
	if rerr != nil {
		t.Fatalf("read after commit: %v", rerr)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("read after commit = %q, want %q", out, raw)
	}
}

// TestAtomicDelete: delete removes the metadata AND unbinds the blob in one
// transaction; afterward the blob is unreachable.
func TestAtomicDelete(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	raw := []byte("<h1>delete me</h1>")
	sha := "sha-del-1"
	body := encode(t, raw)

	h, err := unit.Stage(ctx, "delslug", sha, body)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	p := mkPaste("delslug", "owner-d", sha, len(body), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, rerr := readAll(t, unit, "delslug", sha); rerr != nil {
		t.Fatalf("read after commit: %v", rerr)
	}

	// Delete: metadata + unbind co-commit.
	if err := repo.Delete(domain.Slug("delslug")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The metadata row is gone.
	if _, gerr := repo.Get(domain.Slug("delslug")); !isStorageNotFound(gerr) {
		t.Fatalf("Get after delete = %v, want not-found", gerr)
	}
	// The blob is unbound -> Read 404s (the pointer is gone, even though the
	// bytes may still sit in blobmem until SweepOrphans reclaims them).
	if _, rerr := readAll(t, unit, "delslug", sha); !isNotFound(rerr) {
		t.Fatalf("read after delete = %v, want not-found", rerr)
	}
}

// TestStageWithoutCommit_SweepReclaims: a staged-but-never-bound blob is an
// orphan SweepOrphans reclaims AFTER the grace, not before.
func TestStageWithoutCommit_SweepReclaims(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()
	body := encode(t, []byte("orphan bytes"))

	// Stage, never commit: an orphan object now sits in blobmem with a fresh
	// ModTime (the real wall clock).
	if _, err := unit.Stage(ctx, "orphanslug", "sha-orphan", body); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if countObjects(ctx, bs) != 1 {
		t.Fatalf("after stage: object count = %d, want 1", countObjects(ctx, bs))
	}

	// A sweep with a generous grace must NOT reclaim a just-staged object.
	now := time.Now().UTC()
	if err := repo.SweepBlobOrphans(ctx, now, time.Hour); err != nil {
		t.Fatalf("SweepBlobOrphans (fresh): %v", err)
	}
	if countObjects(ctx, bs) != 1 {
		t.Fatalf("after fresh sweep: object count = %d, want 1 (not reclaimed before grace)", countObjects(ctx, bs))
	}

	// Age the object past the grace, then sweep: now it is reclaimed.
	for k := range listKeys(ctx, bs) {
		bs.SetModTime(k, now.Add(-2*time.Hour))
	}
	if err := repo.SweepBlobOrphans(ctx, now, time.Hour); err != nil {
		t.Fatalf("SweepBlobOrphans (aged): %v", err)
	}
	if countObjects(ctx, bs) != 0 {
		t.Fatalf("after aged sweep: object count = %d, want 0 (reclaimed past grace)", countObjects(ctx, bs))
	}
}

// TestVersions_BindAndRead: an appended version binds its own blob; both the
// head (latest) and the older version read back their own bytes.
func TestVersions_BindAndRead(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	rawV1 := []byte("<h1>v1</h1>")
	shaV1 := "sha-v1"
	bodyV1 := encode(t, rawV1)
	h1, err := unit.Stage(ctx, "verslug", shaV1, bodyV1)
	if err != nil {
		t.Fatalf("Stage v1: %v", err)
	}
	p := mkPaste("verslug", "owner-v", shaV1, len(bodyV1), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h1}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}

	// Append v2 (a different blob). The head moves to v2 (unpinned).
	rawV2 := []byte("<h1>v2 newer</h1>")
	shaV2 := "sha-v2"
	bodyV2 := encode(t, rawV2)
	h2, err := unit.Stage(ctx, "verslug", shaV2, bodyV2)
	if err != nil {
		t.Fatalf("Stage v2: %v", err)
	}
	if err := unit.Commit(ctx, []service.BlobHandle{h2}, func(ctx context.Context) error {
		_, aerr := repo.AppendVersionWithQuotaCheck(ctx, domain.Slug("verslug"), domain.KindHTML, shaV2, len(bodyV2), int64(domain.UserQuotaBytes), now)
		return aerr
	}); err != nil {
		t.Fatalf("Commit v2: %v", err)
	}

	// The head serves v2.
	outHead, herr := readAll(t, unit, "verslug", shaV2)
	if herr != nil {
		t.Fatalf("read head (v2): %v", herr)
	}
	if !bytes.Equal(outHead, rawV2) {
		t.Fatalf("head read = %q, want %q", outHead, rawV2)
	}
	// The older version (v1) still reads back via its own sha -> its own blob.
	outV1, v1err := readAll(t, unit, "verslug", shaV1)
	if v1err != nil {
		t.Fatalf("read v1: %v", v1err)
	}
	if !bytes.Equal(outV1, rawV1) {
		t.Fatalf("v1 read = %q, want %q", outV1, rawV1)
	}
}

// TestSites_BindAllAndRedeployDrops: a site deploy binds EVERY file in one
// transaction (bind-all-with-manifest), each file reads back, and a redeploy
// that drops a file unbinds the dropped file's blob (Read 404s) while the kept
// file still serves.
func TestSites_BindAllAndRedeployDrops(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Deploy v1 with two files. Stage each (as the deploy sink does, via
	// StageStream which encodes), collect the handles, then Commit the manifest
	// + binds together through InsertSiteWithQuotaCheck.
	rawIndex := []byte("<!doctype html><h1>home</h1>")
	rawAbout := []byte("<!doctype html><h1>about</h1>")
	shaIndex := "sha-site-index"
	shaAbout := "sha-site-about"
	h1 := stageFile(t, unit, "siteslug", shaIndex, rawIndex)
	h2 := stageFile(t, unit, "siteslug", shaAbout, rawAbout)

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: shaIndex, Size: len(rawIndex), ContentType: "text/html"})
	man.Add("about.html", domain.ManifestEntry{SHA: shaAbout, Size: len(rawAbout), ContentType: "text/html"})
	site := domain.Site{
		Slug:      domain.Slug("siteslug"),
		Identity:  domain.Identity("owner-s"),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := unit.Commit(ctx, []service.BlobHandle{h1, h2}, func(ctx context.Context) error {
		return repo.InsertSiteWithQuotaCheck(ctx, site, man.DedupedSize(), int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit site v1: %v", err)
	}

	// Both files read back via their shas.
	if out, err := readAll(t, unit, "siteslug", shaIndex); err != nil || !bytes.Equal(out, rawIndex) {
		t.Fatalf("read index v1 = (%q, %v), want %q", out, err, rawIndex)
	}
	if out, err := readAll(t, unit, "siteslug", shaAbout); err != nil || !bytes.Equal(out, rawAbout) {
		t.Fatalf("read about v1 = (%q, %v), want %q", out, err, rawAbout)
	}

	// Redeploy: keep index.html (re-staged: a NEW blob id), drop about.html,
	// add contact.html. The dropped file's OLD blob must be unbound.
	rawIndex2 := []byte("<!doctype html><h1>home v2</h1>")
	rawContact := []byte("<!doctype html><h1>contact</h1>")
	shaIndex2 := "sha-site-index2"
	shaContact := "sha-site-contact"
	now2 := now.Add(time.Minute)
	n1 := stageFile(t, unit, "siteslug", shaIndex2, rawIndex2)
	n2 := stageFile(t, unit, "siteslug", shaContact, rawContact)

	man2 := domain.NewManifest()
	man2.Add("index.html", domain.ManifestEntry{SHA: shaIndex2, Size: len(rawIndex2), ContentType: "text/html"})
	man2.Add("contact.html", domain.ManifestEntry{SHA: shaContact, Size: len(rawContact), ContentType: "text/html"})
	site2 := site
	site2.Manifest = man2
	site2.UpdatedAt = now2
	site2.ExpiresAt = now2.Add(domain.DefaultRetentionWindow)
	if err := unit.Commit(ctx, []service.BlobHandle{n1, n2}, func(ctx context.Context) error {
		return repo.ReplaceSiteWithQuotaCheck(ctx, site2, man2.DedupedSize(), int64(domain.UserQuotaBytes), now2)
	}); err != nil {
		t.Fatalf("Commit site v2 (redeploy): %v", err)
	}

	// The new files serve.
	if out, err := readAll(t, unit, "siteslug", shaIndex2); err != nil || !bytes.Equal(out, rawIndex2) {
		t.Fatalf("read index v2 = (%q, %v), want %q", out, err, rawIndex2)
	}
	if out, err := readAll(t, unit, "siteslug", shaContact); err != nil || !bytes.Equal(out, rawContact) {
		t.Fatalf("read contact v2 = (%q, %v), want %q", out, err, rawContact)
	}
	// The dropped file's blob is unbound -> Read 404s.
	if _, err := readAll(t, unit, "siteslug", shaAbout); !isNotFound(err) {
		t.Fatalf("read dropped about after redeploy = %v, want not-found", err)
	}
	// The OLD index blob (re-staged under a new id) is also unbound -> the old
	// sha no longer resolves (the manifest now maps index.html to shaIndex2).
	if _, err := readAll(t, unit, "siteslug", shaIndex); !isNotFound(err) {
		t.Fatalf("read old index sha after redeploy = %v, want not-found", err)
	}
}

// stageFile stages a single site file through StageStream (which encodes the
// uncompressed bytes), mirroring the deploy sink.
func stageFile(t *testing.T, unit *shaleblob.Unit, slug, sha string, raw []byte) service.BlobHandle {
	t.Helper()
	h, err := unit.StageStream(context.Background(), slug, sha, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("StageStream %s: %v", sha, err)
	}
	return h
}

func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, blob.ErrNotFound) || isStorageNotFound(err))
}

func isStorageNotFound(err error) bool {
	return err != nil && errors.Is(err, storage.ErrNotFound)
}

func countObjects(ctx context.Context, bs *blobmem.Store) int {
	n := 0
	for _, err := range bs.List(ctx, "blob/") {
		if err != nil {
			continue
		}
		n++
	}
	return n
}

func listKeys(ctx context.Context, bs *blobmem.Store) map[string]struct{} {
	out := make(map[string]struct{})
	for info, err := range bs.List(ctx, "blob/") {
		if err != nil {
			continue
		}
		out[info.Key] = struct{}{}
	}
	return out
}
