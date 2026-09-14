package http

// Reads resolve a file by its object key, or a legacy file by its sha, and
// validate on whichever address they read (docs/SPEC.md "Legacy
// content-addressed entries").

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

// recordingBlobs serves bytes by entry address and records every read.
type recordingBlobs struct {
	bodies map[string]string
	reads  []domain.ManifestEntry
}

func (b *recordingBlobs) Read(_ context.Context, e domain.ManifestEntry) (io.ReadCloser, int64, error) {
	b.reads = append(b.reads, e)
	body, ok := b.bodies[e.Address()]
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
	blobs := &recordingBlobs{bodies: map[string]string{key: "<h1>keyed</h1>", "stale": "<h1>wrong</h1>"}}
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindHTML, ContentSHA: "stale", UpdatedAt: time.Now().UTC(),
			// A re-homed version keeps its old flat sha; the manifest's key wins.
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key, SHA: "stale"}),
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

func TestServePaste_LegacyDocumentReadsAndValidatesOnItsSHA(t *testing.T) {
	blobs := &recordingBlobs{bodies: map[string]string{"deadbeef": "<h1>legacy</h1>"}}
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindHTML, ContentSHA: "deadbeef", UpdatedAt: time.Now().UTC(),
		}},
		Blobs: blobs,
	}
	w := serveOne(srv, "/p/abc23456", "")
	if w.Code != 200 || w.Body.String() != "<h1>legacy</h1>" || w.Header().Get("ETag") != `"deadbeef"` {
		t.Fatalf("legacy GET = %d %q etag %q", w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
	if len(blobs.reads) != 1 || blobs.reads[0].Key != "" || blobs.reads[0].SHA != "deadbeef" {
		t.Fatalf("reads = %+v, want one legacy read by sha", blobs.reads)
	}
}

func TestServeSite_EntriesResolveByKeyOrSHA(t *testing.T) {
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{Key: "uploads/u3/0", ContentType: "text/html; charset=utf-8"})
	m.Add("legacy.css", domain.ManifestEntry{SHA: "cafe", ContentType: "text/css; charset=utf-8"})
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Kind: domain.KindSite, Manifest: m, UpdatedAt: time.Now().UTC(),
		}},
		Blobs: &recordingBlobs{bodies: map[string]string{"uploads/u3/0": "<h1>site</h1>", "cafe": "body{}"}},
	}
	for target, want := range map[string][2]string{
		"/p/abc23456/":           {"<h1>site</h1>", `"uploads/u3/0"`},
		"/p/abc23456/legacy.css": {"body{}", `"cafe"`},
	} {
		w := serveOne(srv, target, "")
		if w.Code != 200 || w.Body.String() != want[0] || w.Header().Get("ETag") != want[1] {
			t.Fatalf("GET %s = %d %q etag %q, want %q etag %s", target, w.Code, w.Body.String(), w.Header().Get("ETag"), want[0], want[1])
		}
	}
}
