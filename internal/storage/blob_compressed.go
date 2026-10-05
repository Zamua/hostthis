package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/zstdenc"
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

// CompressedBlobStore is the service byte plane over a raw object store: it
// owns the at-rest encoding and the per-upload object namespace.
type CompressedBlobStore struct {
	Inner InnerBlobStore
}

// InnerBlobStore is the raw object store under the at-rest encoding. Known
// lengths let celld send a Content-Length instead of a chunked body.
type InnerBlobStore interface {
	Put(key string, r io.Reader, size int64) error
	GetReader(key string) (io.ReadCloser, int64, error)
	DeletePrefix(prefix string) error
}

func NewCompressedBlobStore(inner InnerBlobStore) *CompressedBlobStore {
	return &CompressedBlobStore{Inner: inner}
}

// StagePrecompressed streams a body already in the at-rest format.
func (c *CompressedBlobStore) StagePrecompressed(_ context.Context, key string, r io.Reader, size int64) error {
	return c.Inner.Put(key, r, size)
}

// StageEncoding spills the encoded body to disk, so the store receives a known
// length without the payload being held in memory.
func (c *CompressedBlobStore) StageEncoding(ctx context.Context, key string, r io.Reader) (int, error) {
	spill, err := os.CreateTemp("", "hostthis-blob-*")
	if err != nil {
		return 0, fmt.Errorf("create blob spill: %w", err)
	}
	defer os.Remove(spill.Name()) //nolint:errcheck
	defer spill.Close()           //nolint:errcheck

	enc, err := zstdenc.NewWriter(spill)
	if err != nil {
		return 0, fmt.Errorf("compressed blob write magic: %w", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		_ = enc.Close()
		return 0, fmt.Errorf("compressed blob encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return 0, fmt.Errorf("compressed blob close encoder: %w", err)
	}
	total, err := spill.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = spill.Seek(0, io.SeekStart)
	}
	if err != nil {
		return 0, fmt.Errorf("rewind blob spill: %w", err)
	}
	if err := c.StagePrecompressed(ctx, key, spill, total); err != nil {
		return 0, err
	}
	return int(total) - len(zstdenc.Magic), nil
}

// Read streams the UNCOMPRESSED bytes at an entry's key. The caller MUST Close
// it: Close releases both the zstd decoder and the inner reader. The int64 is
// the inner COMPRESSED length, so it must not be used as a Content-Length. An
// entry without a key never reaches the store (docs/SPEC.md "Entries without
// an object key").
func (c *CompressedBlobStore) Read(_ context.Context, entry domain.ManifestEntry) (io.ReadCloser, int64, error) {
	if entry.Key == "" {
		return nil, 0, domain.ErrNotFound
	}
	inner, size, err := c.Inner.GetReader(entry.Key)
	if err != nil {
		return nil, 0, err
	}
	dec, err := decodeCompressedStream(inner, entry.Key)
	if err != nil {
		return nil, 0, err
	}
	return dec, size, nil
}

// DeleteUpload deletes every object under the upload's prefix.
func (c *CompressedBlobStore) DeleteUpload(_ context.Context, uploadID string) error {
	if !domain.ValidUploadID(uploadID) {
		return fmt.Errorf("blob: invalid upload id %q", uploadID)
	}
	return c.Inner.DeletePrefix(domain.UploadPrefix(uploadID))
}

// errUnframed marks a stored object that lacks the at-rest magic. Every write
// frames its object, so such an object is damaged.
var errUnframed = errors.New("object lacks the at-rest magic")

// decodeCompressedStream wraps a stored blob stream with decompression and
// closes the underlying reader on every error path. label identifies failures.
func decodeCompressedStream(rc io.ReadCloser, label string) (io.ReadCloser, error) {
	var hdr [len(zstdenc.Magic)]byte
	_, err := io.ReadFull(rc, hdr[:])
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF || (err == nil && hdr != zstdenc.Magic):
		err = fmt.Errorf("compressed blob %s: %w", label, errUnframed)
	case err != nil:
		err = fmt.Errorf("compressed blob read header %s: %w", label, err)
	}
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	dec, err := getPooledDecoder(rc)
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("compressed blob %s: zstd reader: %w", label, err)
	}
	return &zstdReadCloser{dec: dec, inner: rc}, nil
}

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
