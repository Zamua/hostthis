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

// blobMagicV1 and blobCompressionLevel mirror storage.magicV1 and
// storage.compressionLevel, kept local because the service layer must not
// import the storage package. Together they are a second implementation of the
// at-rest blob format: a diverged magic makes fresh blobs read back as
// uncompressed, and a diverged level changes the stored bytes the quota is
// charged for. TestStreamUploadMatchesStorageAtRestFormat pins the two encoders
// byte-identical.
var blobMagicV1 = [4]byte{'H', 'Z', 0x00, 0x01}

const blobCompressionLevel = zstd.SpeedDefault

// stagedUpload is the result of streaming bytes through the upload pipeline.
// Body is magic-prefixed + zstd-encoded, ready for
// BlobStore.PutPrecompressed; CompressedSize excludes the 4-byte magic.
type stagedUpload struct {
	SHA            string
	Body           []byte
	RawSize        int
	CompressedSize int
	// Prefix holds the leading uncompressed bytes so callers can classify the
	// content without re-reading the source. Sized by domain.SniffPrefixLen,
	// which is what the format heuristics need rather than what the MIME
	// sniff needs.
	Prefix []byte
}

// errRawCapExceeded fires when the raw input crosses HardRawByteCap.
var errRawCapExceeded = errors.New("raw cap exceeded")

// errCompressedCapExceeded fires when the compressed staging crosses
// MaxPasteBytes mid-stream.
var errCompressedCapExceeded = errors.New("compressed cap exceeded")

// streamUpload tees r through three sinks:
//
//   - a sha256 hasher over UNCOMPRESSED bytes, so dedup is by original
//     content and matches user intent
//   - a zstd encoder writing to an in-memory staging buffer, capped at
//     MaxPasteBytes plus the magic header
//   - a raw-byte counter that aborts at HardRawByteCap
//
// Returns errRawCapExceeded, errCompressedCapExceeded (both mapped by the
// caller), or any other error verbatim.
//
// Single-pass: bytes are hashed AND compressed AND counted in one traversal,
// so peak memory is the staging buffer plus a chunk buffer and the source is
// never materialized in full.
func streamUpload(r io.Reader) (stagedUpload, error) {
	// Staging starts with the magic header so PutPrecompressed is a straight
	// write with no further wrapping.
	staging := bytes.NewBuffer(make([]byte, 0, 64*1024))
	staging.Write(blobMagicV1[:])

	cap := &cappedWriter{inner: staging, limit: domain.MaxPasteBytes + len(blobMagicV1)}

	zw, err := zstd.NewWriter(cap, zstd.WithEncoderLevel(blobCompressionLevel))
	if err != nil {
		return stagedUpload{}, fmt.Errorf("zstd writer: %w", err)
	}

	hasher := sha256.New()
	rawCount := &rawCountWriter{limit: domain.HardRawByteCap}
	prefix := &prefixBuffer{cap: domain.SniffPrefixLen}

	mw := io.MultiWriter(zw, hasher, rawCount, prefix)
	if _, err := io.Copy(mw, r); err != nil {
		_ = zw.Close()
		return stagedUpload{}, mapPipelineErr(err)
	}
	if err := zw.Close(); err != nil {
		return stagedUpload{}, mapPipelineErr(err)
	}
	// zstd can emit a final block on Close that crosses the compressed cap,
	// past the cappedWriter's per-Write check.
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
// when the total written would cross limit, so a zstd-encoded body past the
// per-paste cap aborts mid-stream.
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

// rawCountWriter counts uncompressed incoming bytes and aborts at limit,
// enforcing HardRawByteCap on the input side so an attacker cannot stream
// forever to probe compression ratios.
type rawCountWriter struct {
	n     int
	limit int
}

func (w *rawCountWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.limit {
		remaining := max(w.limit-w.n, 0)
		w.n = w.limit
		_ = remaining // accounting only; the actual write is what's reported
		return 0, errRawCapExceeded
	}
	w.n += len(p)
	return len(p), nil
}

// prefixBuffer captures up to cap leading bytes for content-type sniffing.
// Writes past cap are no-ops that still report len(p), nil, which
// io.MultiWriter requires to keep going.
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
