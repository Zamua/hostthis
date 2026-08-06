package http

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// These tests pin the CDN-cache posture. The bare URL of a client-rendered kind
// (markdown, diff) serves one representation, the render shell, to every client
// regardless of Accept: no content negotiation, so it is safe to edge-cache
// (public, max-age=3600). Raw bytes are an explicit opt-in via ?raw=1, a
// distinct single representation with the same posture.

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

func TestBareURL_Shell_Markdown(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	p := mdPaste("abc23456", sha, now)
	body := []byte("# hello\n\nbody")

	cc, ct := cachePosture(t, p, body, "/p/abc23456", "text/html,application/xhtml+xml")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare browser Accept: Content-Type %q, want the shell (text/html)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("bare browser Accept: Cache-Control %q, want public, max-age=3600 (single representation, edge-cacheable)", cc)
	}

	cc, ct = cachePosture(t, p, body, "/p/abc23456", "*/*")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare non-html Accept: Content-Type %q, want the shell (text/html) - the bare URL does not content-negotiate", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("bare non-html Accept: Cache-Control %q, want public, max-age=3600 (single representation, edge-cacheable)", cc)
	}

	cc, ct = cachePosture(t, p, body, "/p/abc23456?raw=1", "text/html")
	if ct != "text/markdown; charset=utf-8" {
		t.Fatalf("?raw=1: Content-Type %q, want raw (text/markdown)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("?raw=1: Cache-Control %q, want public, max-age=3600 (distinct single representation, stays cacheable)", cc)
	}
}

func TestBareURL_Shell_Diff(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	sha := "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	p := diffPaste("abc23456", sha, now)
	body := []byte(sampleDiff)

	cc, ct := cachePosture(t, p, body, "/p/abc23456", "text/html,application/xhtml+xml")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare browser Accept: Content-Type %q, want the diff shell (text/html)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("bare browser Accept: Cache-Control %q, want public, max-age=3600 (single representation, edge-cacheable)", cc)
	}

	cc, ct = cachePosture(t, p, body, "/p/abc23456", "*/*")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("bare non-html Accept: Content-Type %q, want the diff shell (text/html) - the bare URL does not content-negotiate", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("bare non-html Accept: Cache-Control %q, want public, max-age=3600 (single representation, edge-cacheable)", cc)
	}

	cc, ct = cachePosture(t, p, body, "/p/abc23456?raw=1", "text/html")
	if ct != "text/plain; charset=utf-8" {
		t.Fatalf("?raw=1: Content-Type %q, want raw diff (text/plain)", ct)
	}
	if cc != "public, max-age=3600" {
		t.Errorf("?raw=1: Cache-Control %q, want public, max-age=3600 (distinct single representation, stays cacheable)", cc)
	}
}

// TestBareURL_HTMLPaste_Cacheable pins that an HTML paste's bare URL stays
// edge-cacheable: HTML is not content-negotiated either.
func TestBareURL_HTMLPaste_Cacheable(t *testing.T) {
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	p := domain.Paste{
		Slug:       "abc23456",
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe",
		UpdatedAt:  now,
	}
	cc, _ := cachePosture(t, p, []byte("<!doctype html><h1>x</h1>"), "/p/abc23456", "text/html")
	if cc != "public, max-age=3600" {
		t.Errorf("HTML bare URL: Cache-Control %q, want public, max-age=3600 (HTML is not negotiated)", cc)
	}
}
