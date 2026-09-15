package service

import (
	"context"
	"io"

	"github.com/Zamua/hostthis/internal/domain"
)

// BlobUnit is the byte plane used by application services. Every object an
// upload stages lives under that upload's own prefix, so deleting one upload's
// bytes can never touch another's.
type BlobUnit interface {
	// StagePrecompressed persists bytes already in the at-rest format at key.
	StagePrecompressed(ctx context.Context, key string, r io.Reader, size int64) error

	// StageEncoding encodes and persists uncompressed bytes at key, returning
	// their quota-relevant stored size.
	StageEncoding(ctx context.Context, key string, r io.Reader) (storedSize int, err error)

	// Read streams the decompressed bytes at an entry's object key. An entry
	// without a key is ErrNotFound.
	Read(ctx context.Context, entry domain.ManifestEntry) (io.ReadCloser, int64, error)

	// DeleteUpload deletes every object under the upload's prefix. An upload
	// with nothing stored succeeds.
	DeleteUpload(ctx context.Context, uploadID string) error
}
