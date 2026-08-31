package service

import (
	"context"
	"fmt"
	"io"
	"os"
)

// blobReadStore is the streaming read surface used by StandaloneBlobUnit.
type blobReadStore interface {
	GetReader(sha string) (io.ReadCloser, int64, error)
}

// StandaloneBlobUnit adapts a content-addressed store to the service byte port.
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

func (u *StandaloneBlobUnit) StagePrecompressed(_ context.Context, sha string, r io.Reader, size int64) error {
	return u.store.PutPrecompressed(sha, r, size)
}

func (u *StandaloneBlobUnit) StageEncoding(ctx context.Context, r io.Reader) (string, int, error) {
	staged, err := os.CreateTemp("", "hostthis-blob-*")
	if err != nil {
		return "", 0, fmt.Errorf("create blob spill: %w", err)
	}
	name := staged.Name()
	defer os.Remove(name) //nolint:errcheck
	defer staged.Close()  //nolint:errcheck

	sha, size, total, err := u.store.EncodeTo(staged, r)
	if err != nil {
		return "", 0, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("rewind blob spill: %w", err)
	}
	if err := u.StagePrecompressed(ctx, sha, staged, total); err != nil {
		return "", 0, err
	}
	return sha, size, nil
}

func (u *StandaloneBlobUnit) Read(_ context.Context, sha string) (io.ReadCloser, int64, error) {
	return u.store.GetReader(sha)
}

var _ BlobUnit = (*StandaloneBlobUnit)(nil)
