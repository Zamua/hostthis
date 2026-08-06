package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// discardResponseWriter throws the body away. httptest.ResponseRecorder's own
// body accumulation would dominate the allocation numbers and mask the
// buffered-vs-streamed difference the benchmarks measure.
type discardResponseWriter struct {
	h http.Header
}

func newDiscardRW() *discardResponseWriter { return &discardResponseWriter{h: http.Header{}} }

func (d *discardResponseWriter) Header() http.Header         { return d.h }
func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)             {}

// bufferingBlobReader buffers the whole decompressed blob per Read: the
// full-payload-per-GET baseline the streaming serve path is measured against.
type bufferingBlobReader struct{ inner BlobReader }

func (b bufferingBlobReader) ReadAll(ctx context.Context, slug, sha string) ([]byte, error) {
	return b.inner.ReadAll(ctx, slug, sha)
}
func (b bufferingBlobReader) Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error) {
	body, err := b.inner.ReadAll(ctx, slug, sha)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func shaOfBench(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func benchServer(b *testing.B, blobs BlobReader, sha string, updatedAt time.Time) (*Server, *http.Request) {
	b.Helper()
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: sha,
		UpdatedAt:  updatedAt,
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

// runConcurrentGETs stands in for N concurrent clients on one large paste.
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

// BenchmarkServePaste_StreamedGetReader measures the serve path io.Copy'ing
// straight from the streaming zstd decoder. Compare allocs/op + B/op against
// the buffered baseline below.
func BenchmarkServePaste_StreamedGetReader(b *testing.B) {
	store, sha := newBenchBlobStore(b)
	srv, r := benchServer(b, service.NewStandaloneBlobUnit(store), sha, time.Now().UTC())
	b.ReportAllocs()
	runConcurrentGETs(b, srv, r)
}

// BenchmarkServePaste_BufferedGet is the baseline: the whole decompressed blob
// allocated per GET. The B/op delta against the streamed bench is the
// full-payload spike streaming removes, and it scales with concurrency.
func BenchmarkServePaste_BufferedGet(b *testing.B) {
	store, sha := newBenchBlobStore(b)
	srv, r := benchServer(b, bufferingBlobReader{inner: service.NewStandaloneBlobUnit(store)}, sha, time.Now().UTC())
	b.ReportAllocs()
	runConcurrentGETs(b, srv, r)
}

var (
	_ http.ResponseWriter = (*discardResponseWriter)(nil)
	_ BlobReader          = bufferingBlobReader{}
)
