package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdDecoderPool reuses streaming zstd decoders across blob reads. A
// fresh zstd.NewReader allocates a large fixed working set (window /
// history buffers + per-goroutine decode scratch, ~10 MiB with the
// klauspost defaults) that is INDEPENDENT of the blob size, so paying it
// per GET dominated the serve path's allocation profile (every HTML and
// raw-markdown read decodes through here). Pooling pays that cost once
// and reuses it via Decoder.Reset: the buffers stay warm between reads
// instead of being allocated-then-GC'd each time. The pool is reaped by
// the GC under memory pressure, so idle footprint stays bounded.
//
// New never returns nil: zstd.NewReader(nil) only errors on invalid
// options, and we pass none.
var zstdDecoderPool = sync.Pool{
	New: func() any {
		d, _ := zstd.NewReader(nil)
		return d
	},
}

// getPooledDecoder borrows a decoder from the pool and points it at r via
// Reset (Reset reconfigures a reusable decoder for a new stream without
// re-allocating its buffers). On a Reset error the decoder is discarded
// (Close, not returned to the pool) so a bad decoder never gets reused.
func getPooledDecoder(r io.Reader) (*zstd.Decoder, error) {
	d := zstdDecoderPool.Get().(*zstd.Decoder)
	if err := d.Reset(r); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// putPooledDecoder detaches the decoder from its stream (Reset(nil)
// releases the reference to the inner reader and readies it for reuse -
// crucially NOT Close, which frees the buffers we want to keep) and
// returns it to the pool. If Reset fails the decoder is freed instead of
// pooled.
func putPooledDecoder(d *zstd.Decoder) {
	if err := d.Reset(nil); err != nil {
		d.Close()
		return
	}
	zstdDecoderPool.Put(d)
}

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
	body, err := EncodeCompressedBody(r)
	if err != nil {
		return err
	}
	return c.Inner.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// EncodeCompressedBody reads UNCOMPRESSED bytes from r and returns the
// `magic + zstd(bytes)` body buffered in memory - the exact at-rest format Put
// writes and DecodeCompressedStream reads. Factored out so the shale-blob stage
// path (which streams to BlobKV.StageBlob, NOT through CompressedBlobStore.Put)
// stores files in the SAME compressed format, so a site file read on the shale
// path decodes identically to a paste read and to the standalone path. The body
// is bounded by one file (the untar's per-file cap), so buffering is safe.
func EncodeCompressedBody(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(int(estimatedCompressedSize(0)))
	if _, err := buf.Write(magicV1[:]); err != nil {
		return nil, fmt.Errorf("compressed blob write magic: %w", err)
	}
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(compressionLevel))
	if err != nil {
		return nil, fmt.Errorf("compressed blob: zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("compressed blob encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("compressed blob close encoder: %w", err)
	}
	return buf.Bytes(), nil
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
	dec, derr := DecodeCompressedStream(inner, sha)
	if derr != nil {
		return nil, 0, derr
	}
	return dec, size, nil
}

// DecodeCompressedStream wraps a raw stored blob stream (the magic+zstd body,
// or a legacy uncompressed blob) in a reader that yields the DECOMPRESSED bytes,
// closing the underlying reader on Close. It peeks the magic header to decide
// compressed vs legacy, exactly as GetReader did inline; factored out so the
// shale-blob read path (which streams GetBlob's raw object) reuses the SAME
// decode as the standalone GetReader. On any error it closes rc and returns the
// error. label names the blob in error messages (a sha or a blob id).
func DecodeCompressedStream(rc io.ReadCloser, label string) (io.ReadCloser, error) {
	// Peek the magic header to decide compressed vs legacy. Read exactly
	// len(magicV1) bytes (or fewer if the blob is shorter); io.ReadFull
	// handles short blobs via ErrUnexpectedEOF / EOF.
	hdr := make([]byte, len(magicV1))
	n, rerr := io.ReadFull(rc, hdr)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		_ = rc.Close()
		return nil, fmt.Errorf("compressed blob read header %s: %w", label, rerr)
	}
	hdr = hdr[:n]
	if !hasMagicV1(hdr) {
		// Legacy uncompressed - return the peeked header prepended to the
		// rest of the inner stream, unwrapped.
		return newPrefixReadCloser(hdr, rc), nil
	}
	// Compressed: the magic is consumed; decode the remaining stream with a
	// pooled decoder (Reset onto rc, returned to the pool on Close).
	dec, err := getPooledDecoder(rc)
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("compressed blob %s: zstd reader: %w", label, err)
	}
	return &zstdReadCloser{dec: dec, inner: rc}, nil
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
	// Return the decoder to the pool (Reset(nil) detaches the inner stream)
	// rather than Close it, so its buffers stay warm for the next read. The
	// inner reader is still closed here. Safe even if the caller didn't read
	// to EOF (an aborted download): Reset readies the decoder regardless.
	putPooledDecoder(z.dec)
	z.dec = nil
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
