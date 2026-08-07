package service

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// fullBlobStore models the object store rejecting a Put at its bucket quota:
// every write fails with storage.ErrServiceFull. Reads delegate to a real disk
// store, so it carries the whole surface a StandaloneBlobUnit needs.
type fullBlobStore struct{ real *storage.CompressedBlobStore }

func (f fullBlobStore) Put(sha string, r io.Reader, size int64) error {
	_, _ = io.Copy(io.Discard, r)
	return storage.ErrServiceFull
}
func (f fullBlobStore) PutPrecompressed(sha string, body []byte) error {
	return storage.ErrServiceFull
}
func (f fullBlobStore) Get(sha string) ([]byte, error) { return f.real.Get(sha) }
func (f fullBlobStore) GetReader(sha string) (io.ReadCloser, int64, error) {
	return f.real.GetReader(sha)
}

// fullBlobUnit wraps fullBlobStore as the BlobUnit seam: Stage fails, reads
// delegate.
func fullBlobUnit(t *testing.T) *StandaloneBlobUnit {
	t.Helper()
	return NewStandaloneBlobUnit(fullBlobStore{real: realBlobs(t)})
}

func realBlobs(t *testing.T) *storage.CompressedBlobStore {
	t.Helper()
	disk, err := storage.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return storage.NewCompressedBlobStore(disk)
}

// TestUpload_BlobQuotaDrivesFinalizeToFailed pins how the durable ceiling
// surfaces on the ASYNC create path: Create hands out the URL as pending
// before the blob is attempted, so the bucket-quota rejection cannot be a
// Create error - it lands as the paste's failed status (SPEC "Paste lifecycle
// status -> Finalize: the asynchronous half").
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

// TestManageUpdate_BlobQuotaSurfacesServiceFull pins that the synchronous
// update path returns the graceful ErrServiceFull, not a wrapped blob-write
// error.
func TestManageUpdate_BlobQuotaSurfacesServiceFull(t *testing.T) {
	dir := t.TempDir()
	repo := storagetest.NewRepo(t)

	// An owned paste, so Update reaches the blob write instead of being
	// rejected by the owner check.
	disk, err := storage.NewBlobStore(filepath.Join(dir, "seedblobs"))
	if err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	seedBlobs := storage.NewCompressedBlobStore(disk)
	up := NewUpload(repo, NewStandaloneBlobUnit(seedBlobs))
	t.Cleanup(up.WaitFinalize)
	res, err := up.Create(bytes.NewReader([]byte("<!doctype html><p>v1</p>")), "key:owner", "demo", "")
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	up.WaitFinalize() // drain the seed's async blob write before Update

	m := NewManage(repo, fullBlobUnit(t))
	_, err = m.Update(res.Paste.Slug, "key:owner", bytes.NewReader([]byte("<!doctype html><p>v2</p>")), "")
	if !errors.Is(err, ErrServiceFull) {
		t.Fatalf("blob-quota update = %v, want service.ErrServiceFull", err)
	}
}

// TestDeploySite_BlobQuotaSurfacesServiceFull pins that the blob Put rejection
// propagates out through the safe-untar sink as ErrServiceFull.
func TestDeploySite_BlobQuotaSurfacesServiceFull(t *testing.T) {
	sites := storage.NewArtifactSites(storagetest.NewRepo(t), nil)
	pastes := storagetest.NewRepo(t)
	d := NewDeploySite(sites, pastes, fullBlobUnit(t))

	arc := gzipTar(t, map[string]string{"index.html": "<!doctype html><h1>hi</h1>"})
	_, err := d.Deploy(bytes.NewReader(arc), "key:owner")
	if !errors.Is(err, ErrServiceFull) {
		t.Fatalf("blob-quota deploy = %v, want service.ErrServiceFull", err)
	}
}

// EncodeBody delegates to the real at-rest encoder rather than faking a size:
// a fabricated length here would let a size regression pass.
func (f fullBlobStore) EncodeBody(r io.Reader) ([]byte, int, error) {
	body, err := storage.EncodeCompressedBody(r)
	if err != nil {
		return nil, 0, err
	}
	return body, len(body) - storage.CompressedBodyPrefixLen, nil
}
