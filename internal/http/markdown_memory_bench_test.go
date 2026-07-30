package http

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/render"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// Constant-memory proof for the markdown serve path, which either streams the
// raw bytes (io.Copy, a fixed 32 KiB buffer) or writes a fixed const shell,
// both O(1) in the paste size. B/op staying ~flat across sizes IS that
// property.
//
// The body-discarding writer is load-bearing: a recorder would buffer the body
// and the numbers would measure it instead of the handler. render.Markdown
// (kept in internal/render for the dev CLI) is benched alongside as the
// O(size) contrast.

var mdBenchSizes = []int{1 << 10, 256 << 10, 1 << 20, 4 << 20} // 1 KiB, 256 KiB, 1 MiB, 4 MiB

func sizeLabel(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%dKiB", n>>10)
}

// mdBlobOfSize stores a markdown blob of exactly size bytes.
func mdBlobOfSize(b *testing.B, size int) (*storage.CompressedBlobStore, string, []byte) {
	b.Helper()
	disk, err := storage.NewBlobStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewBlobStore: %v", err)
	}
	c := storage.NewCompressedBlobStore(disk)
	unit := []byte("## Section\n\nThe quick brown fox jumps over the lazy dog. Some `code` and **bold**.\n\n")
	body := append([]byte("# Benchmark Doc\n\n"), bytes.Repeat(unit, size/len(unit)+1)...)
	body = body[:size]
	sha := shaOfBench(body)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		b.Fatalf("Put: %v", err)
	}
	return c, sha, body
}

func mdBenchServer(blobs BlobReader, sha string, updatedAt time.Time) (http.Handler, string) {
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindMarkdown,
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
	return srv.Handler(), "/p/abc23456"
}

// BenchmarkMarkdownServe_RawStream covers the raw branch (?raw -> io.Copy).
// B/op MUST stay ~flat across paste size.
func BenchmarkMarkdownServe_RawStream(b *testing.B) {
	for _, sz := range mdBenchSizes {
		b.Run(sizeLabel(sz), func(b *testing.B) {
			store, sha, _ := mdBlobOfSize(b, sz)
			h, path := mdBenchServer(service.NewStandaloneBlobUnit(store), sha, time.Now().UTC())
			r := httptest.NewRequest("GET", path+"?raw=1", nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					h.ServeHTTP(newDiscardRW(), r.Clone(r.Context()))
				}
			})
		})
	}
}

// BenchmarkMarkdownServe_Shell covers the browser branch (fixed const shell).
// B/op MUST stay ~flat AND small across paste size.
func BenchmarkMarkdownServe_Shell(b *testing.B) {
	for _, sz := range mdBenchSizes {
		b.Run(sizeLabel(sz), func(b *testing.B) {
			store, sha, _ := mdBlobOfSize(b, sz)
			h, path := mdBenchServer(service.NewStandaloneBlobUnit(store), sha, time.Now().UTC())
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Accept", "text/html")
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					h.ServeHTTP(newDiscardRW(), r.Clone(r.Context()))
				}
			})
		})
	}
}

// mdRenderBenchSizes caps the contrast bench at small sizes: render.Markdown
// allocates super-linearly, so a 4 MiB entry costs minutes and tens of GB while
// 1 KiB + 256 KiB already show the O(size) growth. The streaming benches above
// are cheap and stay on the full range.
var mdRenderBenchSizes = []int{1 << 10, 256 << 10}

// BenchmarkMarkdownRender_Old benches render.Markdown for contrast: B/op
// SCALES with paste size, unlike the serve path above.
func BenchmarkMarkdownRender_Old(b *testing.B) {
	for _, sz := range mdRenderBenchSizes {
		b.Run(sizeLabel(sz), func(b *testing.B) {
			_, _, body := mdBlobOfSize(b, sz)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := render.Markdown(body)
				if err != nil {
					b.Fatal(err)
				}
				_ = out
			}
		})
	}
}
