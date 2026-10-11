package http

import (
	"bytes"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/zstdenc"
)

// rawObjects is an in-memory object store holding stored bytes as given.
type rawObjects map[string][]byte

func (o rawObjects) Put(key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	o[key] = b
	return err
}

func (o rawObjects) GetReader(key string) (io.ReadCloser, int64, error) {
	b, ok := o[key]
	if !ok {
		return nil, 0, domain.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (o rawObjects) DeletePrefix(string) error { return nil }

// storedHTMLServer serves one ready HTML paste whose object holds stored, read
// through the production at-rest decoding.
func storedHTMLServer(stored []byte, size int) *Server {
	const key = "uploads/u1/0"
	return &Server{
		ApexDomain: "paste.test",
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindHTML, UpdatedAt: time.Now().UTC(),
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key, Size: size}),
		}},
		Blobs: storage.NewCompressedBlobStore(rawObjects{key: stored}),
	}
}

// An object holding the magic and no zstd frame is damaged: a 500, not an
// empty page.
func TestRead_MagicOnlyObjectIs500(t *testing.T) {
	srv := storedHTMLServer(zstdenc.Magic[:], 0)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/p/abc23456", nil))
	if w.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status %d body %q, want 500", w.Code, w.Body.String())
	}
}
