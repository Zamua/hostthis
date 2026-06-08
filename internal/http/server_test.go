package http

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// stubPasteReader / stubBlobReader satisfy the read interfaces.
type stubPasteReader struct{ p domain.Paste }

func (s stubPasteReader) Get(slug domain.Slug) (domain.Paste, error) {
	if s.p.Slug != slug {
		return domain.Paste{}, storage.ErrNotFound
	}
	return s.p, nil
}

type stubBlobReader struct{ body []byte }

func (s stubBlobReader) Get(sha string) ([]byte, error) { return s.body, nil }

func TestSlugFromHost(t *testing.T) {
	s := &Server{ApexDomain: "paste.test"}
	cases := []struct {
		host    string
		want    string // empty when expected ok=false
		wantOK  bool
	}{
		{"abc23456.paste.test", "abc23456", true},
		{"abc23456.paste.test:443", "abc23456", true},
		{"paste.test", "", false},
		{"paste.test:443", "", false},
		// Multi-level subdomain — ignored.
		{"foo.abc23456.paste.test", "", false},
		// Wrong apex — ignored.
		{"abc23456.example.com", "", false},
		// Slug too short — fails ParseSlug.
		{"abc.paste.test", "", false},
		// Slug uses uppercase — fails ParseSlug.
		{"ABC23456.paste.test", "", false},
		// Reserved-ish but valid slug shape — accepted (apex landing
		// blocks reserved by never generating them, not by Host check).
		{"abcdefgh.paste.test", "abcdefgh", true},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			got, ok := s.slugFromHost(c.host)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v (slug=%q)", ok, c.wantOK, got)
			}
			if string(got) != c.want {
				t.Fatalf("slug: got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSlugFromHost_NoApexConfigured(t *testing.T) {
	// When ApexDomain is empty, never match — only path-mode works.
	s := &Server{}
	if _, ok := s.slugFromHost("abc23456.anything.com"); ok {
		t.Fatalf("should not match without ApexDomain")
	}
}

// TestSubdomain_OnlyServesRoot — the bug fix that motivated this
// test: a slug subdomain must ONLY serve the paste at path "/", not
// at /favicon.ico or any other path. Otherwise Safari's auto-favicon
// request gets a 60 KB HTML response and the loading indicator
// hangs.
func TestSubdomain_OnlyServesRoot(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	mux := srv.Handler()

	// "/" on a slug subdomain — would call servePasteSlug. We don't
	// have a real PasteReader here so we'll get a 500 / panic. Skip
	// this case; we only care about the path-rejection path.

	// /favicon.ico on a slug subdomain → must be 404.
	r := httptest.NewRequest("GET", "/favicon.ico", nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("/favicon.ico on slug subdomain: got %d, want 404", w.Code)
	}

	// /anything else on slug subdomain → 404.
	for _, path := range []string{"/style.css", "/wp-login.php", "/p/abc23456", "/foo/bar"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "abc23456.paste.test"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s on slug subdomain: got %d, want 404", path, w.Code)
		}
	}
}

// TestPasteRead_CacheHeaders pins the Cache-Control + ETag + Last-Modified
// headers on a successful paste read. Those headers are the contract with
// the CDN; if they regress, edge caching silently breaks.
func TestPasteRead_CacheHeaders(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	expiresAt := updatedAt.Add(7 * 24 * time.Hour)
	body := []byte("<!doctype html><h1>hi</h1>")
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  updatedAt,
		ExpiresAt:  expiresAt,
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return updatedAt.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control: got %q, want public, max-age=3600", got)
	}
	wantETag := `"` + paste.ContentSHA + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag: got %q, want %q", got, wantETag)
	}
	if got := w.Header().Get("Last-Modified"); got == "" || !strings.Contains(got, "Jun 2026") {
		t.Errorf("Last-Modified: got %q", got)
	}
}

func TestPasteRead_IfNoneMatch304(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  updatedAt,
		ExpiresAt:  updatedAt.Add(7 * 24 * time.Hour),
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: []byte("body")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return updatedAt.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	r.Header.Set("If-None-Match", `"`+paste.ContentSHA+`"`)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 304 {
		t.Fatalf("status: got %d, want 304 Not Modified", w.Code)
	}
	if w.Body.Len() > 0 {
		t.Errorf("304 response body should be empty, got %d bytes", w.Body.Len())
	}
}

func TestPasteRead_MarkdownETagIncludesRendererVersion(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindMarkdown,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  updatedAt,
		ExpiresAt:  updatedAt.Add(7 * 24 * time.Hour),
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: []byte("# hi")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return updatedAt.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	etag := w.Header().Get("ETag")
	if !strings.Contains(etag, paste.ContentSHA) {
		t.Errorf("md ETag should include content SHA, got %q", etag)
	}
	if !strings.Contains(etag, "md.v1") {
		t.Errorf("md ETag should include renderer version, got %q", etag)
	}
}

func TestHealthz_ReturnsOK(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test", Color: "blue"}
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "ok\n" {
		t.Errorf("body: got %q, want %q", got, "ok\n")
	}
	if got := w.Header().Get("X-Backend-Color"); got != "blue" {
		t.Errorf("X-Backend-Color: got %q, want blue", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want no-store", got)
	}
}

func TestHealthz_NoColorWhenUnset(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if got := w.Header().Get("X-Backend-Color"); got != "" {
		t.Errorf("X-Backend-Color: got %q, want empty", got)
	}
}

func TestPasteRead_IfModifiedSince304(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  updatedAt,
		ExpiresAt:  updatedAt.Add(7 * 24 * time.Hour),
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: []byte("body")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return updatedAt.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	r.Header.Set("If-Modified-Since", updatedAt.Add(time.Hour).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 304 {
		t.Fatalf("status: got %d, want 304", w.Code)
	}
}
