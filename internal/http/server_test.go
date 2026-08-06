package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

type stubPasteReader struct{ p domain.Paste }

func (s stubPasteReader) Get(slug domain.Slug) (domain.Paste, error) {
	if s.p.Slug != slug {
		return domain.Paste{}, storage.ErrNotFound
	}
	return s.p, nil
}

type stubBlobReader struct{ body []byte }

func (s stubBlobReader) ReadAll(_ context.Context, _, _ string) ([]byte, error) { return s.body, nil }
func (s stubBlobReader) Read(_ context.Context, _, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(s.body)), int64(len(s.body)), nil
}

func TestSlugFromHost(t *testing.T) {
	s := &Server{ApexDomain: "paste.test"}
	cases := []struct {
		host   string
		want   string // empty when expected ok=false
		wantOK bool
	}{
		{"abc23456.paste.test", "abc23456", true},
		{"abc23456.paste.test:443", "abc23456", true},
		{"paste.test", "", false},
		{"paste.test:443", "", false},
		// Multi-level subdomain.
		{"foo.abc23456.paste.test", "", false},
		// Wrong apex.
		{"abc23456.example.com", "", false},
		// Too short for ParseSlug.
		{"abc.paste.test", "", false},
		// Uppercase fails ParseSlug.
		{"ABC23456.paste.test", "", false},
		// Valid slug shape is accepted even if it reads like a reserved
		// word: reserved names are blocked by never being generated.
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

// An empty ApexDomain must never match a host: that deploy is path-mode only.
func TestSlugFromHost_NoApexConfigured(t *testing.T) {
	s := &Server{}
	if _, ok := s.slugFromHost("abc23456.anything.com"); ok {
		t.Fatalf("should not match without ApexDomain")
	}
}

// TestSubdomain_OnlyServesRoot pins that a slug subdomain serves the paste at
// "/" and nothing else: without it, Safari's auto-favicon request receives the
// whole HTML body and its loading indicator hangs.
//
// "/" itself is not exercised here (no real PasteReader is wired); this covers
// the path-rejection branch only.
func TestSubdomain_OnlyServesRoot(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	mux := srv.Handler()

	r := httptest.NewRequest("GET", "/favicon.ico", nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("/favicon.ico on slug subdomain: got %d, want 404", w.Code)
	}

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

// TestPasteRead_CacheHeaders pins Cache-Control + ETag + Last-Modified on a
// successful read: they are the contract with the CDN, and a regression breaks
// edge caching silently.
func TestPasteRead_CacheHeaders(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	body := []byte("<!doctype html><h1>hi</h1>")
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  updatedAt,
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

// TestPasteRead_PendingServesLoadingPage pins the pending lifecycle state: a
// GET serves the auto-refreshing loading page (200, no-store, meta-refresh)
// and never the content. docs/SPEC.md "Paste lifecycle status".
func TestPasteRead_PendingServesLoadingPage(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:   "abc23456",
		Status: domain.PasteStatusPending,
		Kind:   domain.KindHTML,
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: []byte("should not be served")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want no-store", got)
	}
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "http-equiv=\"refresh\"") {
		t.Errorf("loading page missing meta-refresh: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "preparing your paste") {
		t.Errorf("loading page missing expected copy")
	}
	if strings.Contains(bodyStr, "should not be served") {
		t.Errorf("pending paste leaked content bytes")
	}
}

// TestPasteRead_FailedServesErrorPage pins the failed lifecycle state: a GET
// serves the error page (410 Gone, no-store), never the content.
func TestPasteRead_FailedServesErrorPage(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:   "abc23456",
		Status: domain.PasteStatusFailed,
		Kind:   domain.KindHTML,
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: []byte("should not be served")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 410 {
		t.Fatalf("status: got %d, want 410", w.Code)
	}
	if !strings.Contains(w.Body.String(), "could not be saved") {
		t.Errorf("error page missing expected copy: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "should not be served") {
		t.Errorf("failed paste leaked content bytes")
	}
}

// TestPasteRead_ReadyServesContent pins the terminal success state: an explicit
// ready status serves the content.
func TestPasteRead_ReadyServesContent(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	body := []byte("<!doctype html><h1>ready</h1>")
	paste := domain.Paste{
		Slug:       "abc23456",
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  now,
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != string(body) {
		t.Errorf("body: got %q, want %q", got, body)
	}
}

// failingBlobReader models the blob plane erroring under a read.
type failingBlobReader struct{ err error }

func (f failingBlobReader) ReadAll(_ context.Context, _, _ string) ([]byte, error) {
	return nil, f.err
}
func (f failingBlobReader) Read(_ context.Context, _, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, f.err
}

// failingPasteReader models the metadata plane erroring under a read.
type failingPasteReader struct{ err error }

func (f failingPasteReader) Get(domain.Slug) (domain.Paste, error) { return domain.Paste{}, f.err }

// TestPasteRead5xxLogsSlugAndError pins the read-surface observability contract
// (docs/SPEC.md "5xx observability on the read surface"): every 5xx logs one
// line carrying the slug and the underlying error, while the response body
// stays the generic "internal error". Without the log a 500 is unattributable.
func TestPasteRead5xxLogsSlugAndError(t *testing.T) {
	now := time.Now().UTC()
	paste := domain.Paste{
		Slug:       "abc23456",
		Kind:       domain.KindMarkdown,
		ContentSHA: "deadbeef",
		Status:     domain.PasteStatusReady,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	t.Run("blob read failure", func(t *testing.T) {
		var logged strings.Builder
		srv := &Server{
			Pastes: stubPasteReader{p: paste},
			Blobs:  failingBlobReader{err: context.DeadlineExceeded},
			Logf:   func(f string, a ...any) { logged.WriteString(strings.TrimSpace(sprintf(f, a...)) + "\n") },
			Now:    func() time.Time { return now },
		}
		r := httptest.NewRequest("GET", "/p/abc23456?raw=1", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 500 {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if got := w.Body.String(); !strings.Contains(got, "internal error") {
			t.Fatalf("body %q must stay the generic internal error", got)
		}
		if out := logged.String(); !strings.Contains(out, "abc23456") || !strings.Contains(out, "deadline") {
			t.Fatalf("5xx blob read must log slug + underlying error, got %q", out)
		}
	})

	t.Run("metadata read failure", func(t *testing.T) {
		var logged strings.Builder
		srv := &Server{
			Pastes: failingPasteReader{err: context.DeadlineExceeded},
			Blobs:  stubBlobReader{},
			Logf:   func(f string, a ...any) { logged.WriteString(strings.TrimSpace(sprintf(f, a...)) + "\n") },
			Now:    func() time.Time { return now },
		}
		r := httptest.NewRequest("GET", "/p/abc23456", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 500 {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if out := logged.String(); !strings.Contains(out, "abc23456") || !strings.Contains(out, "deadline") {
			t.Fatalf("5xx metadata read must log slug + underlying error, got %q", out)
		}
	})

	t.Run("nil Logf does not panic", func(t *testing.T) {
		srv := &Server{
			Pastes: stubPasteReader{p: paste},
			Blobs:  failingBlobReader{err: context.DeadlineExceeded},
			Now:    func() time.Time { return now },
		}
		r := httptest.NewRequest("GET", "/p/abc23456?raw=1", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 500 {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
