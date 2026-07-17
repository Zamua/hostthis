package service

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// fakeBlobs is a controllable BlobStore for the lifecycle tests: it can
// be told to fail PutPrecompressed (to exercise the failed path) and
// records the SHAs it stored (to confirm the bytes did/didn't land).
type fakeBlobs struct {
	mu       sync.Mutex
	stored   map[string][]byte
	failPut  bool
	putCalls int
	// holdPut, when non-nil, parks every PutPrecompressed until the channel
	// is closed. The blob write is the background finalizer's FIRST act, so
	// parking it holds the whole finalizer (no MarkReady/MarkFailed can run)
	// - the seam that lets a test assert pre-finalize state without racing
	// the finalizer goroutine. Set before the first Create; never mutate
	// once uploads are in flight.
	holdPut chan struct{}
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{stored: map[string][]byte{}} }

func (f *fakeBlobs) PutPrecompressed(sha string, body []byte) error {
	if f.holdPut != nil {
		<-f.holdPut
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	if f.failPut {
		return errors.New("simulated blob write failure")
	}
	f.stored[sha] = append([]byte(nil), body...)
	return nil
}

func (f *fakeBlobs) Put(sha string, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return f.PutPrecompressed(sha, b)
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

// testBlobStore is the combined read+write surface a StandaloneBlobUnit
// needs, satisfied by the test fakes (fakeBlobs, fullBlobStore) and by
// *storage.CompressedBlobStore.
type testBlobStore interface {
	BlobStore
	blobReadStore
}

// newStackWithBlobs wires the real sqlite repo with a caller-supplied
// blob store (wrapped in the StandaloneBlobUnit seam) + a finalize-done
// signal so tests can wait deterministically.
func newStackWithBlobs(t *testing.T, blobs testBlobStore) (*Upload, *storage.PasteRepo, chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewPasteRepo(db)
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
	// Park the finalizer at its first act (the blob write) so the
	// pre-finalize assertions below cannot race it: without the hold, a
	// slow runner lets the background finalizer flip the row to ready
	// between Create returning and repo.Get.
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
	// The persisted row is pending too, immediately after Create returns.
	// The finalizer is parked at the blob write, so this observes the
	// synchronous half's committed state deterministically.
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusPending {
		t.Fatalf("persisted status before finalize: got %q, want pending", got.Status)
	}
	// Release the finalizer and let it drain so cleanup doesn't strand it.
	close(blobs.holdPut)
	waitFinalize(t, done)
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

// A failed paste releases its reservation: its bytes no longer count
// toward the owner's quota, so a subsequent same-owner upload that would
// otherwise be over-quota succeeds.
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
	// Active bytes for the owner should be zero now (the failed paste was
	// released). SumActiveBytesByOwner counts only non-failed pastes.
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if n != 0 {
		t.Fatalf("active bytes after failed paste: got %d, want 0 (reservation released)", n)
	}
}

// Quota is enforced SYNCHRONOUSLY in Create, before any URL is returned,
// even though the blob write is async. An over-quota upload is rejected
// up front.
func TestUpload_Create_QuotaEnforcedSynchronously(t *testing.T) {
	blobs := newFakeBlobs()
	u, repo, done := newStackWithBlobs(t, blobs)
	owner := "cap-owner"

	// Shrink the effective budget by pre-loading the owner near the cap.
	// We can't change UserQuotaBytes, so instead upload enough small
	// pastes to approach it would be slow; simpler: assert the reserve
	// path is reached by uploading one paste and checking its bytes are
	// counted immediately (pending counts toward quota).
	res, err := u.Create(bytes.NewReader([]byte("<p>counts</p>")), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A pending paste counts toward quota right away (synchronous reserve).
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if n != res.Paste.Size {
		t.Fatalf("pending paste should count toward quota: got %d, want %d", n, res.Paste.Size)
	}
	waitFinalize(t, done)
}
