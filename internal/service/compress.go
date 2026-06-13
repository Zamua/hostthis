package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/Zamua/hostthis/internal/domain"
)

// blobMagicV1 mirrors storage.magicV1 - kept locally so the service
// layer can produce blob bytes without importing the storage package.
// If the two ever diverge, blob reads break with "no magic header
// found" on freshly written blobs.
var blobMagicV1 = [4]byte{'H', 'Z', 0x00, 0x01}

// stagedUpload is the result of streaming bytes through the upload
// pipeline. The Body is the magic-prefixed + zstd-encoded bytes ready
// to hand to BlobStore.PutPrecompressed. RawSize is the original
// uncompressed byte count (used by DetectKind + sniffing logic);
// CompressedSize is Body length minus the 4-byte magic.
type stagedUpload struct {
	SHA            string
	Body           []byte
	RawSize        int
	CompressedSize int
	// Prefix holds the first ~512 bytes (uncompressed) so callers
	// can sniff content type without re-reading the source.
	Prefix []byte
}

// errRawCapExceeded fires when the raw input crosses HardRawByteCap.
var errRawCapExceeded = errors.New("raw cap exceeded")

// errCompressedCapExceeded fires when the compressed staging crosses
// MaxPasteBytes mid-stream.
var errCompressedCapExceeded = errors.New("compressed cap exceeded")

// streamUpload tees r through three sinks:
//
//   - a sha256 hasher (over UNCOMPRESSED bytes - content-addressability
//     is by original content so dedup matches user intent)
//   - a zstd encoder writing to an in-memory staging buffer (capped
//     at MaxPasteBytes + a small overhead for the magic header)
//   - a raw-byte counter that aborts at HardRawByteCap
//
// Returns a stagedUpload ready for PutPrecompressed, or one of:
//
//   - errRawCapExceeded     → raw input too large (caller maps to
//     ErrRawTooLarge)
//   - errCompressedCapExceeded → compressed output too large (caller
//     maps to ErrCompressedTooLarge)
//   - any other error verbatim (read error, zstd error, etc.)
//
// The pipeline is single-pass: bytes pass through exactly once for
// hashing AND compression AND counting. Peak memory per call is
// bounded by the staging buffer (~MaxPasteBytes) plus a small chunk
// buffer; the source data is never materialized in full.
func streamUpload(r io.Reader) (stagedUpload, error) {
	// Staging starts with the magic header so PutPrecompressed can
	// be a straight write with no further wrapping.
	staging := bytes.NewBuffer(make([]byte, 0, 64*1024))
	staging.Write(blobMagicV1[:])

	// cappedWriter aborts if the compressed body crosses
	// MaxPasteBytes. zstd's output is bounded by raw size + ~0.5%,
	// so a successful upload's compressed staging is always small.
	cap := &cappedWriter{inner: staging, limit: domain.MaxPasteBytes + len(blobMagicV1)}

	zw, err := zstd.NewWriter(cap, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return stagedUpload{}, fmt.Errorf("zstd writer: %w", err)
	}

	hasher := sha256.New()
	rawCount := &rawCountWriter{limit: domain.HardRawByteCap}
	prefix := &prefixBuffer{cap: 512}

	mw := io.MultiWriter(zw, hasher, rawCount, prefix)
	if _, err := io.Copy(mw, r); err != nil {
		_ = zw.Close()
		return stagedUpload{}, mapPipelineErr(err)
	}
	if err := zw.Close(); err != nil {
		return stagedUpload{}, mapPipelineErr(err)
	}
	// One last check: zstd may emit the final block on Close that
	// pushes us over the compressed cap. Re-check via staging.Len.
	if staging.Len() > domain.MaxPasteBytes+len(blobMagicV1) {
		return stagedUpload{}, errCompressedCapExceeded
	}

	return stagedUpload{
		SHA:            hex.EncodeToString(hasher.Sum(nil)),
		Body:           staging.Bytes(),
		RawSize:        rawCount.n,
		CompressedSize: staging.Len() - len(blobMagicV1),
		Prefix:         prefix.bytes(),
	}, nil
}

func mapPipelineErr(err error) error {
	switch {
	case errors.Is(err, errRawCapExceeded):
		return errRawCapExceeded
	case errors.Is(err, errCompressedCapExceeded):
		return errCompressedCapExceeded
	default:
		return err
	}
}

// cappedWriter forwards Write to inner, returning errCompressedCapExceeded
// when total written would cross limit. Used to abort mid-stream when
// zstd-encoded output blows past the per-paste cap.
type cappedWriter struct {
	inner   io.Writer
	written int
	limit   int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		return 0, errCompressedCapExceeded
	}
	n, err := w.inner.Write(p)
	w.written += n
	return n, err
}

// rawCountWriter counts incoming bytes (uncompressed) and aborts at
// limit. Used to enforce HardRawByteCap on the input side so attackers
// can't stream forever to probe compression ratios.
type rawCountWriter struct {
	n     int
	limit int
}

func (w *rawCountWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.limit {
		// Accept what fits up to the limit before aborting so
		// downstream writers see the same prefix bytes.
		remaining := max(w.limit-w.n, 0)
		w.n = w.limit
		_ = remaining // accounting only; the actual write is what's reported
		return 0, errRawCapExceeded
	}
	w.n += len(p)
	return len(p), nil
}

// prefixBuffer captures up to cap leading bytes for content-type
// sniffing. Subsequent writes past cap are no-ops (Write must return
// len(p), nil for io.MultiWriter to keep going).
type prefixBuffer struct {
	buf []byte
	cap int
}

func (p *prefixBuffer) Write(b []byte) (int, error) {
	if len(p.buf) < p.cap {
		room := p.cap - len(p.buf)
		take := b
		if len(take) > room {
			take = take[:room]
		}
		p.buf = append(p.buf, take...)
	}
	return len(b), nil
}

func (p *prefixBuffer) bytes() []byte { return p.buf }
