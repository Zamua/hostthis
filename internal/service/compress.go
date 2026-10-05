package service

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/zstdenc"
)

// stagedUpload is the result of streaming bytes through the upload pipeline.
// CompressedSize excludes the 4-byte magic.
type stagedUpload struct {
	// File holds the at-rest bytes (magic + zstd), ready for StagePrecompressed.
	// Spilled to disk so peak memory does not track the payload. Positioned at
	// 0 and owned by the caller, which MUST close and remove it.
	File           *os.File
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

// streamUpload tees r in one pass through the at-rest encoder spilling to a
// temp file capped at MaxPasteBytes plus the magic, a raw-byte counter that
// aborts at HardRawByteCap, and a sniff-prefix capture. The source is never
// materialized; peak memory is a chunk buffer plus the compressor window.
//
// Returns errRawCapExceeded, errCompressedCapExceeded, or any other error
// verbatim. On error the spill file is already removed.
func streamUpload(r io.Reader) (stagedUpload, error) {
	f, ferr := os.CreateTemp(os.Getenv("HOSTTHIS_STAGING_DIR"), "hostthis-paste-*")
	if ferr != nil {
		return stagedUpload{}, fmt.Errorf("staging temp: %w", ferr)
	}
	fail := func(err error) (stagedUpload, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return stagedUpload{}, err
	}
	// Every encoded byte, the final block Close emits included, passes the cap.
	capped := &cappedWriter{inner: f, limit: domain.MaxPasteBytes + len(zstdenc.Magic)}
	zw, err := zstdenc.NewWriter(capped)
	if err != nil {
		return fail(err)
	}
	rawCount := &rawCountWriter{limit: domain.HardRawByteCap}
	prefix := &prefixBuffer{cap: domain.SniffPrefixLen}

	if _, err := io.Copy(io.MultiWriter(zw, rawCount, prefix), r); err != nil {
		_ = zw.Close()
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(fmt.Errorf("rewind staged paste: %w", err))
	}

	return stagedUpload{
		File:           f,
		RawSize:        rawCount.n,
		CompressedSize: capped.written - len(zstdenc.Magic),
		Prefix:         prefix.bytes(),
	}, nil
}

// documentManifest is a single document's one-entry manifest, naming the
// object its staged bytes are written to.
func documentManifest(key string, kind domain.ContentKind, s stagedUpload) domain.Manifest {
	return domain.DocumentManifest(domain.ManifestEntry{
		Key:            key,
		Size:           s.RawSize,
		CompressedSize: s.CompressedSize,
		Kind:           string(kind),
	})
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
		w.n = w.limit
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

// encodedSize is the exact at-rest length, magic included - what a size-aware
// stage needs.
func (s stagedUpload) encodedSize() int64 { return int64(s.CompressedSize + len(zstdenc.Magic)) }

// discard closes and removes a staged upload's spill file. Safe on a zero value.
func (s stagedUpload) discard() {
	if s.File == nil {
		return
	}
	_ = s.File.Close()
	_ = os.Remove(s.File.Name())
}
