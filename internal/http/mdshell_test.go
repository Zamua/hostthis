package http

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// mdPaste builds a ready markdown paste fixture with the given slug +
// content SHA. The serve-time content comes from the stub blob reader,
// not from this struct.
func mdPaste(slug, sha string, now time.Time) domain.Paste {
	return domain.Paste{
		Slug:       domain.Slug(slug),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindMarkdown,
		ContentSHA: sha,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(7 * 24 * time.Hour),
	}
}

// TestMarkdownShell_ServedForBrowser pins the shell branch: a browser
// navigation (Accept: text/html, no ?raw) gets the fixed client-render
// shell with the CSP header, and the shell ETag is content-INDEPENDENT.
func TestMarkdownShell_ServedForBrowser(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	srv := &Server{
		Pastes:     stubPasteReader{p: mdPaste("abc23456", sha, now)},
		Blobs:      stubBlobReader{body: []byte("# hello\n\nbody")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/html; charset=utf-8", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/_hostthis/md.js") {
		t.Errorf("shell missing /_hostthis/md.js, got %q", body)
	}
	if !strings.Contains(body, `id="content"`) {
		t.Errorf("shell missing id=\"content\", got %q", body)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatalf("shell missing Content-Security-Policy header")
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP missing script-src 'self', got %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP missing connect-src 'self', got %q", csp)
	}

	// The shell ETag must be content-INDEPENDENT: a different markdown
	// paste (different slug + content SHA) yields the SAME shell ETag.
	etag1 := w.Header().Get("ETag")
	sha2 := "00001111222233334444555566667777888899990000111122223333deadbeef"
	srv2 := &Server{
		Pastes:     stubPasteReader{p: mdPaste("xyz98765", sha2, now)},
		Blobs:      stubBlobReader{body: []byte("# other\n\ndifferent")},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r2 := httptest.NewRequest("GET", "/p/xyz98765", nil)
	r2.Header.Set("Accept", "text/html")
	w2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(w2, r2)
	etag2 := w2.Header().Get("ETag")
	if etag1 == "" || etag1 != etag2 {
		t.Errorf("shell ETag must be content-independent: got %q and %q", etag1, etag2)
	}
	if strings.Contains(etag1, sha) {
		t.Errorf("shell ETag must NOT embed the content SHA, got %q", etag1)
	}
}

// TestMarkdownRaw_QueryParam pins the raw branch via ?raw=1: the server
// streams the exact raw source bytes as text/markdown with the content
// SHA as the ETag.
func TestMarkdownRaw_QueryParam(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	src := []byte("# title\n\nsome **markdown** body\n")
	srv := &Server{
		Pastes:     stubPasteReader{p: mdPaste("abc23456", sha, now)},
		Blobs:      stubBlobReader{body: src},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456?raw=1", nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/markdown; charset=utf-8", ct)
	}
	if got := w.Body.String(); got != string(src) {
		t.Errorf("raw body: got %q, want %q", got, src)
	}
	wantETag := `"` + sha + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag: got %q, want %q", got, wantETag)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("raw response should NOT carry a CSP, got %q", csp)
	}
}

// TestMarkdownRaw_NonHTMLAccept pins the curl path: Accept */* with no
// ?raw serves the RAW markdown, not the shell.
func TestMarkdownRaw_NonHTMLAccept(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	src := []byte("# curl me\n")
	srv := &Server{
		Pastes:     stubPasteReader{p: mdPaste("abc23456", sha, now)},
		Blobs:      stubBlobReader{body: src},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	r.Header.Set("Accept", "*/*")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/markdown; charset=utf-8", ct)
	}
	if got := w.Body.String(); got != string(src) {
		t.Errorf("raw body: got %q, want %q", got, src)
	}
}

// TestServeAsset_WhitelistAndDeny pins the asset handler: a whitelisted
// lib serves with the immutable cache header and a non-empty body; an
// unknown name 404s (no path traversal / arbitrary embedded disclosure).
func TestServeAsset_WhitelistAndDeny(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	mux := srv.Handler()

	r := httptest.NewRequest("GET", "/_hostthis/marked.min.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("marked.min.js: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/javascript; charset=utf-8", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control: got %q, want it to contain immutable", cc)
	}
	if w.Body.Len() == 0 {
		t.Errorf("marked.min.js body is empty")
	}

	r2 := httptest.NewRequest("GET", "/_hostthis/evil.js", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code != 404 {
		t.Errorf("evil.js: got %d, want 404", w2.Code)
	}
}

// TestHTMLPaste_NoShellTreatment confirms the HTML serve path is
// unchanged: still streamed text/html, and it does NOT get the markdown
// shell's CSP header.
func TestHTMLPaste_NoShellTreatment(t *testing.T) {
	now := time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)
	body := []byte("<!doctype html><h1>html paste</h1>")
	paste := domain.Paste{
		Slug:       "abc23456",
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  now,
		ExpiresAt:  now.Add(7 * 24 * time.Hour),
	}
	srv := &Server{
		Pastes:     stubPasteReader{p: paste},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", "/p/abc23456", nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/html; charset=utf-8", ct)
	}
	if got := w.Body.String(); got != string(body) {
		t.Errorf("body: got %q, want %q (HTML should stream raw)", got, body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("HTML paste should NOT carry the markdown shell CSP, got %q", csp)
	}
	wantETag := `"` + paste.ContentSHA + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("HTML ETag: got %q, want %q (content SHA)", got, wantETag)
	}
}
