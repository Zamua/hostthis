package storage

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// CompressedBlobStore wraps another BlobStore and transparently zstd-
// encodes bytes on Put, decodes on Get. The wire/disk format adds a
// 4-byte magic prefix `HZ\0\x01` so legacy uncompressed blobs (written
// before this layer existed) are still readable — Get returns those
// as-is when the magic doesn't match.
//
// Compression happens in the storage layer so it's transparent to the
// service layer above: callers continue to think in terms of original
// uncompressed bytes, and dedup remains keyed on the sha256 of those
// original bytes.
type CompressedBlobStore struct {
	Inner innerBlobStore
}

// innerBlobStore is the minimal contract this wrapper depends on —
// avoids importing the service-layer interface and keeps things in
// the storage package.
type innerBlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
}

// magic prefix for blobs written by this layer.
//
//   - bytes 0..1: 'H' 'Z'             (hostthis-zstd)
//   - byte 2:     0x00                 (reserved)
//   - byte 3:     0x01                 (format version 1)
//
// 4 bytes is short enough to be cheap to inspect on every Get and
// distinct enough that no real HTML/Markdown blob (which all start
// with `<` or `#` etc.) would match by accident.
var magicV1 = [4]byte{'H', 'Z', 0x00, 0x01}

// zstd level — level 3 is the SpeedDefault. Compression ratio close
// to the slower levels for HTML/text content; ~500 MB/s encode and
// ~1 GB/s decode on modern hardware.
const compressionLevel = zstd.SpeedDefault

// NewCompressedBlobStore wraps inner with the compression layer.
func NewCompressedBlobStore(inner innerBlobStore) *CompressedBlobStore {
	return &CompressedBlobStore{Inner: inner}
}

// Put reads UNCOMPRESSED bytes from r, encodes them as `magic +
// zstd(bytes)`, and writes the result to the inner store. The inner
// store's `size` argument receives the COMPRESSED size; callers who
// pass an UNCOMPRESSED estimate (e.g. S3 wanting Content-Length) get
// the right number for what's actually being uploaded.
func (c *CompressedBlobStore) Put(sha string, r io.Reader, _ int64) error {
	var buf bytes.Buffer
	buf.Grow(int(estimatedCompressedSize(0))) // tiny header bump
	if _, err := buf.Write(magicV1[:]); err != nil {
		return fmt.Errorf("compressed blob write magic: %w", err)
	}
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(compressionLevel))
	if err != nil {
		return fmt.Errorf("compressed blob: zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		_ = enc.Close()
		return fmt.Errorf("compressed blob encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("compressed blob close encoder: %w", err)
	}
	body := buf.Bytes()
	return c.Inner.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// Get reads bytes from the inner store, strips the magic + zstd-
// decodes when present, and returns the UNCOMPRESSED bytes. A blob
// without the magic header is treated as a legacy uncompressed
// upload (pre-compression-rollout) and returned as-is — backstop for
// the rolling migration so we don't have to mass-rewrite every blob
// before flipping the read path.
func (c *CompressedBlobStore) Get(sha string) ([]byte, error) {
	body, err := c.Inner.Get(sha)
	if err != nil {
		return nil, err
	}
	if !hasMagicV1(body) {
		// Legacy uncompressed — return as-is.
		return body, nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("compressed blob: zstd reader: %w", err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(body[len(magicV1):], nil)
	if err != nil {
		return nil, fmt.Errorf("compressed blob decode %s: %w", sha, err)
	}
	return out, nil
}

func hasMagicV1(b []byte) bool {
	return len(b) >= len(magicV1) &&
		b[0] == magicV1[0] && b[1] == magicV1[1] &&
		b[2] == magicV1[2] && b[3] == magicV1[3]
}

// estimatedCompressedSize gives Buffer a small head start when we
// know the uncompressed size; minor optimization. Returns at least
// the magic header length so the empty-input case still allocates.
func estimatedCompressedSize(uncompressed int) int {
	// zstd worst-case is roughly +0.5% + 16 bytes. We add the magic.
	if uncompressed <= 0 {
		return len(magicV1) + 64
	}
	return len(magicV1) + uncompressed/2 // optimistic; Buffer grows if needed
}
