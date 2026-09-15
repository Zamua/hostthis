package http

// Reads resolve a file by its object key and validate on that key; an entry
// without one is not found (docs/SPEC.md "Entries without an object key").

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// recordingBlobs serves bytes by object key and records every read.
type recordingBlobs struct {
	bodies map[string]string
	reads  []domain.ManifestEntry
}

func (b *recordingBlobs) Read(_ context.Context, e domain.ManifestEntry) (io.ReadCloser, int64, error) {
	b.reads = append(b.reads, e)
	body, ok := b.bodies[e.Key]
	if !ok {
		return nil, 0, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader([]byte(body))), int64(len(body)), nil
}

func serveOne(srv *Server, target, ifNoneMatch string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func TestServePaste_KeyedDocumentReadsAndValidatesOnItsKey(t *testing.T) {
	const key = "uploads/u1/0"
	blobs := &recordingBlobs{bodies: map[string]string{key: "<h1>keyed</h1>"}}
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindHTML, UpdatedAt: time.Now().UTC(),
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key}),
		}},
		Blobs: blobs,
	}

	w := serveOne(srv, "/p/abc23456", "")
	if w.Code != 200 || w.Body.String() != "<h1>keyed</h1>" {
		t.Fatalf("GET = %d %q, want the keyed object", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"`+key+`"` {
		t.Fatalf("ETag = %q, want the object key", got)
	}

	reads := len(blobs.reads)
	if w := serveOne(srv, "/p/abc23456", `"`+key+`"`); w.Code != 304 {
		t.Fatalf("conditional GET on the key = %d, want 304", w.Code)
	}
	if len(blobs.reads) != reads {
		t.Fatal("a 304 read the object")
	}
}

func TestServePaste_RawMarkdownReadsItsKey(t *testing.T) {
	const key = "uploads/u2/0"
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindMarkdown, UpdatedAt: time.Now().UTC(),
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key}),
		}},
		Blobs: &recordingBlobs{bodies: map[string]string{key: "# keyed"}},
	}
	w := serveOne(srv, "/p/abc23456?raw=1", "")
	if w.Code != 200 || w.Body.String() != "# keyed" || w.Header().Get("ETag") != `"`+key+`"` {
		t.Fatalf("raw GET = %d %q etag %q, want the keyed object validated on its key", w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
}

// A root without an object key names no bytes: every read of it is a 404 that
// never reaches the byte plane, even while an object under its old sha exists.
func TestServePaste_KeylessRootIsNotFound(t *testing.T) {
	for name, p := range map[string]domain.Paste{
		"row without a manifest": {Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindHTML},
		"root without a key": {Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindMarkdown,
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Size: 15})},
	} {
		for _, req := range []struct{ target, ifNoneMatch string }{
			{"/p/abc23456", ""}, {"/p/abc23456?raw=1", ""}, {"/p/abc23456", `"deadbeef"`},
		} {
			p.UpdatedAt = time.Now().UTC()
			blobs := &recordingBlobs{bodies: map[string]string{"deadbeef": "<h1>legacy</h1>", "": "<h1>legacy</h1>"}}
			w := serveOne(&Server{Pastes: stubPasteReader{p: p}, Blobs: blobs}, req.target, req.ifNoneMatch)
			if w.Code != 404 || len(blobs.reads) != 0 {
				t.Fatalf("%s: GET %s (If-None-Match %q) = %d with %d reads, want 404 and none",
					name, req.target, req.ifNoneMatch, w.Code, len(blobs.reads))
			}
		}
	}
}

// A site file without an object key is a 404 that never reaches the byte
// plane, while its keyed siblings still serve.
func TestServeSite_KeylessEntryIsNotFound(t *testing.T) {
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{Key: "uploads/u3/0", ContentType: "text/html; charset=utf-8"})
	m.Add("old.css", domain.ManifestEntry{Size: 6, ContentType: "text/css; charset=utf-8"})
	blobs := &recordingBlobs{bodies: map[string]string{"uploads/u3/0": "<h1>site</h1>", "": "body{}"}}
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindSite, Manifest: m, UpdatedAt: time.Now().UTC(),
		}},
		Blobs: blobs,
	}
	if w := serveOne(srv, "/p/abc23456/old.css", ""); w.Code != 404 || len(blobs.reads) != 0 {
		t.Fatalf("GET keyless file = %d with %d reads, want 404 and none", w.Code, len(blobs.reads))
	}
	w := serveOne(srv, "/p/abc23456/", "")
	if w.Code != 200 || w.Body.String() != "<h1>site</h1>" || w.Header().Get("ETag") != `"uploads/u3/0"` {
		t.Fatalf("GET keyed index = %d %q etag %q, want the keyed object validated on its key",
			w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
}
