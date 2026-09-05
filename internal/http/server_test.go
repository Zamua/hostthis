package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
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

func (s stubBlobReader) Read(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(s.body)), int64(len(s.body)), nil
}

func TestWriteRelayAdmitErrorMapsDrainingToUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	writeRelayAdmitError(w, roomwire.ErrRelayDraining)
	if w.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
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

// readyHTMLServer wires one ready HTML paste at abc23456 with shaA as its
// content, updated at updatedAt.
func readyHTMLServer(body []byte, updatedAt time.Time) *Server {
	return &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindHTML,
			ContentSHA: shaA, UpdatedAt: updatedAt,
		}},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
	}
}

// getPaste serves GET /p/abc23456 with the given request headers.
func getPaste(srv *Server, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// Cache-Control, ETag, and Last-Modified are the contract with the CDN.
func TestPasteRead_CacheHeaders(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	body := []byte("<!doctype html><h1>hi</h1>")
	w := getPaste(readyHTMLServer(body, updatedAt), nil)
	if w.Code != 200 || w.Body.String() != string(body) {
		t.Fatalf("status %d body %q, want 200 with the content", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control: got %q, want public, max-age=3600", got)
	}
	if got := w.Header().Get("ETag"); got != `"`+shaA+`"` {
		t.Errorf("ETag: got %q, want the content SHA", got)
	}
	if got := w.Header().Get("Last-Modified"); got != "Sun, 07 Jun 2026 14:00:00 GMT" {
		t.Errorf("Last-Modified: got %q", got)
	}
}

// A matching validator on either header answers 304 with no body.
func TestPasteRead_Conditional304(t *testing.T) {
	updatedAt := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	srv := readyHTMLServer([]byte("body"), updatedAt)
	for name, hdr := range map[string]map[string]string{
		"If-None-Match":     {"If-None-Match": `"` + shaA + `"`},
		"If-Modified-Since": {"If-Modified-Since": updatedAt.Add(time.Hour).UTC().Format(stdhttp.TimeFormat)},
	} {
		w := getPaste(srv, hdr)
		if w.Code != 304 || w.Body.Len() > 0 {
			t.Errorf("%s: status %d body %d bytes, want 304 and empty", name, w.Code, w.Body.Len())
		}
	}
}

// /healthz answers on any Host, uncached, and names the replica color only
// when one is configured.
func TestHealthz(t *testing.T) {
	for _, color := range []string{"blue", ""} {
		srv := &Server{ApexDomain: "paste.test", Color: color}
		r := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "ok\n" {
			t.Errorf("color %q: status %d body %q, want 200 ok", color, w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Backend-Color"); got != color {
			t.Errorf("X-Backend-Color: got %q, want %q", got, color)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control: got %q, want no-store", got)
		}
	}
}

// The lifecycle gate (docs/SPEC.md "Paste lifecycle status"): only a ready
// paste serves its content. A pending paste serves an uncached 200 with
// Retry-After, the signal e2e polls on; a failed paste is an uncached 410
// without it.
func TestPasteRead_Lifecycle(t *testing.T) {
	content := []byte("<!doctype html><h1>ready</h1>")
	cases := []struct {
		status     domain.PasteStatus
		code       int
		retryAfter bool
		serves     bool
	}{
		{domain.PasteStatusPending, 200, true, false},
		{domain.PasteStatusFailed, 410, false, false},
		{domain.PasteStatusReady, 200, false, true},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			srv := readyHTMLServer(content, time.Now().UTC())
			p := srv.Pastes.(stubPasteReader).p
			p.Status = c.status
			srv.Pastes = stubPasteReader{p: p}
			w := getPaste(srv, nil)
			if w.Code != c.code {
				t.Fatalf("status: got %d, want %d", w.Code, c.code)
			}
			if got := w.Header().Get("Retry-After") != ""; got != c.retryAfter {
				t.Errorf("Retry-After present: got %v, want %v", got, c.retryAfter)
			}
			if got := w.Body.String() == string(content); got != c.serves {
				t.Errorf("serves content: got %v, want %v (body %q)", got, c.serves, w.Body.String())
			}
			if !c.serves {
				if got := w.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control: got %q, want no-store", got)
				}
			}
		})
	}
}

// failingBlobReader models the blob plane erroring under a read.
type failingBlobReader struct{ err error }

func (f failingBlobReader) Read(_ context.Context, _ string) (io.ReadCloser, int64, error) {
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
