package service

import (
	"context"
	"io"
)

// blobReadStore is the read surface StandaloneBlobUnit needs on top of the
// write+buffered-read BlobStore the services already hold: GetReader streams
// the decompressed bytes, Get buffers them. Declared here rather than imported
// from http so the service layer does not depend on the http package.
type blobReadStore interface {
	GetReader(sha string) (io.ReadCloser, int64, error)
	Get(sha string) ([]byte, error)
}

// StandaloneBlobUnit adapts the detached content-addressed blob store (a
// *storage.CompressedBlobStore) to the BlobUnit seam. Bytes are durable the
// moment Stage/StageStream returns, so the ordering is blob-first and Commit
// has nothing to bind.
type StandaloneBlobUnit struct {
	store interface {
		BlobStore
		blobReadStore
	}
}

// NewStandaloneBlobUnit wraps a content-addressed blob store as a BlobUnit.
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
// verbatim, preserving the caller's slug-collision / quota error handling.
func (u *StandaloneBlobUnit) Commit(ctx context.Context, _ []BlobHandle, metaWrite func(context.Context) error) error {
	return metaWrite(ctx)
}

// Read streams the decompressed bytes for sha. slug is unused: blobs are keyed
// by content sha alone.
func (u *StandaloneBlobUnit) Read(_ context.Context, _ /*slug*/, sha string) (io.ReadCloser, int64, error) {
	return u.store.GetReader(sha)
}

// ReadAll returns the full decompressed bytes for sha.
func (u *StandaloneBlobUnit) ReadAll(_ context.Context, _ /*slug*/, sha string) ([]byte, error) {
	return u.store.Get(sha)
}

// UnbindOnDelete is a no-op: the global content-addressed sweep reclaims blobs
// no live record references, so a delete never removes bytes directly.
func (u *StandaloneBlobUnit) UnbindOnDelete(_ context.Context, _ string, _ []string) error {
	return nil
}

// IsTransactional is false: the bytes land in a detached store with no
// co-commit, so Upload.Create keeps the pending/finalizer model on this path
// (commit a PENDING row, write the bytes in the background, flip to ready).
func (u *StandaloneBlobUnit) IsTransactional() bool { return false }

var _ BlobUnit = (*StandaloneBlobUnit)(nil)

// BeginUpload is a no-op on the standalone path: its Stage writes bytes that
// are immediately durable and content-addressed, so there is no staged-but-
// unowned window for a recovery to reclaim and therefore nothing to fence.
func (u *StandaloneBlobUnit) BeginUpload(ctx context.Context, _ string) (context.Context, error) {
	return ctx, nil
}

// StageEncoding implements BlobUnit for the standalone path, on the same
// contract as the transactional adapter: encode to the at-rest format, stage
// it, report the compressed size excluding the framing prefix.
func (u *StandaloneBlobUnit) StageEncoding(ctx context.Context, slug, sha string, r io.Reader) (BlobHandle, int, error) {
	body, size, err := u.store.EncodeBody(r)
	if err != nil {
		return BlobHandle{}, 0, err
	}
	h, err := u.Stage(ctx, slug, sha, body)
	if err != nil {
		return BlobHandle{}, 0, err
	}
	return h, size, nil
}
