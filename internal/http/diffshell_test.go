package http

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// diffPaste builds a ready diff paste fixture with the given slug + content
// SHA. The serve-time content comes from the stub blob reader.
func diffPaste(slug, sha string, now time.Time) domain.Paste {
	return domain.Paste{
		Slug:       domain.Slug(slug),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindDiff,
		ContentSHA: sha,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(7 * 24 * time.Hour),
	}
}

const sampleDiff = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n-old\n+new\n+added\n"

// TestDiffShell_ServedForBrowser pins the shell branch: a browser
// navigation (Accept: text/html, no ?raw) gets the fixed client-render
// diff shell with the CSP header, and the shell ETag is content-INDEPENDENT.
func TestDiffShell_ServedForBrowser(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	srv := &Server{
		Pastes:     stubPasteReader{p: diffPaste("abc23456", sha, now)},
		Blobs:      stubBlobReader{body: []byte(sampleDiff)},
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
	if !strings.Contains(body, "/_hostthis/diff.js") {
		t.Errorf("shell missing /_hostthis/diff.js, got %q", body)
	}
	if !strings.Contains(body, "/_hostthis/diff2html-ui-base.min.js") {
		t.Errorf("shell missing diff2html lib, got %q", body)
	}
	if !strings.Contains(body, "/_hostthis/highlight.min.js") {
		t.Errorf("shell missing highlight.js, got %q", body)
	}
	if !strings.Contains(body, `id="diff"`) {
		t.Errorf("shell missing id=\"diff\", got %q", body)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP missing script-src 'self', got %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP missing connect-src 'self', got %q", csp)
	}

	// The shell ETag must be content-INDEPENDENT: a different diff paste
	// (different slug + content SHA) yields the SAME shell ETag.
	etag1 := w.Header().Get("ETag")
	sha2 := "00001111222233334444555566667777888899990000111122223333deadbeef"
	srv2 := &Server{
		Pastes:     stubPasteReader{p: diffPaste("xyz98765", sha2, now)},
		Blobs:      stubBlobReader{body: []byte(sampleDiff)},
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

// TestDiffRaw_QueryParam pins the raw branch via ?raw=1: the server streams
// the exact raw diff bytes unchanged as text/plain with the content SHA as
// the ETag and no CSP.
func TestDiffRaw_QueryParam(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	src := []byte(sampleDiff)
	srv := &Server{
		Pastes:     stubPasteReader{p: diffPaste("abc23456", sha, now)},
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
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want text/plain; charset=utf-8", ct)
	}
	if got := w.Body.String(); got != string(src) {
		t.Errorf("raw body: got %q, want %q (must be byte-identical)", got, src)
	}
	wantETag := `"` + sha + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag: got %q, want %q", got, wantETag)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("raw response should NOT carry a CSP, got %q", csp)
	}
}

// TestDiffRaw_NonHTMLAccept pins the curl path: Accept */* with no ?raw
// serves the RAW diff, not the shell.
func TestDiffRaw_NonHTMLAccept(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	src := []byte(sampleDiff)
	srv := &Server{
		Pastes:     stubPasteReader{p: diffPaste("abc23456", sha, now)},
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
	if got := w.Body.String(); got != string(src) {
		t.Errorf("raw body: got %q, want %q", got, src)
	}
}

// TestServeDiffAsset_WhitelistAndDeny pins the diff asset handler: a
// whitelisted lib serves with the immutable cache header and a non-empty
// body; an unknown name 404s.
func TestServeDiffAsset_WhitelistAndDeny(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	mux := srv.Handler()

	for _, name := range []string{"diff2html.min.css", "diff2html-ui-base.min.js", "highlight.min.js", "diff.js", "diff.css", "hljs-light.css", "hljs-dark.css"} {
		r := httptest.NewRequest("GET", "/_hostthis/"+name, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: got %d, want 200", name, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control: got %q, want it to contain immutable", name, cc)
		}
		if w.Body.Len() == 0 {
			t.Errorf("%s body is empty", name)
		}
	}

	r := httptest.NewRequest("GET", "/_hostthis/diffshell-evil.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("evil asset: got %d, want 404", w.Code)
	}
}

// TestDiffShell_FetchesRawQuery pins diff.js to the "?raw=1" query the
// cache-invalidation policy (internal/cache) assumes the shell fetches, in
// lockstep with the markdown shell's equivalent guard.
func TestDiffShell_FetchesRawQuery(t *testing.T) {
	b, err := diffShellFS.ReadFile("assets/diffshell/diff.js")
	if err != nil {
		t.Fatalf("read diff.js: %v", err)
	}
	if !strings.Contains(string(b), `"?raw=1"`) {
		t.Errorf("diff.js must fetch \"?raw=1\" so cache purge stays in lockstep")
	}
}
