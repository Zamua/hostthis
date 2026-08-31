package service

import (
	"context"
	"io"
)

// BlobUnit is the detached content-addressed byte plane used by application
// services. Metadata commits name blobs by SHA; reclamation is a separate
// reachability sweep.
type BlobUnit interface {
	// StagePrecompressed persists bytes already in the at-rest format.
	StagePrecompressed(ctx context.Context, sha string, r io.Reader, size int64) error

	// StageEncoding encodes and persists uncompressed bytes, returning their SHA
	// and quota-relevant stored size.
	StageEncoding(ctx context.Context, r io.Reader) (sha string, storedSize int, err error)

	// Read streams the decompressed bytes addressed by sha.
	Read(ctx context.Context, sha string) (io.ReadCloser, int64, error)
}
