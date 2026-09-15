package service

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Zamua/hostthis/internal/domain"
)

// BlobStore owns the at-rest encoding and the object namespace. Known lengths
// let S3-shaped backends avoid unknown-size multipart buffering.
type BlobStore interface {
	PutPrecompressed(key string, r io.Reader, size int64) error
	EncodeTo(w io.Writer, r io.Reader) (payloadSize int, totalSize int64, err error)
	DeletePrefix(prefix string) error
}

// blobReadStore is the streaming read surface used by StandaloneBlobUnit.
type blobReadStore interface {
	GetReader(key string) (io.ReadCloser, int64, error)
}

// StandaloneBlobUnit adapts an object store to the service byte port.
type StandaloneBlobUnit struct {
	store interface {
		BlobStore
		blobReadStore
	}
}

func NewStandaloneBlobUnit(store interface {
	BlobStore
	blobReadStore
}) *StandaloneBlobUnit {
	return &StandaloneBlobUnit{store: store}
}

func (u *StandaloneBlobUnit) StagePrecompressed(_ context.Context, key string, r io.Reader, size int64) error {
	return u.store.PutPrecompressed(key, r, size)
}

func (u *StandaloneBlobUnit) StageEncoding(ctx context.Context, key string, r io.Reader) (int, error) {
	staged, err := os.CreateTemp("", "hostthis-blob-*")
	if err != nil {
		return 0, fmt.Errorf("create blob spill: %w", err)
	}
	name := staged.Name()
	defer os.Remove(name) //nolint:errcheck
	defer staged.Close()  //nolint:errcheck

	size, total, err := u.store.EncodeTo(staged, r)
	if err != nil {
		return 0, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewind blob spill: %w", err)
	}
	if err := u.StagePrecompressed(ctx, key, staged, total); err != nil {
		return 0, err
	}
	return size, nil
}

// Read answers an entry without an object key with ErrNotFound before touching
// the store (docs/SPEC.md "Entries without an object key").
func (u *StandaloneBlobUnit) Read(_ context.Context, entry domain.ManifestEntry) (io.ReadCloser, int64, error) {
	if entry.Key == "" {
		return nil, 0, domain.ErrNotFound
	}
	return u.store.GetReader(entry.Key)
}

func (u *StandaloneBlobUnit) DeleteUpload(_ context.Context, uploadID string) error {
	if !domain.ValidUploadID(uploadID) {
		return fmt.Errorf("blob: invalid upload id %q", uploadID)
	}
	return u.store.DeletePrefix(domain.UploadPrefix(uploadID))
}

var _ BlobUnit = (*StandaloneBlobUnit)(nil)
