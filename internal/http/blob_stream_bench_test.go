package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// discardResponseWriter is an http.ResponseWriter that throws the body
// away. Using it instead of httptest.ResponseRecorder keeps the
// benchmark measuring the SERVE PATH's allocation behavior (buffered
// Get vs streamed GetReader) rather than the recorder's own
// body-accumulation allocations, which would otherwise dominate and
// mask the difference.
type discardResponseWriter struct {
	h http.Header
}

func newDiscardRW() *discardResponseWriter { return &discardResponseWriter{h: http.Header{}} }

func (d *discardResponseWriter) Header() http.Header         { return d.h }
func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)             {}

// bufferingBlobReader forces the OLD serve behavior: GetReader buffers
// the whole decompressed blob into memory (via Get) and hands back an
// in-memory reader. This is the "before" baseline for the benchmark -
// it reproduces the full-payload per-GET allocation that FIX #1
// removes. The "after" path uses the real *storage.CompressedBlobStore,
// whose GetReader streams.
type bufferingBlobReader struct{ inner BlobReader }

func (b bufferingBlobReader) Get(sha string) ([]byte, error) { return b.inner.Get(sha) }
func (b bufferingBlobReader) GetReader(sha string) (io.ReadCloser, int64, error) {
	body, err := b.inner.Get(sha)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func shaOfBench(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// benchServer wires a Server over a real compressed-on-disk blob store
// holding one large HTML paste, returning the server and the request to
// drive. The blob is realistic (~4 MiB of compressible HTML).
func benchServer(b *testing.B, blobs BlobReader, sha string, updatedAt time.Time) (*Server, *http.Request) {
	b.Helper()
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: sha,
		UpdatedAt:  updatedAt,
		ExpiresAt:  updatedAt.Add(7 * 24 * time.Hour),
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      blobs,
		ApexDomain: "paste.test",
		Now:        func() time.Time { return updatedAt.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	return srv, r
}

func newBenchBlobStore(b *testing.B) (*storage.CompressedBlobStore, string) {
	b.Helper()
	disk, err := storage.NewBlobStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewBlobStore: %v", err)
	}
	c := storage.NewCompressedBlobStore(disk)
	// ~4 MiB of compressible HTML, representative of a large paste.
	body := bytes.Repeat([]byte("<p>the quick brown fox jumps over the lazy dog</p>\n"), 85000)
	sha := shaOfBench(body)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		b.Fatalf("Put: %v", err)
	}
	return c, sha
}

// runConcurrentGETs drives the server's handler from b.RunParallel,
// simulating N concurrent clients hitting the same large paste.
func runConcurrentGETs(b *testing.B, srv *Server, r *http.Request) {
	b.Helper()
	h := srv.Handler()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.ServeHTTP(newDiscardRW(), r.Clone(r.Context()))
		}
	})
}

// BenchmarkServePaste_StreamedGetReader is the AFTER path: the HTML
// serve path io.Copy's straight from the streaming zstd decoder. Report
// allocs/op + B/op; compare against the buffered baseline below.
func BenchmarkServePaste_StreamedGetReader(b *testing.B) {
	store, sha := newBenchBlobStore(b)
	srv, r := benchServer(b, store, sha, time.Now().UTC())
	b.ReportAllocs()
	runConcurrentGETs(b, srv, r)
}

// BenchmarkServePaste_BufferedGet is the BEFORE baseline: the serve
// path buffers the whole decompressed blob per GET (the pre-FIX-#1
// behavior). The delta in B/op vs the streamed bench is the per-GET
// full-payload spike that scaled with concurrency on the small VPS.
func BenchmarkServePaste_BufferedGet(b *testing.B) {
	store, sha := newBenchBlobStore(b)
	srv, r := benchServer(b, bufferingBlobReader{inner: store}, sha, time.Now().UTC())
	b.ReportAllocs()
	runConcurrentGETs(b, srv, r)
}

// compile-time assertions that the bench writer + reader satisfy the
// interfaces they stand in for.
var (
	_ http.ResponseWriter = (*discardResponseWriter)(nil)
	_ BlobReader          = bufferingBlobReader{}
)
