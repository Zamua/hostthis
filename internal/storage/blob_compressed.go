package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdDecoderPool reuses streaming zstd decoders across blob reads. A fresh
// zstd.NewReader allocates a large working set (~10 MiB at the klauspost
// defaults) INDEPENDENT of the blob size, so paying it per GET would dominate
// the serve path's allocations. The GC reaps the pool under memory pressure.
//
// New never returns nil: zstd.NewReader(nil) only errors on invalid options.
var zstdDecoderPool = sync.Pool{
	New: func() any {
		d, _ := zstd.NewReader(nil)
		return d
	},
}

// getPooledDecoder borrows a decoder and points it at r via Reset, which
// reconfigures it for a new stream without re-allocating its buffers. A Reset
// error discards the decoder rather than pooling it, so a bad one is never
// handed out again.
func getPooledDecoder(r io.Reader) (*zstd.Decoder, error) {
	d := zstdDecoderPool.Get().(*zstd.Decoder)
	if err := d.Reset(r); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// putPooledDecoder detaches the decoder from its stream and returns it to the
// pool. Reset(nil), NOT Close: Close frees the very buffers the pool exists to
// keep warm. A failed Reset frees the decoder instead of pooling it.
func putPooledDecoder(d *zstd.Decoder) {
	if err := d.Reset(nil); err != nil {
		d.Close()
		return
	}
	zstdDecoderPool.Put(d)
}

// CompressedBlobStore owns the at-rest encoding over a raw object store:
// writes arrive already encoded, reads decode. An object without the magic
// prefix is returned as-is, so uncompressed objects stay readable.
type CompressedBlobStore struct {
	Inner innerBlobStore
}

// innerBlobStore is the minimal contract this wrapper depends on, declared
// here so the storage package need not import the service-layer interface.
type innerBlobStore interface {
	Put(key string, r io.Reader, size int64) error
	GetReader(key string) (io.ReadCloser, int64, error)
	DeletePrefix(prefix string) error
}

// InnerBlobStore is the exported alias of innerBlobStore, so wiring code in
// cmd/ can name the raw backend it selects.
type InnerBlobStore = innerBlobStore

// PutPrecompressed streams a body already encoded in the at-rest format.
func (c *CompressedBlobStore) PutPrecompressed(key string, body io.Reader, size int64) error {
	return c.Inner.Put(key, body, size)
}

// DeletePrefix deletes every object under prefix.
func (c *CompressedBlobStore) DeletePrefix(prefix string) error {
	return c.Inner.DeletePrefix(prefix)
}

// magic prefix for blobs written by this layer.
//
//   - bytes 0..1: 'H' 'Z'             (hostthis-zstd)
//   - byte 2:     0x00                 (reserved)
//   - byte 3:     0x01                 (format version 1)
//
// Cheap to inspect on every Get, and distinct enough that no real
// HTML/Markdown blob matches by accident.
var magicV1 = [4]byte{'H', 'Z', 0x00, 0x01}

// SpeedDefault (level 3): ratio close to the slower levels on HTML/text at a
// fraction of their cost.
const compressionLevel = zstd.SpeedDefault

// NewCompressedBlobStore wraps inner with the compression layer.
func NewCompressedBlobStore(inner innerBlobStore) *CompressedBlobStore {
	return &CompressedBlobStore{Inner: inner}
}

// CompressedBodyPrefixLen is the width of the at-rest framing prefix EncodeTo
// writes ahead of the zstd stream. Exported so a caller computing the
// quota-relevant payload size subtracts the real width rather than hardcoding
// it.
const CompressedBodyPrefixLen = len(magicV1)

// GetReader streams the UNCOMPRESSED bytes at key. The caller MUST Close it:
// Close releases both the zstd decoder and the inner reader. The int64 is the
// inner COMPRESSED length, so it must not be used as a Content-Length.
func (c *CompressedBlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	inner, size, err := c.Inner.GetReader(key)
	if err != nil {
		return nil, 0, err
	}
	dec, err := decodeCompressedStream(inner, key)
	if err != nil {
		return nil, 0, err
	}
	return dec, size, nil
}

// decodeCompressedStream wraps a stored blob stream with decompression and
// closes the underlying reader on every error path. label identifies failures.
func decodeCompressedStream(rc io.ReadCloser, label string) (io.ReadCloser, error) {
	// A blob shorter than the header is not an error: the short read fails the
	// magic check and is served through unwrapped.
	hdr := make([]byte, len(magicV1))
	n, rerr := io.ReadFull(rc, hdr)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		_ = rc.Close()
		return nil, fmt.Errorf("compressed blob read header %s: %w", label, rerr)
	}
	hdr = hdr[:n]
	if !hasMagicV1(hdr) {
		// Uncompressed: the peeked bytes are real content, so prepend them.
		return newPrefixReadCloser(hdr, rc), nil
	}
	// The magic is consumed; decode the rest with a pooled decoder.
	dec, err := getPooledDecoder(rc)
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("compressed blob %s: zstd reader: %w", label, err)
	}
	return &zstdReadCloser{dec: dec, inner: rc}, nil
}

// prefixReadCloser serves the peeked header bytes before continuing from the
// underlying reader, for uncompressed blobs where those bytes are content.
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

// zstdReadCloser couples a streaming zstd decoder to its inner reader so Close
// releases both.
type zstdReadCloser struct {
	dec   *zstd.Decoder
	inner io.ReadCloser
}

func (z *zstdReadCloser) Read(b []byte) (int, error) { return z.dec.Read(b) }

func (z *zstdReadCloser) Close() error {
	// Pooled rather than Closed, so the buffers stay warm. Safe even when the
	// caller stopped short of EOF (an aborted download): Reset readies the
	// decoder regardless.
	putPooledDecoder(z.dec)
	z.dec = nil
	return z.inner.Close()
}

func hasMagicV1(b []byte) bool { return bytes.HasPrefix(b, magicV1[:]) }

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// EncodeTo streams the at-rest representation of r into w, returning the
// quota-relevant payload size and the total length written.
func (c *CompressedBlobStore) EncodeTo(w io.Writer, r io.Reader) (int, int64, error) {
	counted := &countingWriter{w: w}
	if _, err := counted.Write(magicV1[:]); err != nil {
		return 0, 0, fmt.Errorf("compressed blob write magic: %w", err)
	}
	enc, err := zstd.NewWriter(counted, zstd.WithEncoderLevel(compressionLevel))
	if err != nil {
		return 0, 0, fmt.Errorf("compressed blob: zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		_ = enc.Close()
		return 0, 0, fmt.Errorf("compressed blob encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return 0, 0, fmt.Errorf("compressed blob close encoder: %w", err)
	}
	return int(counted.n) - CompressedBodyPrefixLen, counted.n, nil
}
