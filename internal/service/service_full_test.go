package service

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// fullBlobStore is a BlobStore whose writes always fail with
// storage.ErrServiceFull, simulating the object store rejecting a Put
// because its bucket is at the configured hard quota (the durable
// total-bytes ceiling). Reads delegate to a real disk store so the Manage
// show path (if exercised) still works.
type fullBlobStore struct{ real BlobStore }

func (f fullBlobStore) Put(sha string, r io.Reader, size int64) error {
	_, _ = io.Copy(io.Discard, r)
	return storage.ErrServiceFull
}
func (f fullBlobStore) PutPrecompressed(sha string, body []byte) error {
	return storage.ErrServiceFull
}
func (f fullBlobStore) Get(sha string) ([]byte, error) { return f.real.Get(sha) }

// realBlobs builds a real compressed disk blob store for the read side.
func realBlobs(t *testing.T) BlobStore {
	t.Helper()
	disk, err := storage.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return storage.NewCompressedBlobStore(disk)
}

// TestUpload_BlobQuotaDrivesFinalizeToFailed pins the create-path ceiling
// contract under the ASYNC blob write: Create returns a pending paste (the
// URL is handed out before the blob is attempted), then the background
// finalizer's blob Put is rejected by the object-store bucket quota
// (storage.ErrServiceFull) and the paste transitions to failed. The
// durable-ceiling rejection can no longer be a synchronous Create error on
// this path - the bytes are written in the background - so the failure
// surfaces as the paste's failed status + a released reservation (see SPEC
// "Paste lifecycle status -> Finalize: the asynchronous half"). The Update
// and DeploySite paths remain synchronous and still return ErrServiceFull
// directly (their tests below).
func TestUpload_BlobQuotaDrivesFinalizeToFailed(t *testing.T) {
	u, repo, done := newStackWithBlobs(t, fullBlobStore{real: realBlobs(t)})

	res, err := u.Create(bytes.NewReader([]byte("<!doctype html><p>hi</p>")), "key:owner", "demo", "")
	if err != nil {
		t.Fatalf("create should succeed (pending) before the async blob write: %v", err)
	}
	if res.Paste.Status != domain.PasteStatusPending {
		t.Fatalf("status: got %q, want pending", res.Paste.Status)
	}
	waitFinalize(t, done)
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PasteStatusFailed {
		t.Fatalf("status after blob-quota failure: got %q, want failed", got.Status)
	}
}

// TestManageUpdate_BlobQuotaSurfacesServiceFull pins the same translation on
// the paste-update path: a blob Put rejected by the bucket quota becomes the
// graceful ErrServiceFull rather than a wrapped blob-write error.
func TestManageUpdate_BlobQuotaSurfacesServiceFull(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewPasteRepo(db)

	// Seed an owned paste so Update reaches the blob write before the
	// owner check can reject it.
	disk, err := storage.NewBlobStore(filepath.Join(dir, "seedblobs"))
	if err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	seedBlobs := storage.NewCompressedBlobStore(disk)
	up := NewUpload(repo, seedBlobs)
	res, err := up.Create(bytes.NewReader([]byte("<!doctype html><p>v1</p>")), "key:owner", "demo", "")
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	up.WaitFinalize() // drain the seed's async blob write before Update

	m := NewManage(repo, fullBlobStore{real: realBlobs(t)})
	_, err = m.Update(res.Paste.Slug, "key:owner", bytes.NewReader([]byte("<!doctype html><p>v2</p>")), "")
	if !errors.Is(err, ErrServiceFull) {
		t.Fatalf("blob-quota update = %v, want service.ErrServiceFull", err)
	}
}

// TestDeploySite_BlobQuotaSurfacesServiceFull pins the translation on the
// site-deploy path: the blob Put rejection propagates through the safe-untar
// sink and DeploySite.Deploy returns the graceful ErrServiceFull.
func TestDeploySite_BlobQuotaSurfacesServiceFull(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sites := storage.NewSiteRepo(db)
	pastes := storage.NewPasteRepo(db)
	d := NewDeploySite(sites, pastes, fullBlobStore{real: realBlobs(t)})

	arc := gzipTar(t, map[string]string{"index.html": "<!doctype html><h1>hi</h1>"})
	_, err = d.Deploy(bytes.NewReader(arc), "key:owner")
	if !errors.Is(err, ErrServiceFull) {
		t.Fatalf("blob-quota deploy = %v, want service.ErrServiceFull", err)
	}
}
