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
// before this layer existed) are still readable - Get returns those
// as-is when the magic doesn't match.
//
// Compression happens in the storage layer so it's transparent to the
// service layer above: callers continue to think in terms of original
// uncompressed bytes, and dedup remains keyed on the sha256 of those
// original bytes.
type CompressedBlobStore struct {
	Inner innerBlobStore
}

// innerBlobStore is the minimal contract this wrapper depends on -
// avoids importing the service-layer interface and keeps things in
// the storage package.
type innerBlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
	GetReader(sha string) (io.ReadCloser, int64, error)
}

// InnerBlobStore is the exported alias of innerBlobStore so wiring code
// in other packages (cmd/) can name the type the compression layer
// wraps. The write-back cache satisfies it, so it can be slotted between
// the compression layer and the durable backend.
type InnerBlobStore = innerBlobStore

// PutPrecompressed writes a body that is ALREADY zstd-encoded with the
// magic prefix in place. Used by the streaming upload path in the
// service layer, which produces the encoded bytes incrementally as
// stdin arrives - no point asking this wrapper to compress them again.
func (c *CompressedBlobStore) PutPrecompressed(sha string, body []byte) error {
	return c.Inner.Put(sha, bytes.NewReader(body), int64(len(body)))
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

// zstd level - level 3 is the SpeedDefault. Compression ratio close
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
// upload (pre-compression-rollout) and returned as-is - backstop for
// the rolling migration so we don't have to mass-rewrite every blob
// before flipping the read path.
func (c *CompressedBlobStore) Get(sha string) ([]byte, error) {
	body, err := c.Inner.Get(sha)
	if err != nil {
		return nil, err
	}
	if !hasMagicV1(body) {
		// Legacy uncompressed - return as-is.
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

// GetReader streams the UNCOMPRESSED bytes for sha without buffering
// the whole blob into memory the way Get does. It reads the inner
// stored bytes lazily: peeks the 4-byte magic header, and either
//
//   - returns the remaining stream wrapped in a streaming zstd decoder
//     (the normal compressed case), or
//   - returns the bytes as-is (legacy uncompressed blobs without the
//     magic, same backstop Get applies).
//
// The returned reader yields byte-identical output to Get(sha) - a
// streaming zstd decode produces the same bytes as DecodeAll. The
// caller MUST Close it; Close releases both the zstd decoder and the
// underlying inner reader. The int64 is the inner (compressed) byte
// length reported by the inner store, not the decoded length, so the
// serve path does not use it for Content-Length.
func (c *CompressedBlobStore) GetReader(sha string) (io.ReadCloser, int64, error) {
	inner, size, err := c.Inner.GetReader(sha)
	if err != nil {
		return nil, 0, err
	}
	// Peek the magic header to decide compressed vs legacy. Read exactly
	// len(magicV1) bytes (or fewer if the blob is shorter); io.ReadFull
	// handles short blobs via ErrUnexpectedEOF / EOF.
	hdr := make([]byte, len(magicV1))
	n, rerr := io.ReadFull(inner, hdr)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		_ = inner.Close()
		return nil, 0, fmt.Errorf("compressed blob read header %s: %w", sha, rerr)
	}
	hdr = hdr[:n]
	if !hasMagicV1(hdr) {
		// Legacy uncompressed - return the peeked header prepended to the
		// rest of the inner stream, unwrapped.
		return newPrefixReadCloser(hdr, inner), size, nil
	}
	// Compressed: the magic is consumed; decode the remaining stream.
	dec, err := zstd.NewReader(inner)
	if err != nil {
		_ = inner.Close()
		return nil, 0, fmt.Errorf("compressed blob: zstd reader: %w", err)
	}
	return &zstdReadCloser{dec: dec, inner: inner}, size, nil
}

// prefixReadCloser serves a small in-memory prefix (the peeked magic
// header) before continuing from the underlying reader. Used for legacy
// uncompressed blobs where the peeked bytes are real content.
type prefixReadCloser struct {
	r      io.Reader
	closer io.Closer
}

func newPrefixReadCloser(prefix []byte, rc io.ReadCloser) *prefixReadCloser {
	return &prefixReadCloser{
		r:      io.MultiReader(bytes.NewReader(prefix), rc),
		closer: rc,
	}
}

func (p *prefixReadCloser) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *prefixReadCloser) Close() error               { return p.closer.Close() }

// zstdReadCloser couples a streaming zstd decoder to its underlying
// inner reader so Close releases both.
type zstdReadCloser struct {
	dec   *zstd.Decoder
	inner io.ReadCloser
}

func (z *zstdReadCloser) Read(b []byte) (int, error) { return z.dec.Read(b) }

func (z *zstdReadCloser) Close() error {
	z.dec.Close() // returns no error
	return z.inner.Close()
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
