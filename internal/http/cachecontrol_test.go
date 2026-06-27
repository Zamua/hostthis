package http

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// These tests pin the CDN-cache posture (issue #3). The content-negotiated
// BARE URL of a client-rendered kind (markdown, diff) must be
// Cache-Control: no-store for BOTH representations - the shell (browser
// Accept) and the raw-by-Accept bytes (curl Accept). A CDN can't vary on
// Accept, so a cacheable bare URL lets whichever client primes the edge
// first pin its variant for everyone. The explicit ?raw=1 URL is NOT
// content-negotiated and stays edge-cacheable (public, max-age=3600), as
// does a non-negotiated HTML paste.

// cachePosture serves slug with the given Accept header and returns the
// response Cache-Control + Content-Type.
func cachePosture(t *testing.T, p domain.Paste, body []byte, target, accept string) (cacheControl, contentType string) {
	t.Helper()
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	srv := &Server{
		Pastes:     stubPasteReader{p: p},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
		Now:        func() time.Time { return now.Add(time.Hour) },
	}
	r := httptest.NewRequest("GET", target, nil)
	r.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%s (Accept %q): status %d, want 200", target, accept, w.Code)
	}
	return w.Header().Get("Cache-Control"), w.Header().Get("Content-Type")
}

func TestBareURL_NoStore_Markdown(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	p := mdPaste("abc23456", sha, now)
	body := []byte("# hello\n\nbody")

	// Bare URL, browser Accept -> shell, no-store.
	cc, ct := cachePosture(t, p, body, "/p/abc23456", "text/html,application/xhtml+xml")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare browser Accept: Content-Type %q, want the shell (text/html)", ct)
	}
	if cc != "no-store" {
		t.Errorf("bare browser Accept: Cache-Control %q, want no-store (bare URL must not be edge-cached)", cc)
	}

	// Bare URL, non-html Accept (curl/bot) -> raw bytes, ALSO no-store.
	// This is the representation the bug left at max-age=3600.
	cc, ct = cachePosture(t, p, body, "/p/abc23456", "*/*")
	if ct != "text/markdown; charset=utf-8" {
		t.Fatalf("bare non-html Accept: Content-Type %q, want raw (text/markdown)", ct)
	}
	if cc != "no-store" {
		t.Errorf("bare non-html Accept: Cache-Control %q, want no-store - the raw-by-Accept bare URL must not inherit max-age=3600 (this is the bug)", cc)
	}

	// Explicit ?raw=1 -> raw bytes, stays edge-cacheable.
	cc, ct = cachePosture(t, p, body, "/p/abc23456?raw=1", "text/html")
	if ct != "text/markdown; charset=utf-8" {
		t.Fatalf("?raw=1: Content-Type %q, want raw (text/markdown)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("?raw=1: Cache-Control %q, want public, max-age=3600 (not negotiated, stays cacheable)", cc)
	}
}

func TestBareURL_NoStore_Diff(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	p := diffPaste("abc23456", sha, now)
	body := []byte(sampleDiff)

	// Bare URL, browser Accept -> shell, no-store.
	cc, ct := cachePosture(t, p, body, "/p/abc23456", "text/html,application/xhtml+xml")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare browser Accept: Content-Type %q, want the diff shell (text/html)", ct)
	}
	if cc != "no-store" {
		t.Errorf("bare browser Accept: Cache-Control %q, want no-store (bare URL must not be edge-cached)", cc)
	}

	// Bare URL, non-html Accept (curl/bot) -> raw diff bytes, ALSO no-store.
	cc, ct = cachePosture(t, p, body, "/p/abc23456", "*/*")
	if ct != "text/plain; charset=utf-8" {
		t.Fatalf("bare non-html Accept: Content-Type %q, want raw diff (text/plain)", ct)
	}
	if cc != "no-store" {
		t.Errorf("bare non-html Accept: Cache-Control %q, want no-store - the raw-by-Accept diff bare URL must not inherit max-age=3600 (this is the bug)", cc)
	}

	// Explicit ?raw=1 -> raw diff bytes, stays edge-cacheable.
	cc, ct = cachePosture(t, p, body, "/p/abc23456?raw=1", "text/html")
	if ct != "text/plain; charset=utf-8" {
		t.Fatalf("?raw=1: Content-Type %q, want raw diff (text/plain)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("?raw=1: Cache-Control %q, want public, max-age=3600 (not negotiated, stays cacheable)", cc)
	}
}

// TestBareURL_HTMLPaste_Cacheable confirms an HTML paste's bare URL is
// unaffected by the no-store rule: HTML is not content-negotiated (one
// representation) so it stays edge-cacheable.
func TestBareURL_HTMLPaste_Cacheable(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	p := domain.Paste{
		Slug:       "abc23456",
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  now,
		ExpiresAt:  now.Add(7 * 24 * time.Hour),
	}
	cc, _ := cachePosture(t, p, []byte("<!doctype html><h1>x</h1>"), "/p/abc23456", "text/html")
	if cc != "public, max-age=3600" {
		t.Errorf("HTML bare URL: Cache-Control %q, want public, max-age=3600 (HTML is not negotiated)", cc)
	}
}
