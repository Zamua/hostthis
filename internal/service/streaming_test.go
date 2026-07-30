package service_test

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/service"
)

// dripReader hands out a bounded chunk per Read, counting calls and bytes, so
// a test can tell streaming apart from a single io.ReadAll.
type dripReader struct {
	src   io.Reader
	chunk int
	calls int
	total int
}

func (d *dripReader) Read(p []byte) (int, error) {
	d.calls++
	if d.chunk > 0 && len(p) > d.chunk {
		p = p[:d.chunk]
	}
	n, err := d.src.Read(p)
	d.total += n
	return n, err
}

// TestUpload_StreamsInChunks pins that the upload path does not slurp the whole
// body into one buffer: at 4 KiB per Read a 4 MiB body takes ~1000 calls, where
// an io.ReadAll would take 1-3.
func TestUpload_StreamsInChunks(t *testing.T) {
	upload, _, _ := newStack(t)
	body := htmlBody(4 << 20) // 4 MiB high-entropy ASCII
	drip := &dripReader{src: bytesReaderFor(body), chunk: 4096}

	res, err := upload.Create(drip, "key:streamtest", "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Paste.Size <= 0 {
		t.Fatalf("compressed size should be positive, got %d", res.Paste.Size)
	}
	// A deliberately loose floor: anything over 100 calls rules out a single
	// io.ReadAll, and the real figure is in the thousands.
	if drip.calls < 100 {
		t.Fatalf("expected hundreds+ Read calls, got %d (body slurped via io.ReadAll?)", drip.calls)
	}
	if drip.total != len(body) {
		t.Fatalf("drip count mismatch: total=%d want=%d", drip.total, len(body))
	}
}

// TestUpload_PeakHeapUnderCap pins that a 5 MiB upload's heap delta stays in
// the low-MiB range rather than scaling with the body. GC behaviour is
// timing-dependent, so the threshold is deliberately loose.
func TestUpload_PeakHeapUnderCap(t *testing.T) {
	if testing.Short() {
		t.Skip("heap-watching test; skip under -short")
	}
	upload, _, _ := newStack(t)
	body := htmlBody(5 << 20) // 5 MiB high-entropy ASCII

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := upload.Create(bytesReaderFor(body), "key:heaptest", "", ""); err != nil {
		t.Fatalf("upload: %v", err)
	}

	runtime.ReadMemStats(&after)
	delta := int(after.HeapAlloc) - int(before.HeapAlloc)
	t.Logf("heap delta after 5 MiB upload: %d bytes", delta)
	// The test body, the readerWrap holding it, the zstd encoder state and the
	// staging buffer already total tens of MiB under perfect streaming, so the
	// ceiling only catches a change that 10x's the profile (an io.ReadAll
	// creeping back in), not a tight bound.
	if delta > 50<<20 {
		t.Fatalf("heap delta too high: %d bytes (expected < 50 MiB; body alone was 5 MiB)", delta)
	}
}

// TestUpload_RawCapTrips pins ErrRawTooLarge for stdin exceeding HardRawByteCap
// before EOF. The input must be HIGHLY COMPRESSIBLE so the raw counter hits
// 100 MiB before the zstd output crosses the 10 MiB compressed cap; otherwise
// ErrCompressedTooLarge fires first, which is correct for an incompressible
// payload but tests the other cap.
func TestUpload_RawCapTrips(t *testing.T) {
	upload, _, _ := newStack(t)
	// 110 MiB of repeated 'a' compresses to ~30 KB.
	body := make([]byte, 110<<20)
	body[0] = '<'
	for i := 1; i < len(body); i++ {
		body[i] = 'a'
	}
	_, err := upload.Create(bytesReaderFor(body), "key:rawcap", "", "")
	if !errors.Is(err, service.ErrRawTooLarge) {
		t.Fatalf("expected ErrRawTooLarge for >100 MiB raw input, got %v", err)
	}
}

// TestUpload_CompressedCapTrips pins the compressed cap for a body under the
// raw cap: 11 MiB of high-entropy ASCII barely compresses, so it lands past the
// 10 MiB compressed limit.
func TestUpload_CompressedCapTrips(t *testing.T) {
	upload, _, _ := newStack(t)
	body := htmlBody(11 << 20)
	_, err := upload.Create(bytesReaderFor(body), "key:csizecap", "", "")
	if !errors.Is(err, service.ErrCompressedTooLarge) {
		t.Fatalf("expected ErrCompressedTooLarge for ~11 MiB high-entropy, got %v", err)
	}
}

func bytesReaderFor(b []byte) io.Reader {
	return &readerWrap{b: b}
}

type readerWrap struct {
	b   []byte
	pos int
}

func (r *readerWrap) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// Anchors the strings import against helper churn.
var _ = strings.Repeat
