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
		{"/missing.html", 404, "", ""},
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
		{"/p/abc23456/missing.html", 404, ""},
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
