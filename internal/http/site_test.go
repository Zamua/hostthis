package http

import (
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

// stubBlobMap returns bytes by sha.
type stubBlobMap struct{ m map[string][]byte }

func (b stubBlobMap) Get(sha string) ([]byte, error) {
	body, ok := b.m[sha]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return body, nil
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
		ExpiresAt: now.Add(domain.RetentionWindow),
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
		// SPA fallback: a ".html" miss is a client-side route, so it serves
		// the root index.html (200). A ".css" miss is a real missing asset,
		// so it stays a 404. See domain.Manifest.LookupWithSPAFallback.
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

// TestSite_SPAFallback pins the route-vs-asset behavior end-to-end over
// the real HTTP handler: a route-shaped miss serves the ROOT index.html
// (200, index bytes, same headers as serving "/" directly), while an
// asset-shaped miss stays a 404. Real files and directory indexes are
// unaffected.
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
		// Real files / indexes still resolve directly (no fallback).
		{"root", "/", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"dir index", "/blog/", 200, "<h1>blog</h1>", "text/html; charset=utf-8"},
		{"real file", "/css/style.css", 200, "body{}", "text/css; charset=utf-8"},

		// Route-shaped misses serve the ROOT index.html via the fallback.
		{"no-ext route", "/about", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"deep route", "/users/123", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"nested route", "/users/123/edit", 200, "<h1>root</h1>", "text/html; charset=utf-8"},
		{"html route", "/about.html", 200, "<h1>root</h1>", "text/html; charset=utf-8"},

		// Asset-shaped misses 404 (a genuinely-missing asset).
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

// TestSite_SPAFallback_SameHeadersAsRoot proves a fallback response is
// byte-identical to requesting "/": same body, content-type, sandbox
// headers, and ETag (the root index.html's content SHA).
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
	// Sites serve no-cache: a re-deploy must be visible on the next normal
	// reload. Under max-age a browser would keep serving a site's cached
	// js/css sub-resources without revalidating, so updates would not show.
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
		// SPA fallback also applies in path mode: a ".html" miss serves the
		// root index.html (200), a ".js" miss stays a 404.
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

func TestSite_ExpiredReturns404(t *testing.T) {
	now := time.Now().UTC()
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-index", Size: 1, ContentType: "text/html; charset=utf-8"})
	site := domain.Site{
		Slug: "abc23456", Identity: "key:test", Manifest: m,
		CreatedAt: now.Add(-2 * domain.RetentionWindow),
		UpdatedAt: now.Add(-2 * domain.RetentionWindow),
		ExpiresAt: now.Add(-time.Hour), // expired
	}
	srv := &Server{
		ApexDomain: "paste.test",
		Sites:      stubSiteReader{s: site},
		Blobs:      stubBlobMap{m: map[string][]byte{"sha-index": []byte("x")}},
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("expired site: got %d, want 404", w.Code)
	}
}

func TestSite_FallsThroughToPasteWhenNoSite(t *testing.T) {
	// A slug that is NOT a site falls through to the paste path. With no
	// paste reader wired, servePasteSlug would panic - so we wire a stub
	// paste that owns the slug, and prove the site path declines cleanly.
	now := time.Now().UTC()
	p := domain.Paste{
		Slug: "abc23456", Identity: "key:test", Kind: domain.KindHTML,
		ContentSHA: "sha-p", Size: 5, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
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
