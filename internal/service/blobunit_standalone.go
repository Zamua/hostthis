package service

import (
	"context"
	"io"
)

// blobReadStore is the read surface StandaloneBlobUnit needs on top of the
// write+buffered-read BlobStore the services already hold. *storage.
// CompressedBlobStore satisfies both. GetReader streams the decompressed
// bytes; Get buffers them. Kept here (not imported from http) so the
// service layer does not depend on the http package.
type blobReadStore interface {
	GetReader(sha string) (io.ReadCloser, int64, error)
	Get(sha string) ([]byte, error)
}

// StandaloneBlobUnit adapts the existing detached content-addressed blob
// store (the *storage.CompressedBlobStore over disk or S3) to the BlobUnit
// seam. It is the ONLY implementation today; every metadata backend
// (sqlite, slatedb, shale) uses it, so wiring the services through the seam
// changes nothing at runtime - this is a behavior-preserving refactor.
//
// The mapping to today's direct calls:
//
//   - Stage           -> BlobStore.PutPrecompressed(sha, body)
//   - StageStream      -> BlobStore.Put(sha, r, size)  (compresses at rest)
//   - Commit           -> just runs metaWrite; the bytes are already durable
//     from Stage/StageStream, so there is no separate bind. This reproduces
//     the pre-seam blob-first ordering (write the blob, then the metadata
//     row) that update and deploy already used.
//   - Read             -> GetReader(sha)   (streaming, decompressed)
//   - ReadAll          -> Get(sha)         (buffered, decompressed)
//   - UnbindOnDelete   -> no-op. Blobs are content-addressed and reclaimed
//     by the global content-addressed sweep when no live record references
//     their sha; a delete never removes bytes directly on this path.
type StandaloneBlobUnit struct {
	store interface {
		BlobStore
		blobReadStore
	}
}

// NewStandaloneBlobUnit wraps a content-addressed blob store (the same
// *storage.CompressedBlobStore the services hold today) as a BlobUnit.
func NewStandaloneBlobUnit(store interface {
	BlobStore
	blobReadStore
}) *StandaloneBlobUnit {
	return &StandaloneBlobUnit{store: store}
}

// Stage writes the precompressed body and returns its (slug, sha) handle.
func (u *StandaloneBlobUnit) Stage(_ context.Context, slug, sha string, body []byte) (BlobHandle, error) {
	if err := u.store.PutPrecompressed(sha, body); err != nil {
		return BlobHandle{}, err
	}
	return BlobHandle{Slug: slug, SHA: sha}, nil
}

// StageStream streams r (uncompressed) into the store, which compresses it
// at rest, and returns the (slug, sha) handle.
func (u *StandaloneBlobUnit) StageStream(_ context.Context, slug, sha string, r io.Reader, size int64) (BlobHandle, error) {
	if err := u.store.Put(sha, r, size); err != nil {
		return BlobHandle{}, err
	}
	return BlobHandle{Slug: slug, SHA: sha}, nil
}

// Commit runs the record's metadata write. The staged bytes are already
// durable, so there is nothing to bind: Commit returns metaWrite's error
// verbatim (preserving the caller's slug-collision / quota error handling).
func (u *StandaloneBlobUnit) Commit(_ context.Context, _ []BlobHandle, metaWrite func() error) error {
	return metaWrite()
}

// Read streams the decompressed bytes for sha (slug is unused on this path -
// blobs are keyed by content sha alone).
func (u *StandaloneBlobUnit) Read(_ context.Context, _ /*slug*/, sha string) (io.ReadCloser, int64, error) {
	return u.store.GetReader(sha)
}

// ReadAll returns the full decompressed bytes for sha.
func (u *StandaloneBlobUnit) ReadAll(_ context.Context, _ /*slug*/, sha string) ([]byte, error) {
	return u.store.Get(sha)
}

// UnbindOnDelete is a no-op on the standalone path: the global sweep
// reclaims unreferenced blobs by content sha.
func (u *StandaloneBlobUnit) UnbindOnDelete(_ context.Context, _ string, _ []string) error {
	return nil
}

// Ensure StandaloneBlobUnit satisfies the seam.
var _ BlobUnit = (*StandaloneBlobUnit)(nil)
