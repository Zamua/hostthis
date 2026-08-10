//go:build slatedb

// End-to-end tests for the transactional shale-blob path. The METADATA plane is
// a real single-node shale cluster over the slate backend (needs MINIO_TEST_
// ENDPOINT, the slatedb tag and the dylib); the BLOB plane is an in-memory
// blobmem.Store, so the bytes never touch MinIO or the network. blobmem's
// settable ModTime is what makes the SweepOrphans age-gate deterministic.

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
	"github.com/Zamua/shale/pkg/cluster"

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

// newBlobRepo opens a blob-capable ShaleRepo on a unique logical metadata db,
// with a blobmem store for the byte plane. The store is returned so a test can
// drive the SweepOrphans age-gate. Skips when MinIO is absent.
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

// encode applies the same magic+zstd framing the upload pipeline produces, so a
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
	}
}

// A staged blob is invisible until Commit binds it, and the committed paste is
// READY directly with no pending row.
func TestReaderAtomicCreate(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	raw := []byte("<!doctype html><h1>atomic</h1>")
	sha := "sha-atomic-1"
	body := encode(t, raw)

	// Staged without committing: the bytes are durable but unreferenced.
	h := stageOwned(t, unit, &ctx, "atomicslug", sha, body)
	// No metadata row resolves the blob id yet, so the read must 404.
	if _, rerr := readAll(t, unit, "atomicslug", sha); !isNotFound(rerr) {
		t.Fatalf("read before commit = %v, want not-found", rerr)
	}

	// The metadata row and the bind co-commit.
	p := mkPaste("atomicslug", "owner-a", sha, len(body), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, gerr := repo.Get(domain.Slug("atomicslug"))
	if gerr != nil {
		t.Fatalf("Get after commit: %v", gerr)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status after commit = %q, want ready (pending-collapse)", got.Status)
	}

	out, rerr := readAll(t, unit, "atomicslug", sha)
	if rerr != nil {
		t.Fatalf("read after commit: %v", rerr)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("read after commit = %q, want %q", out, raw)
	}
}

// Delete removes the metadata and unbinds the blob in ONE transaction, leaving
// the blob unreachable.
func TestAtomicDelete(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	raw := []byte("<h1>delete me</h1>")
	sha := "sha-del-1"
	body := encode(t, raw)

	h := stageOwned(t, unit, &ctx, "delslug", sha, body)
	p := mkPaste("delslug", "owner-d", sha, len(body), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, rerr := readAll(t, unit, "delslug", sha); rerr != nil {
		t.Fatalf("read after commit: %v", rerr)
	}

	if err := repo.Delete(domain.Slug("delslug"), p.Identity, p.CreatedAt); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, gerr := repo.Get(domain.Slug("delslug")); !isStorageNotFound(gerr) {
		t.Fatalf("Get after delete = %v, want not-found", gerr)
	}
	// The pointer is gone even though the bytes may sit in blobmem until
	// SweepOrphans reclaims them.
	if _, rerr := readAll(t, unit, "delslug", sha); !isNotFound(rerr) {
		t.Fatalf("read after delete = %v, want not-found", rerr)
	}
}

// Each version binds its own blob, so the head and an older version each read
// back their own bytes.
func TestVersions_BindAndRead(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	rawV1 := []byte("<h1>v1</h1>")
	shaV1 := "sha-v1"
	bodyV1 := encode(t, rawV1)
	h1 := stageOwned(t, unit, &ctx, "verslug", shaV1, bodyV1)
	p := mkPaste("verslug", "owner-v", shaV1, len(bodyV1), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h1}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}

	// v2 is a different blob; unpinned, so the head moves to it.
	rawV2 := []byte("<h1>v2 newer</h1>")
	shaV2 := "sha-v2"
	bodyV2 := encode(t, rawV2)
	h2 := stageOwned(t, unit, &ctx, "verslug", shaV2, bodyV2)
	if err := unit.Commit(ctx, []service.BlobHandle{h2}, func(ctx context.Context) error {
		_, aerr := repo.AppendVersionWithQuotaCheck(ctx, domain.Slug("verslug"), domain.KindHTML, shaV2, len(bodyV2), int64(domain.UserQuotaBytes), now)
		return aerr
	}); err != nil {
		t.Fatalf("Commit v2: %v", err)
	}

	outHead, herr := readAll(t, unit, "verslug", shaV2)
	if herr != nil {
		t.Fatalf("read head (v2): %v", herr)
	}
	if !bytes.Equal(outHead, rawV2) {
		t.Fatalf("head read = %q, want %q", outHead, rawV2)
	}
	// v1 still reads back via its own sha, hence its own blob.
	outV1, v1err := readAll(t, unit, "verslug", shaV1)
	if v1err != nil {
		t.Fatalf("read v1: %v", v1err)
	}
	if !bytes.Equal(outV1, rawV1) {
		t.Fatalf("v1 read = %q, want %q", outV1, rawV1)
	}
}

// A site deploy binds EVERY file in one transaction, and a redeploy unbinds the
// blobs of the files it drops while the kept files still serve.
func TestSites_BindAllAndRedeployDrops(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Stage each file as the deploy sink does, then commit the manifest and all
	// the binds together.
	rawIndex := []byte("<!doctype html><h1>home</h1>")
	rawAbout := []byte("<!doctype html><h1>about</h1>")
	shaIndex := "sha-site-index"
	shaAbout := "sha-site-about"
	h1 := stageFile(t, unit, &ctx, "siteslug", shaIndex, rawIndex)
	h2 := stageFile(t, unit, &ctx, "siteslug", shaAbout, rawAbout)

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: shaIndex, Size: len(rawIndex), ContentType: "text/html"})
	man.Add("about.html", domain.ManifestEntry{SHA: shaAbout, Size: len(rawAbout), ContentType: "text/html"})
	site := domain.Site{
		Slug:      domain.Slug("siteslug"),
		Identity:  domain.Identity("owner-s"),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := unit.Commit(ctx, []service.BlobHandle{h1, h2}, func(ctx context.Context) error {
		return storage.NewSites(repo).InsertWithQuotaCheck(ctx, site, man.Size(), int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit site v1: %v", err)
	}

	if out, err := readAll(t, unit, "siteslug", shaIndex); err != nil || !bytes.Equal(out, rawIndex) {
		t.Fatalf("read index v1 = (%q, %v), want %q", out, err, rawIndex)
	}
	if out, err := readAll(t, unit, "siteslug", shaAbout); err != nil || !bytes.Equal(out, rawAbout) {
		t.Fatalf("read about v1 = (%q, %v), want %q", out, err, rawAbout)
	}

	// Redeploy: index.html is re-staged under a NEW blob id, about.html is
	// dropped, contact.html is added.
	rawIndex2 := []byte("<!doctype html><h1>home v2</h1>")
	rawContact := []byte("<!doctype html><h1>contact</h1>")
	shaIndex2 := "sha-site-index2"
	shaContact := "sha-site-contact"
	now2 := now.Add(time.Minute)
	n1 := stageFile(t, unit, &ctx, "siteslug", shaIndex2, rawIndex2)
	n2 := stageFile(t, unit, &ctx, "siteslug", shaContact, rawContact)

	man2 := domain.NewManifest()
	man2.Add("index.html", domain.ManifestEntry{SHA: shaIndex2, Size: len(rawIndex2), ContentType: "text/html"})
	man2.Add("contact.html", domain.ManifestEntry{SHA: shaContact, Size: len(rawContact), ContentType: "text/html"})
	site2 := site
	site2.Manifest = man2
	site2.UpdatedAt = now2
	if err := unit.Commit(ctx, []service.BlobHandle{n1, n2}, func(ctx context.Context) error {
		return storage.NewSites(repo).ReplaceWithQuotaCheck(ctx, site2, man2.Size(), int64(domain.UserQuotaBytes), now2)
	}); err != nil {
		t.Fatalf("Commit site v2 (redeploy): %v", err)
	}

	if out, err := readAll(t, unit, "siteslug", shaIndex2); err != nil || !bytes.Equal(out, rawIndex2) {
		t.Fatalf("read index v2 = (%q, %v), want %q", out, err, rawIndex2)
	}
	if out, err := readAll(t, unit, "siteslug", shaContact); err != nil || !bytes.Equal(out, rawContact) {
		t.Fatalf("read contact v2 = (%q, %v), want %q", out, err, rawContact)
	}
	// v1's blobs are STILL BOUND, and that is the contract now: a re-deploy
	// appends a version rather than replacing one, so v1 stays live and
	// rollable-back and its bytes have to survive. Unbinding them here would
	// make a rollback serve a manifest whose files are gone.
	//
	// This assertion used to be the reverse, from when a re-deploy destroyed
	// what it replaced.
	if out, err := readAll(t, unit, "siteslug", shaAbout); err != nil || !bytes.Equal(out, rawAbout) {
		t.Fatalf("v1's dropped file must survive the re-deploy: (%q, %v), want %q. "+
			"A retained version whose blobs are unbound cannot be rolled back to",
			out, err, rawAbout)
	}
	if out, err := readAll(t, unit, "siteslug", shaIndex); err != nil || !bytes.Equal(out, rawIndex) {
		t.Fatalf("v1's index blob must survive the re-deploy: (%q, %v), want %q", out, err, rawIndex)
	}
}

// stageFile stages one site file through StageStream, which encodes the
// uncompressed bytes, mirroring the deploy sink.
// stageFile stages one file of a multi-file deploy, claiming ownership the way
// the deploy service does. ctx is updated in place because the epoch has to
// reach the Commit that binds these handles.
func stageFile(t *testing.T, unit *shaleblob.Unit, ctx *context.Context, slug, sha string, raw []byte) service.BlobHandle {
	t.Helper()
	owned, err := unit.BeginUpload(*ctx, slug)
	if err != nil {
		t.Fatalf("BeginUpload %s: %v", slug, err)
	}
	*ctx = owned
	h, err := unit.StageStream(*ctx, slug, sha, bytes.NewReader(raw), int64(len(raw)))
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

// countObjects reports how many blob objects the byte plane holds.
//
// Every object in this store arrives through the upload path, which stages
// under blob/, so the store's total IS the blob count. Nothing in these tests
// writes to it directly.
func countObjects(_ context.Context, bs *blobmem.Store) int {
	return bs.Len()
}

// An abandoned upload's bytes are reclaimed, and a committed one's are not.
//
// This is the whole point of the saga: bytes staged by an upload that died
// before committing are deleted from the object store on recovery, while bytes
// belonging to a write that landed are left alone. Both halves in one test,
// because the dangerous failure is not "fails to reclaim" - it is "reclaims the
// wrong one", and only comparing the two catches that.
func TestReclaimStagedBytes_AbandonedGoesCommittedStays(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()

	// An upload that COMMITS: stage, then bind through a real metadata write.
	live := domain.Slug("livepast")
	liveCtx, err := unit.BeginUpload(ctx, string(live))
	if err != nil {
		t.Fatalf("begin live: %v", err)
	}
	lh, err := unit.Stage(liveCtx, string(live), "sha-live", encode(t, []byte("live bytes")))
	if err != nil {
		t.Fatalf("stage live: %v", err)
	}
	if err := unit.Commit(liveCtx, []service.BlobHandle{lh}, func(c context.Context) error {
		return repo.InsertWithQuotaCheck(c, domain.Paste{
			Slug: live, Identity: "key:live", Kind: domain.KindHTML,
			ContentSHA: "sha-live", Size: 10,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}, 0, time.Now().UTC())
	}); err != nil {
		t.Fatalf("commit live: %v", err)
	}

	// An upload that DIES after staging: no commit, so its records survive.
	dead := domain.Slug("deadpast")
	deadCtx, err := unit.BeginUpload(ctx, string(dead))
	if err != nil {
		t.Fatalf("begin dead: %v", err)
	}
	if _, err := unit.Stage(deadCtx, string(dead), "sha-dead", encode(t, []byte("abandoned bytes"))); err != nil {
		t.Fatalf("stage dead: %v", err)
	}

	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("fixture: %d objects staged, want 2 (one live, one abandoned)", got)
	}

	// Recovery reclaims the abandoned upload. Driven through the production
	// sweep, which sees BOTH slugs in one pass - so "reclaims the wrong one"
	// is reachable here rather than excluded by only ever pointing it at the
	// slug we expect it to delete.
	past := time.Now().UTC().Add(24 * time.Hour)
	if _, err := repo.SweepStagedBytes(ctx, past); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := countObjects(ctx, bs); got != 1 {
		t.Fatalf("after reclaiming the abandoned upload: %d objects, want 1", got)
	}

	// And the case ErrBound actually guards: a committed upload whose records
	// SURVIVED, because clearing them is best-effort and can fail. Re-record
	// the bound ref to reproduce that, since a successful commit clears them
	// and reclaim would otherwise return early without ever unstaging - passing
	// for the wrong reason.
	liveRef, ok := lh.Ref.(cluster.BlobRef)
	if !ok {
		t.Fatal("handle carries no cluster.BlobRef")
	}
	if err := repo.RecordStagedRef(ctx, live, liveRef); err != nil {
		t.Fatalf("re-record the bound ref: %v", err)
	}
	if _, err := repo.SweepStagedBytes(ctx, past); err != nil {
		t.Fatalf("sweep with a surviving bound record: %v", err)
	}
	if got := countObjects(ctx, bs); got != 1 {
		t.Fatalf("reclaiming a COMMITTED upload deleted its bytes: %d objects left, want 1. "+
			"A live paste now 404s and no scan can put it back", got)
	}
	// The committed paste still reads back.
	if out, rerr := readAll(t, unit, string(live), "sha-live"); rerr != nil || string(out) != "live bytes" {
		t.Fatalf("committed paste unreadable after reclaim: (%q, %v)", out, rerr)
	}
}

// stageOwned claims blob ownership and then stages, which is the sequence the
// service layer performs. Staging without the claim binds nothing: the fence
// fails closed, deliberately, so a path that forgets cannot silently skip it.
//
// It updates ctx in place because the epoch rides the context all the way to
// Commit, and a test that staged with one context and committed with another
// would drop it.
func stageOwned(t *testing.T, unit *shaleblob.Unit, ctx *context.Context, slug, sha string, body []byte) service.BlobHandle {
	t.Helper()
	owned, err := unit.BeginUpload(*ctx, slug)
	if err != nil {
		t.Fatalf("BeginUpload %s: %v", slug, err)
	}
	*ctx = owned
	h, err := unit.Stage(*ctx, slug, sha, body)
	if err != nil {
		t.Fatalf("Stage %s: %v", slug, err)
	}
	return h
}

// An upload that dies DURING staging is reclaimed by the sweep.
//
// This is the likeliest abandonment - a long multi-file deploy interrupted
// partway - and it is the case the intent-driven sweep can miss, because the
// intent is opened by the INSERT, which such an upload never reaches. If the
// only record of those bytes is unreachable from recovery, they leak forever
// and the whole feature does not cover its main case.
func TestSweep_ReclaimsAnUploadThatDiedWhileStaging(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()
	slug := domain.Slug("diedstag")

	owned, err := unit.BeginUpload(ctx, string(slug))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	// Two files staged, then the process dies: no insert, so no commit.
	for _, f := range []struct{ sha, body string }{{"sha-d1", "one"}, {"sha-d2", "two"}} {
		if _, err := unit.Stage(owned, string(slug), f.sha, encode(t, []byte(f.body))); err != nil {
			t.Fatalf("stage %s: %v", f.sha, err)
		}
	}
	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("fixture: %d objects staged, want 2", got)
	}

	// A sweep running NOW must leave it alone: from the outside an upload
	// still staging is indistinguishable from an abandoned one, and only the
	// grace separates them. Without this half, a sweep that ignored the grace
	// entirely would still pass the assertion below.
	if _, err := repo.SweepStagedBytes(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("sweep within the grace: %v", err)
	}
	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("a sweep inside the grace reclaimed %d of 2 objects: it cannot tell a "+
			"running upload from a dead one, so it deletes bytes out from under live deploys",
			2-got)
	}

	// Past the grace, it must reach them.
	if _, err := repo.SweepStagedBytes(ctx, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := countObjects(ctx, bs); got != 0 {
		t.Fatalf("after the sweep: %d objects remain, want 0. An upload that died while "+
			"STAGING left bytes the sweep never reached - the likeliest abandonment is "+
			"the one not covered", got)
	}
}

// The sweep counts uploads it RECLAIMED, not uploads it looked at.
//
// A count that includes skipped uploads reports deletions that did not happen.
// That is worse than a wrong number: it is the log line an operator reads to
// decide whether reclamation works at all, and it would say "yes" while the
// mechanism did nothing.
func TestSweep_CountsOnlyWhatItActuallyReclaimed(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()
	slug := domain.Slug("counted1")

	owned, err := unit.BeginUpload(ctx, string(slug))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if _, err := unit.Stage(owned, string(slug), "sha-c1", encode(t, []byte("bytes"))); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// Inside the grace: one candidate, nothing reclaimed.
	n, err := repo.SweepStagedBytes(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep within the grace: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep reported %d reclaimed while deleting nothing (%d objects still there): "+
			"the count includes uploads it merely looked at", n, countObjects(ctx, bs))
	}

	// Past it: the same candidate, now genuinely reclaimed.
	n, err = repo.SweepStagedBytes(ctx, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("sweep past the grace: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep reported %d reclaimed, want 1", n)
	}
	if got := countObjects(ctx, bs); got != 0 {
		t.Fatalf("%d objects remain after a sweep that reported reclaiming them", got)
	}
}

// An upload with live staged records IS reported as a candidate.
//
// The whole sweep hangs off this scan. If it silently returned nothing the
// reclaimed count would be a truthful zero forever and every abandoned upload
// would leak, with no error anywhere to say so.
//
// The converse - that a COMMITTED upload's cleared records are NOT reported -
// cannot be pinned here. Clearing at ReplicationFactor 1 is a real delete, so
// the scan cannot see it either way; the empty-payload envelope that a scan
// hands back only exists at RF>1. That case is verified on staging (RF=2),
// where it is observable.
func TestSweep_ReportsAnUploadWithLiveStagedRecords(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	slug := domain.Slug("candid01")

	owned, err := unit.BeginUpload(ctx, string(slug))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if _, err := unit.Stage(owned, string(slug), "sha-cand", encode(t, []byte("staged"))); err != nil {
		t.Fatalf("stage: %v", err)
	}

	slugs, err := repo.StagedUploadsLocalForTest()
	if err != nil {
		t.Fatalf("candidate scan: %v", err)
	}
	for _, s := range slugs {
		if s == slug {
			return
		}
	}
	t.Fatalf("the candidate scan did not report %s, which has staged records: a scan that "+
		"comes back empty makes the sweep a permanent no-op that reports success (got %v)",
		slug, slugs)
}

// Deleting a paste reclaims its bytes, and leaves every other paste's alone.
//
// Unbinding removes the POINTER; the object outlives it. Before the delete
// recorded what it orphaned, those bytes were unreachable forever - the record
// that located them was cleared at commit, so nothing named them.
//
// Both halves in one test, because the dangerous failure is not "fails to
// reclaim", it is "reclaims the wrong one".
func TestDelete_ReclaimsItsBytesAndSparesTheOthers(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()

	create := func(slug domain.Slug, sha, body string) {
		t.Helper()
		owned, err := unit.BeginUpload(ctx, string(slug))
		if err != nil {
			t.Fatalf("BeginUpload %s: %v", slug, err)
		}
		h, err := unit.Stage(owned, string(slug), sha, encode(t, []byte(body)))
		if err != nil {
			t.Fatalf("stage %s: %v", slug, err)
		}
		if err := unit.Commit(owned, []service.BlobHandle{h}, func(c context.Context) error {
			return repo.InsertWithQuotaCheck(c, domain.Paste{
				Slug: slug, Identity: "key:deltest", Kind: domain.KindHTML,
				ContentSHA: sha, Size: len(body),
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}, 0, time.Now().UTC())
		}); err != nil {
			t.Fatalf("commit %s: %v", slug, err)
		}
	}

	const doomed, keeper = domain.Slug("doomed01"), domain.Slug("keeper01")
	create(doomed, "sha-doomed", "bytes that should go")
	create(keeper, "sha-keeper", "bytes that must stay")
	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("fixture: %d objects, want 2", got)
	}

	doomedP, err := repo.Get(doomed)
	if err != nil {
		t.Fatalf("get doomed: %v", err)
	}
	if err := repo.Delete(doomed, doomedP.Identity, doomedP.CreatedAt); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The delete removed the pointer. The object is still there until a sweep.
	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("fixture: delete removed %d object(s) synchronously; this test is "+
			"about what the SWEEP reclaims", 2-got)
	}

	if _, err := repo.SweepStagedBytes(ctx, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := countObjects(ctx, bs); got != 1 {
		t.Fatalf("after the sweep: %d objects, want 1. A deleted paste's bytes are "+
			"unreachable unless the delete recorded what it orphaned", got)
	}
	// And the survivor is the RIGHT one: the keeper still reads back.
	if out, err := readAll(t, unit, string(keeper), "sha-keeper"); err != nil || string(out) != "bytes that must stay" {
		t.Fatalf("the sweep reclaimed the wrong paste's bytes: keeper reads (%q, %v)", out, err)
	}
}

// Deleting ONE version reclaims that version's bytes and leaves the others.
//
// The sharpest case for "reclaims the wrong one": two blobs under the SAME
// slug, so a reclamation keyed on the slug rather than the ref would take both
// and leave the surviving version serving nothing.
func TestDeleteVersion_ReclaimsOnlyThatVersionsBytes(t *testing.T) {
	repo, unit, bs := newBlobRepo(t)
	ctx := context.Background()
	slug := domain.Slug("twovers1")

	owned, err := unit.BeginUpload(ctx, string(slug))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	h1, err := unit.Stage(owned, string(slug), "sha-v1", encode(t, []byte("version one")))
	if err != nil {
		t.Fatalf("stage v1: %v", err)
	}
	if err := unit.Commit(owned, []service.BlobHandle{h1}, func(c context.Context) error {
		return repo.InsertWithQuotaCheck(c, domain.Paste{
			Slug: slug, Identity: "key:vers", Kind: domain.KindHTML,
			ContentSHA: "sha-v1", Size: 11,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}, 0, time.Now().UTC())
	}); err != nil {
		t.Fatalf("commit v1: %v", err)
	}

	owned2, err := unit.BeginUpload(ctx, string(slug))
	if err != nil {
		t.Fatalf("BeginUpload v2: %v", err)
	}
	h2, err := unit.Stage(owned2, string(slug), "sha-v2", encode(t, []byte("version two")))
	if err != nil {
		t.Fatalf("stage v2: %v", err)
	}
	if err := unit.Commit(owned2, []service.BlobHandle{h2}, func(c context.Context) error {
		_, aerr := repo.AppendVersionWithQuotaCheck(c, slug, domain.KindHTML, "sha-v2", 11, 0, time.Now().UTC())
		return aerr
	}); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	if got := countObjects(ctx, bs); got != 2 {
		t.Fatalf("fixture: %d objects, want 2 (one per version)", got)
	}

	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("delete version 1: %v", err)
	}
	if _, err := repo.SweepStagedBytes(ctx, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := countObjects(ctx, bs); got != 1 {
		t.Fatalf("after deleting v1 and sweeping: %d objects, want 1", got)
	}
	// v2 must still serve. If the sweep took both, this is where it shows.
	if out, err := readAll(t, unit, string(slug), "sha-v2"); err != nil || string(out) != "version two" {
		t.Fatalf("deleting v1 destroyed v2's bytes: v2 reads (%q, %v). A reclamation "+
			"keyed on the slug rather than the ref takes every version's blob", out, err)
	}
}
