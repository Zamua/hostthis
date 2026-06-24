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

// Constant-memory proof for #458. The markdown serve path used to
// ReadAll the whole document and render.Markdown it (allocating O(size)
// per GET). It now either streams the raw bytes (io.Copy, a fixed 32 KiB
// buffer) or writes a fixed const shell - both O(1) in the paste size.
//
// These benchmarks serve markdown at 1 KiB .. 4 MiB through the real
// handler with a body-discarding writer (so we measure the HANDLER's
// allocations, not a recorder buffering the body). B/op staying ~flat
// across sizes IS the constant-memory property; the old render path
// (kept in internal/render for the dev CLI) is benched alongside to
// show the O(size) growth this change removed from the serve path.

var mdBenchSizes = []int{1 << 10, 256 << 10, 1 << 20, 4 << 20} // 1 KiB, 256 KiB, 1 MiB, 4 MiB

func sizeLabel(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%dKiB", n>>10)
}

// mdBlobOfSize stores a realistic markdown blob of exactly size bytes and
// returns the streaming store, its sha, and the raw bytes.
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

// BenchmarkMarkdownServe_RawStream: the NEW raw branch (?raw -> io.Copy).
// B/op MUST be ~flat across paste size = constant memory.
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

// BenchmarkMarkdownServe_Shell: the NEW browser branch (fixed const
// shell). B/op MUST be ~flat AND small across paste size.
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

// mdRenderBenchSizes caps the OLD-render contrast bench at small sizes:
// render.Markdown allocates super-linearly (a 4 MiB doc allocated ~60 GB
// and ran ~99 s in one run), so we only need 1 KiB + 256 KiB to show the
// O(size) growth without a multi-minute, tens-of-GB bench. The streaming
// shell/raw benches above stay on the full range (they are cheap there).
var mdRenderBenchSizes = []int{1 << 10, 256 << 10}

// BenchmarkMarkdownRender_Old: the REMOVED server-side render path, for
// contrast. B/op SCALES with paste size - the per-GET allocation #458
// took off the serve path.
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
