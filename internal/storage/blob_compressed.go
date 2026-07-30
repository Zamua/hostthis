package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdDecoderPool reuses streaming zstd decoders across blob reads. A fresh
// zstd.NewReader allocates a large working set (window / history buffers +
// decode scratch, ~10 MiB at the klauspost defaults) INDEPENDENT of the blob
// size, so paying it per GET would dominate the serve path's allocations.
// Pooling pays it once and reuses it via Decoder.Reset; the GC reaps the pool
// under memory pressure, so idle footprint stays bounded.
//
// New never returns nil: zstd.NewReader(nil) only errors on invalid options,
// and none are passed.
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

// CompressedBlobStore wraps another BlobStore and transparently zstd-encodes on
// Put, decodes on Get. The at-rest format carries a 4-byte magic prefix; a blob
// without it is returned as-is, so uncompressed blobs stay readable.
//
// Compression lives in the storage layer so callers above keep thinking in
// original uncompressed bytes, and dedup stays keyed on the sha256 of those
// original bytes.
type CompressedBlobStore struct {
	Inner innerBlobStore
}

// innerBlobStore is the minimal contract this wrapper depends on, declared
// here so the storage package need not import the service-layer interface.
type innerBlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
	GetReader(sha string) (io.ReadCloser, int64, error)
}

// InnerBlobStore is the exported alias of innerBlobStore, so wiring code in
// cmd/ can name the type the compression layer wraps. The write-back cache
// satisfies it and slots between this layer and the durable backend.
type InnerBlobStore = innerBlobStore

// PutPrecompressed writes a body that is ALREADY zstd-encoded with the magic
// prefix in place, for the service-layer streaming upload path that encodes
// incrementally as stdin arrives.
func (c *CompressedBlobStore) PutPrecompressed(sha string, body []byte) error {
	return c.Inner.Put(sha, bytes.NewReader(body), int64(len(body)))
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

// SpeedDefault (level 3): ratio close to the slower levels on HTML/text, at
// roughly 500 MB/s encode and 1 GB/s decode.
const compressionLevel = zstd.SpeedDefault

// NewCompressedBlobStore wraps inner with the compression layer.
func NewCompressedBlobStore(inner innerBlobStore) *CompressedBlobStore {
	return &CompressedBlobStore{Inner: inner}
}

// Put reads UNCOMPRESSED bytes from r and writes `magic + zstd(bytes)` to the
// inner store. The size argument is ignored: the inner store is handed the
// COMPRESSED length, which is what a backend wanting a Content-Length needs.
func (c *CompressedBlobStore) Put(sha string, r io.Reader, _ int64) error {
	body, err := EncodeCompressedBody(r)
	if err != nil {
		return err
	}
	return c.Inner.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// CompressedBodyPrefixLen is the width of the at-rest framing prefix that
// EncodeCompressedBody writes ahead of the zstd stream. Exported so a caller
// computing the quota-relevant payload size subtracts the real width instead
// of keeping its own copy of the magic bytes to measure.
const CompressedBodyPrefixLen = len(magicV1)

// EncodeCompressedBody returns `magic + zstd(r)` buffered in memory: the exact
// at-rest format Put writes and DecodeCompressedStream reads. The shale-blob
// stage path streams to BlobKV.StageBlob rather than through
// CompressedBlobStore.Put, and calls this so both paths store the identical
// format and a site file decodes the same as a paste. The body is bounded by
// one file (the untar's per-file cap), so buffering is safe.
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

// Get returns the UNCOMPRESSED bytes for sha. A blob without the magic header
// is returned as-is, so an uncompressed blob stays readable without a mass
// rewrite of the store.
func (c *CompressedBlobStore) Get(sha string) ([]byte, error) {
	body, err := c.Inner.Get(sha)
	if err != nil {
		return nil, err
	}
	if !hasMagicV1(body) {
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

// GetReader streams the UNCOMPRESSED bytes for sha without buffering the whole
// blob the way Get does, yielding byte-identical output. The caller MUST Close
// it: Close releases both the zstd decoder and the inner reader. The int64 is
// the inner COMPRESSED length, not the decoded length, so it must not be used
// as a Content-Length.
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

// DecodeCompressedStream wraps a raw stored blob stream in a reader yielding
// the DECOMPRESSED bytes, closing the underlying reader on Close (including on
// any error path). Shared by the standalone GetReader and the shale-blob read
// path so both decode identically. label names the blob in error messages.
func DecodeCompressedStream(rc io.ReadCloser, label string) (io.ReadCloser, error) {
	// A blob shorter than the header is not an error: io.ReadFull signals it
	// with ErrUnexpectedEOF / EOF, and the short read simply fails the magic
	// check and is served through unwrapped.
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

func hasMagicV1(b []byte) bool {
	return len(b) >= len(magicV1) &&
		b[0] == magicV1[0] && b[1] == magicV1[1] &&
		b[2] == magicV1[2] && b[3] == magicV1[3]
}

// estimatedCompressedSize gives Buffer a head start. Never below the magic
// header length, so the empty-input case still allocates.
func estimatedCompressedSize(uncompressed int) int {
	if uncompressed <= 0 {
		return len(magicV1) + 64
	}
	return len(magicV1) + uncompressed/2 // optimistic; Buffer grows if needed
}

// EncodeBody implements the service-side BlobStore encoder, returning the
// at-rest body and the payload size excluding the framing prefix. The service
// asks rather than computing, so the prefix width lives only here.
func (s *CompressedBlobStore) EncodeBody(r io.Reader) ([]byte, int, error) {
	body, err := EncodeCompressedBody(r)
	if err != nil {
		return nil, 0, err
	}
	return body, len(body) - CompressedBodyPrefixLen, nil
}
