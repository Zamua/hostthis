package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// fakeBlobs is a controllable BlobStore for the lifecycle tests: it can be
// told to fail PutPrecompressed and records the SHAs it stored.
type fakeBlobs struct {
	mu       sync.Mutex
	stored   map[string][]byte
	failPut  bool
	putCalls int
	// holdPut, when non-nil, parks every PutPrecompressed until the channel
	// is closed. The blob write is the background finalizer's FIRST act, so
	// parking it holds the whole finalizer (no MarkReady/MarkFailed can run),
	// which is what lets a test assert pre-finalize state without racing the
	// finalizer goroutine. Set before the first Create; never mutate once
	// uploads are in flight.
	holdPut chan struct{}
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{stored: map[string][]byte{}} }

type syncOrderBlob struct {
	stageCalls int
	stageErr   error
}

func (b *syncOrderBlob) StagePrecompressed(context.Context, string, io.Reader, int64) error {
	b.stageCalls++
	return b.stageErr
}
func (*syncOrderBlob) StageEncoding(context.Context, io.Reader) (string, int, error) {
	return "", 0, errors.New("unexpected encoding")
}
func (*syncOrderBlob) Read(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("unexpected read")
}

type syncOrderRepo struct {
	blob        *syncOrderBlob
	insertCalls int
	markCalls   int
	inserted    domain.Paste
}

func (r *syncOrderRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, _ int64, _ time.Time) error {
	if r.blob.stageCalls == 0 {
		return errors.New("metadata inserted before bytes")
	}
	r.insertCalls++
	r.inserted = p
	return nil
}
func (*syncOrderRepo) Get(domain.Slug) (domain.Paste, error) {
	return domain.Paste{}, domain.ErrNotFound
}
func (r *syncOrderRepo) MarkReady(domain.Paste) error  { r.markCalls++; return nil }
func (r *syncOrderRepo) MarkFailed(domain.Paste) error { r.markCalls++; return nil }

// Sync mode makes ready metadata visible only after its bytes are durable.
func TestUpload_SyncBlobOrdersBytesBeforeReadyMetadata(t *testing.T) {
	blob := &syncOrderBlob{}
	repo := &syncOrderRepo{blob: blob}
	u := NewUpload(repo, blob)
	u.SyncBlob = true

	res, err := u.Create(bytes.NewReader([]byte("# body")), "key:owner", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if blob.stageCalls != 1 || repo.insertCalls != 1 {
		t.Fatalf("stage/insert calls = %d/%d, want 1/1", blob.stageCalls, repo.insertCalls)
	}
	if res.Paste.Status != domain.PasteStatusReady || repo.inserted.Status != domain.PasteStatusReady {
		t.Fatalf("result/inserted status = %q/%q, want ready", res.Paste.Status, repo.inserted.Status)
	}
	if repo.markCalls != 0 {
		t.Fatalf("status transition calls = %d, want 0", repo.markCalls)
	}
}

// A failed synchronous byte write leaves no metadata behind.
func TestUpload_SyncBlobWriteFailureSkipsMetadata(t *testing.T) {
	writeErr := errors.New("blob unavailable")
	blob := &syncOrderBlob{stageErr: writeErr}
	repo := &syncOrderRepo{blob: blob}
	u := NewUpload(repo, blob)
	u.SyncBlob = true

	_, err := u.Create(bytes.NewReader([]byte("# body")), "key:owner", "", "")
	if !errors.Is(err, writeErr) {
		t.Fatalf("Create = %v, want blob error", err)
	}
	if repo.insertCalls != 0 || repo.markCalls != 0 {
		t.Fatalf("insert/status calls = %d/%d, want 0/0", repo.insertCalls, repo.markCalls)
	}
}

func (f *fakeBlobs) PutPrecompressed(sha string, body io.Reader, size int64) error {
	if f.holdPut != nil {
		<-f.holdPut
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("precompressed size mismatch")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	if f.failPut {
		return errors.New("simulated blob write failure")
	}
	f.stored[sha] = b
	return nil
}

func (f *fakeBlobs) Put(sha string, r io.Reader, size int64) error {
	return f.PutPrecompressed(sha, r, size)
}

func (f *fakeBlobs) Get(sha string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.stored[sha]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return b, nil
}

func (f *fakeBlobs) GetReader(sha string) (io.ReadCloser, int64, error) {
	b, err := f.Get(sha)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeBlobs) has(sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.stored[sha]
	return ok
}

// testBlobStore is the combined read+write surface a StandaloneBlobUnit needs,
// satisfied by the test fakes and by *storage.CompressedBlobStore.
type testBlobStore interface {
	BlobStore
	blobReadStore
}

// newStackWithBlobs wires the real metadata repo with a caller-supplied blob
// store (wrapped in the StandaloneBlobUnit seam) plus a finalize-done signal so
// tests can wait deterministically.
func newStackWithBlobs(t *testing.T, blobs testBlobStore) (*Upload, *storage.MemRepo, chan struct{}) {
	t.Helper()
	repo := storagetest.NewRepo(t)
	u := NewUpload(repo, NewStandaloneBlobUnit(blobs))
	done := make(chan struct{}, 8)
	u.onFinalizeDone = func() { done <- struct{}{} }
	t.Cleanup(u.WaitFinalize)
	return u, repo, done
}

func waitFinalize(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background finalize")
	}
}

// Create returns a PENDING paste before the blob write completes.
func TestUpload_Create_ReturnsPending(t *testing.T) {
	// Park the finalizer at its first act (the blob write) so the assertions
	// below cannot race it: without the hold, a slow runner lets the finalizer
	// flip the row to ready between Create returning and repo.Get.
	blobs := newFakeBlobs()
	blobs.holdPut = make(chan struct{})
	u, repo, done := newStackWithBlobs(t, blobs)
	res, err := u.Create(bytes.NewReader([]byte("<p>hi</p>")), "owner", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Status != domain.PasteStatusPending {
		t.Fatalf("returned status: got %q, want pending", res.Paste.Status)
	}
	// The persisted row is pending too: with the finalizer parked, this
	// observes the synchronous half's committed state deterministically.
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusPending {
		t.Fatalf("persisted status before finalize: got %q, want pending", got.Status)
	}
	// Release the finalizer and drain it so cleanup does not strand it.
	close(blobs.holdPut)
	waitFinalize(t, done)
}

// A finalizer cannot settle a replacement that reuses the slug.
func TestUpload_Finalize_FencesReplacementIncarnation(t *testing.T) {
	for _, failPut := range []bool{false, true} {
		name := "ready"
		if failPut {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			blobs := newFakeBlobs()
			blobs.holdPut = make(chan struct{})
			blobs.failPut = failPut
			u, repo, done := newStackWithBlobs(t, blobs)

			res, err := u.Create(bytes.NewReader([]byte("<p>old</p>")), "owner", "", "")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			old := res.Paste
			if err := repo.Delete(old.Slug, old.Identity, old.CreatedAt); err != nil {
				t.Fatalf("delete old incarnation: %v", err)
			}
			replacement := old
			replacement.Generation = domain.NewPasteGeneration()
			replacement.ContentSHA = "replacement-sha"
			replacement.Size = 1
			replacement.Status = domain.PasteStatusPending
			if err := repo.InsertWithQuotaCheck(context.Background(), replacement, 0, replacement.CreatedAt); err != nil {
				t.Fatalf("insert replacement: %v", err)
			}

			close(blobs.holdPut)
			waitFinalize(t, done)
			got, err := repo.Get(old.Slug)
			if err != nil {
				t.Fatalf("get replacement: %v", err)
			}
			if got.Generation != replacement.Generation || got.Status != domain.PasteStatusPending {
				t.Fatalf("replacement generation/status = %q/%q, want %q/pending", got.Generation, got.Status, replacement.Generation)
			}
		})
	}
}

// Happy path: the background finalizer writes the blob + flips to ready.
func TestUpload_Finalize_PendingToReady(t *testing.T) {
	blobs := newFakeBlobs()
	u, repo, done := newStackWithBlobs(t, blobs)
	res, err := u.Create(bytes.NewReader([]byte("<p>ready</p>")), "owner", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitFinalize(t, done)
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status after finalize: got %q, want ready", got.Status)
	}
	if !blobs.has(res.Paste.ContentSHA) {
		t.Fatalf("blob was not stored by finalize")
	}
}

// Background blob-write failure flips the paste to failed.
func TestUpload_Finalize_BlobFailureToFailed(t *testing.T) {
	blobs := newFakeBlobs()
	blobs.failPut = true
	u, repo, done := newStackWithBlobs(t, blobs)
	res, err := u.Create(bytes.NewReader([]byte("<p>doomed</p>")), "owner", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitFinalize(t, done)
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusFailed {
		t.Fatalf("status after failed finalize: got %q, want failed", got.Status)
	}
}

// A failed paste releases its reservation, so its bytes stop counting toward
// the owner's quota.
func TestUpload_Finalize_FailureReleasesQuota(t *testing.T) {
	blobs := newFakeBlobs()
	blobs.failPut = true
	u, repo, done := newStackWithBlobs(t, blobs)
	owner := "quota-owner"

	res, err := u.Create(bytes.NewReader([]byte("<p>first</p>")), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitFinalize(t, done)
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusFailed {
		t.Fatalf("status: got %q, want failed", got.Status)
	}
	// SumActiveBytesByOwner counts only non-failed pastes.
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if n != 0 {
		t.Fatalf("active bytes after failed paste: got %d, want 0 (reservation released)", n)
	}
}

// Quota is reserved SYNCHRONOUSLY in Create, before any URL is returned, even
// though the blob write is async: a pending paste counts against the owner
// immediately.
func TestUpload_Create_QuotaEnforcedSynchronously(t *testing.T) {
	blobs := newFakeBlobs()
	u, repo, done := newStackWithBlobs(t, blobs)
	owner := "cap-owner"

	res, err := u.Create(bytes.NewReader([]byte("<p>counts</p>")), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if n != res.Paste.Size {
		t.Fatalf("pending paste should count toward quota: got %d, want %d", n, res.Paste.Size)
	}
	waitFinalize(t, done)
}

// EncodeTo delegates to the real encoder so size assertions use the production
// at-rest format.
func (f *fakeBlobs) EncodeTo(w io.Writer, r io.Reader) (string, int, int64, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", 0, 0, err
	}
	body, err := storage.EncodeCompressedBody(bytes.NewReader(raw))
	if err != nil {
		return "", 0, 0, err
	}
	n, err := w.Write(body)
	return domain.HashContent(raw), len(body) - storage.CompressedBodyPrefixLen, int64(n), err
}
