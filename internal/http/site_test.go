package http

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

// stubSiteReader serves one site by slug.
type stubSiteReader struct{ s domain.Site }

func (r stubSiteReader) Get(slug domain.Slug) (domain.Site, error) {
	if r.s.Slug != slug {
		return domain.Site{}, storage.ErrNotFound
	}
	return r.s, nil
}

// stubBlobMap returns bytes by sha, ignoring the slug as the standalone
// content-addressed path does.
type stubBlobMap struct{ m map[string][]byte }

func (b stubBlobMap) ReadAll(_ context.Context, _, sha string) ([]byte, error) {
	body, ok := b.m[sha]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return body, nil
}

func (b stubBlobMap) Read(_ context.Context, _, sha string) (io.ReadCloser, int64, error) {
	body, ok := b.m[sha]
	if !ok {
		return nil, 0, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func buildSiteServer(t *testing.T) *Server {
	t.Helper()
	now := time.Now().UTC()
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-index", Size: 10, ContentType: "text/html; charset=utf-8"})
	m.Add("css/style.css", domain.ManifestEntry{SHA: "sha-css", Size: 5, ContentType: "text/css; charset=utf-8"})
	m.Add("blog/index.html", domain.ManifestEntry{SHA: "sha-blog", Size: 6, ContentType: "text/html; charset=utf-8"})
	m.Add("data.bin", domain.ManifestEntry{SHA: "sha-bin", Size: 3, ContentType: "application/octet-stream"})
	site := domain.Site{
		Slug:      "abc23456",
		Identity:  "key:test",
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return &Server{
		ApexDomain: "paste.test",
		Sites:      stubSiteReader{s: site},
		Blobs: stubBlobMap{m: map[string][]byte{
			"sha-index": []byte("<h1>root</h1>"),
			"sha-css":   []byte("body{}"),
			"sha-blog":  []byte("<h1>blog</h1>"),
			"sha-bin":   []byte("\x00\x01\x02"),
		}},
	}
}

func TestSite_ServesFilesAndIndex(t *testing.T) {
	srv := buildSiteServer(t)
	mux := srv.Handler()
	cases := []struct {
		path  string
		code  int
		body  string
		ctype string
	}{
		{"/", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"/index.html", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"/css/style.css", 200, "body{}", "text/css; charset=utf-8"},
		{"/blog/", 200, "<h1>blog</h1>", "text/html; charset=utf-8"},
		{"/blog", 200, "<h1>blog</h1>", "text/html; charset=utf-8"},
		{"/data.bin", 200, "\x00\x01\x02", "application/octet-stream"},
		// SPA fallback: a ".html" miss is a client-side route and serves the
		// root index.html; a ".css" miss is a real missing asset and 404s.
		{"/missing.html", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"/blog/missing.css", 404, "", ""},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.path, nil)
			r.Host = "abc23456.paste.test"
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != c.code {
				t.Fatalf("code: got %d, want %d", w.Code, c.code)
			}
			if c.code != 200 {
				return
			}
			if w.Body.String() != c.body {
				t.Fatalf("body: got %q, want %q", w.Body.String(), c.body)
			}
			if ct := w.Header().Get("Content-Type"); ct != c.ctype {
				t.Fatalf("content-type: got %q, want %q", ct, c.ctype)
			}
		})
	}
}

// TestSite_SPAFallback pins route-vs-asset behavior over the real handler: a
// route-shaped miss serves the ROOT index.html, an asset-shaped miss 404s, and
// real files and directory indexes are unaffected.
func TestSite_SPAFallback(t *testing.T) {
	srv := buildSiteServer(t)
	mux := srv.Handler()

	cases := []struct {
		name  string
		path  string
		code  int
		body  string // checked only on 200
		ctype string // checked only on 200
	}{
		// Real files / indexes resolve directly, with no fallback.
		{"root", "/", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"dir index", "/blog/", 200, "<h1>blog</h1>", "text/html; charset=utf-8"},
		{"real file", "/css/style.css", 200, "body{}", "text/css; charset=utf-8"},

		// Route-shaped misses serve the ROOT index.html via the fallback.
		{"no-ext route", "/about", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"deep route", "/users/123", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"nested route", "/users/123/edit", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"html route", "/about.html", 200, "<h1>root</h1>", "text/html; charset=utf-8"},

		// Asset-shaped misses 404.
		{"missing js", "/assets/nope.js", 404, "", ""},
		{"missing css", "/styles/gone.css", 404, "", ""},
		{"missing png", "/img/missing.png", 404, "", ""},
		{"missing woff2", "/fonts/x.woff2", 404, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.path, nil)
			r.Host = "abc23456.paste.test"
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != c.code {
				t.Fatalf("code: got %d, want %d", w.Code, c.code)
			}
			if c.code != 200 {
				return
			}
			if w.Body.String() != c.body {
				t.Fatalf("body: got %q, want %q", w.Body.String(), c.body)
			}
			if ct := w.Header().Get("Content-Type"); ct != c.ctype {
				t.Fatalf("content-type: got %q, want %q", ct, c.ctype)
			}
		})
	}
}

// TestSite_SPAFallback_SameHeadersAsRoot pins that a fallback response is
// indistinguishable from requesting "/": same body, content-type, sandbox
// headers, and ETag.
func TestSite_SPAFallback_SameHeadersAsRoot(t *testing.T) {
	srv := buildSiteServer(t)
	mux := srv.Handler()

	get := func(p string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "abc23456.paste.test"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	root := get("/")
	route := get("/about")

	if route.Code != 200 || root.Code != 200 {
		t.Fatalf("codes: root=%d route=%d, want both 200", root.Code, route.Code)
	}
	if route.Body.String() != root.Body.String() {
		t.Fatalf("body mismatch: route=%q root=%q", route.Body.String(), root.Body.String())
	}
	for _, hdr := range []string{
		"Content-Type", "X-Frame-Options", "Referrer-Policy",
		"Permissions-Policy", "Cache-Control", "ETag",
	} {
		if route.Header().Get(hdr) != root.Header().Get(hdr) {
			t.Fatalf("header %s: route=%q root=%q", hdr,
				route.Header().Get(hdr), root.Header().Get(hdr))
		}
	}
	if route.Header().Get("ETag") != `"sha-index"` {
		t.Fatalf("fallback etag: got %q, want %q", route.Header().Get("ETag"), `"sha-index"`)
	}
	// Sites serve no-cache so a re-deploy shows on the next reload. Under
	// max-age a browser would keep serving cached js/css sub-resources without
	// revalidating and updates would never appear.
	if got := root.Header().Get("Cache-Control"); got != "public, no-cache" {
		t.Fatalf("site Cache-Control: got %q, want public, no-cache", got)
	}
}

func TestSite_SandboxHeaders(t *testing.T) {
	srv := buildSiteServer(t)
	mux := srv.Handler()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	h := w.Header()
	if h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing X-Frame-Options: %q", h.Get("X-Frame-Options"))
	}
	if h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing Referrer-Policy")
	}
	if h.Get("Permissions-Policy") == "" {
		t.Fatalf("missing Permissions-Policy")
	}
	if h.Get("ETag") != `"sha-index"` {
		t.Fatalf("etag: got %q", h.Get("ETag"))
	}
}

func TestSite_PathMode(t *testing.T) {
	srv := buildSiteServer(t)
	mux := srv.Handler()
	// Path-mode requests on the apex Host (no slug subdomain).
	cases := []struct {
		path string
		code int
		body string
	}{
		{"/p/abc23456", 200, "<h1>root</h1>"},
		{"/p/abc23456/css/style.css", 200, "body{}"},
		{"/p/abc23456/blog/", 200, "<h1>blog</h1>"},
		// The SPA fallback applies in path mode too.
		{"/p/abc23456/missing.html", 200, "<h1>root</h1>"},
		{"/p/abc23456/missing.js", 404, ""},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.path, nil)
			r.Host = "paste.test"
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != c.code {
				t.Fatalf("code: got %d, want %d", w.Code, c.code)
			}
			if c.code == 200 && w.Body.String() != c.body {
				t.Fatalf("body: got %q, want %q", w.Body.String(), c.body)
			}
		})
	}
}

func TestSite_FallsThroughToPasteWhenNoSite(t *testing.T) {
	// A slug that is NOT a site falls through to the paste path. The stub
	// paste owning the slug is required: servePasteSlug panics with no paste
	// reader wired.
	now := time.Now().UTC()
	p := domain.Paste{
		Slug: "abc23456", Identity: "key:test", Kind: domain.KindHTML,
		ContentSHA: "sha-p", Size: 5, UpdatedAt: now}
	srv := &Server{
		ApexDomain: "paste.test",
		Pastes:     stubPasteReader{p: p},
		Sites:      stubSiteReader{s: domain.Site{Slug: "zzzzzzzz"}}, // different slug
		Blobs:      stubBlobMap{m: map[string][]byte{"sha-p": []byte("paste")}},
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "paste" {
		t.Fatalf("paste fallthrough: code=%d body=%q", w.Code, w.Body.String())
	}
}

// A directory artifact serves its files from the HEAD's manifest, with no site
// row involved. Pins the unified read path: the same manifest lookup the legacy
// site path uses, reached through the artifact instead.
func TestArtifact_DirectoryServesFromHeadManifest(t *testing.T) {
	now := time.Now().UTC()
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-index", Size: 5, ContentType: "text/html"})
	m.Add("app.css", domain.ManifestEntry{SHA: "sha-css", Size: 3, ContentType: "text/css"})

	srv := &Server{
		ApexDomain: "paste.test",
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abcd2345", Identity: "key:test",
			// The KIND declares the shape; the manifest holds the content.
			Kind: domain.KindSite, UpdatedAt: now, Manifest: m,
		}},
		Blobs: stubBlobMap{m: map[string][]byte{
			"sha-index": []byte("index"),
			"sha-css":   []byte("css"),
		}},
	}

	for _, c := range []struct{ path, body, ct string }{
		{"/", "index", "text/html"},
		{"/app.css", "css", "text/css"},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Host = "abcd2345.paste.test"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != c.body {
			t.Fatalf("%s: code=%d body=%q, want 200/%q", c.path, w.Code, w.Body.String(), c.body)
		}
		if got := w.Header().Get("Content-Type"); got != c.ct {
			t.Fatalf("%s: content-type %q, want %q", c.path, got, c.ct)
		}
	}
}

// A document artifact answers only at its own URL: a deeper path is not a file
// inside it.
func TestArtifact_DocumentRejectsDeepPaths(t *testing.T) {
	srv := &Server{
		ApexDomain: "paste.test",
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "wxyz6789", Identity: "key:test", Kind: domain.KindHTML,
			ContentSHA: "sha-d", Size: 3, UpdatedAt: time.Now().UTC()}},
		Blobs: stubBlobMap{m: map[string][]byte{"sha-d": []byte("doc")}},
	}
	// The bare URL must SERVE, or the 404 below would prove only that the
	// request never reached the document path.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "wxyz6789.paste.test"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "doc" {
		t.Fatalf("document at its own URL: code=%d body=%q, want 200/doc", w.Code, w.Body.String())
	}

	r = httptest.NewRequest("GET", "/deeper/path", nil)
	r.Host = "wxyz6789.paste.test"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("deep path into a document: code=%d, want 404", w.Code)
	}
}
