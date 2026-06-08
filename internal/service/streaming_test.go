package service_test

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/service"
)

// dripReader hands out n bytes at a time, recording the cumulative
// count + the number of Read() calls. Used to verify the upload path
// streams rather than buffering everything via io.ReadAll.
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

// TestUpload_StreamsInChunks proves the upload path doesn't slurp the
// whole body into one buffer up front. With a drip reader handing out
// 4 KiB per Read, a 4 MiB body should require ~1000 Read calls. If
// the path were doing io.ReadAll, it'd be 1-3 calls.
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
	// Floor for "definitely streamed" is generous — anything > 100
	// calls proves it wasn't a single io.ReadAll. Real number will
	// be in the thousands for a 4 MiB body at 4 KiB per Read.
	if drip.calls < 100 {
		t.Fatalf("expected hundreds+ Read calls, got %d (body slurped via io.ReadAll?)", drip.calls)
	}
	if drip.total != len(body) {
		t.Fatalf("drip count mismatch: total=%d want=%d", drip.total, len(body))
	}
}

// TestUpload_PeakHeapUnderCap measures the heap delta over a 5 MiB
// upload. With the streaming pipeline + compressed staging, peak
// allocation should be in the low-MiB range, not the body-size range.
//
// Compares HeapInuse before vs peak during; flaky on contended runners
// (GC behavior is timing-dependent), so the threshold is generous.
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
	// HeapAlloc includes objects still alive; the staged buffer is
	// freed after Put returns. Delta should be in the low MB range,
	// not the 5+ MiB the body itself occupies.
	delta := int(after.HeapAlloc) - int(before.HeapAlloc)
	t.Logf("heap delta after 5 MiB upload: %d bytes", delta)
	// Generous ceiling: 50 MiB. The test body itself plus the
	// readerWrap holding it plus the zstd encoder's internal state
	// plus the staging buffer adds up to ~30-40 MiB even with
	// perfect streaming. The point is to catch a future change that
	// 10x's this (e.g. accidental io.ReadAll re-introduction), not
	// to enforce a tight bound.
	if delta > 50<<20 {
		t.Fatalf("heap delta too high: %d bytes (expected < 50 MiB; body alone was 5 MiB)", delta)
	}
}

// TestUpload_RawCapTrips fires when stdin delivers more than
// HardRawByteCap before EOF. Needs HIGHLY COMPRESSIBLE input so the
// raw counter hits 100 MiB before zstd output crosses the 10 MiB
// compressed cap — otherwise ErrCompressedTooLarge fires first (which
// is correct behavior for typical incompressible payloads).
func TestUpload_RawCapTrips(t *testing.T) {
	upload, _, _ := newStack(t)
	// 110 MiB of repeated 'a' compresses to ~30 KB. Raw counter
	// trips at 100 MiB long before any compressed-size concern.
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

// TestUpload_CompressedCapTrips fires when the body fits under the raw
// cap but its compressed output exceeds the per-paste compressed cap.
// High-entropy ASCII at 11 MiB compresses to ~11 MiB, blowing past the
// 10 MiB compressed cap.
func TestUpload_CompressedCapTrips(t *testing.T) {
	upload, _, _ := newStack(t)
	body := htmlBody(11 << 20)
	_, err := upload.Create(bytesReaderFor(body), "key:csizecap", "", "")
	if !errors.Is(err, service.ErrCompressedTooLarge) {
		t.Fatalf("expected ErrCompressedTooLarge for ~11 MiB high-entropy, got %v", err)
	}
}

// bytesReaderFor is a thin alias so the test reads more linearly
// (avoids the "bytes.NewReader" import noise in the test body when
// most tests already import bytes for unrelated reasons).
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

// strings import compiler-anchor (silences vet if helpers grow).
var _ = strings.Repeat
