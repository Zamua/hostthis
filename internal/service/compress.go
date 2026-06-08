package service

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// compressedSize returns the post-zstd byte count for body. Used by
// Upload + Update to enforce the per-paste cap + the per-identity
// quota in compressed-byte terms (matching what gets stored).
//
// Allocates a buffer the size of the compressed output; for the
// 10 MiB cap this peaks well under a megabyte on realistic input.
// Uses the same encoder level as the storage layer's
// CompressedBlobStore — values agree by construction.
func compressedSize(body []byte) (int, error) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return 0, fmt.Errorf("compressedSize: zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, bytes.NewReader(body)); err != nil {
		_ = enc.Close()
		return 0, fmt.Errorf("compressedSize: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return 0, fmt.Errorf("compressedSize: close: %w", err)
	}
	return buf.Len(), nil
}
