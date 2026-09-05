package http

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

const (
	shaA = "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	shaB = "00001111222233334444555566667777888899990000111122223333deadbeef"
)

// serveReady serves one GET of a ready paste of kind through the mux.
func serveReady(t *testing.T, kind domain.ContentKind, slug, sha string, body []byte, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: domain.Slug(slug), Status: domain.PasteStatusReady, Kind: kind,
			ContentSHA: sha, UpdatedAt: time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC),
		}},
		Blobs:      stubBlobReader{body: body},
		ApexDomain: "paste.test",
	}
	r := httptest.NewRequest("GET", target, nil)
	r.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%s %s (Accept %q): status %d, want 200", kind, target, accept, w.Code)
	}
	return w
}

// Every registered kind serves the same contract: the bare URL is the shell
// for every Accept (no negotiation, so it edge-caches), with the shell's CSP
// and a content-independent ETag; ?raw=1 is the stored bytes under the kind's
// raw type, ETag'd by content, with no CSP.
func TestShells_ServeContractForEveryKind(t *testing.T) {
	if len(shells) == 0 {
		t.Fatal("no shells registered")
	}
	src := []byte("# title\n\nsome **body**\n")
	for kind, sh := range shells {
		t.Run(string(kind), func(t *testing.T) {
			for _, accept := range []string{"text/html,application/xhtml+xml", "*/*"} {
				w := serveReady(t, kind, "abc23456", shaA, src, "/p/abc23456", accept)
				if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
					t.Errorf("Accept %q: Content-Type %q, want the shell", accept, ct)
				}
				if got := w.Body.Bytes(); string(got) != string(sh.html(kind)) {
					t.Errorf("Accept %q: body is not the shell page", accept)
				}
				if csp := w.Header().Get("Content-Security-Policy"); csp != sh.policy() {
					t.Errorf("Accept %q: CSP %q, want %q", accept, csp, sh.policy())
				}
				if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
					t.Errorf("Accept %q: Cache-Control %q, want public, max-age=3600", accept, cc)
				}
				etag := w.Header().Get("ETag")
				if etag != `"`+sh.version+`"` || strings.Contains(etag, shaA) {
					t.Errorf("Accept %q: shell ETag %q, want the shell version", accept, etag)
				}
				other := serveReady(t, kind, "xyz98765", shaB, []byte("other"), "/p/xyz98765", accept)
				if got := other.Header().Get("ETag"); got != etag {
					t.Errorf("shell ETag depends on content: %q vs %q", etag, got)
				}
			}

			w := serveReady(t, kind, "abc23456", shaA, src, "/p/abc23456?raw=1", "text/html")
			if ct := w.Header().Get("Content-Type"); ct != rawContentType[kind] {
				t.Errorf("raw Content-Type %q, want %q", ct, rawContentType[kind])
			}
			if got := w.Body.String(); got != string(src) {
				t.Errorf("raw body %q, want the stored bytes", got)
			}
			if got := w.Header().Get("ETag"); got != `"`+shaA+`"` {
				t.Errorf("raw ETag %q, want the content SHA", got)
			}
			if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
				t.Errorf("raw response carries a CSP: %q", csp)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
				t.Errorf("raw Cache-Control %q, want public, max-age=3600", cc)
			}
		})
	}
}

// HTML is served as itself: the stored bytes, content ETag, no shell CSP, and
// the same edge-cacheable posture.
func TestHTMLPaste_ServedAsItself(t *testing.T) {
	body := []byte("<!doctype html><h1>html paste</h1>")
	w := serveReady(t, domain.KindHTML, "abc23456", shaA, body, "/p/abc23456", "text/html")
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if got := w.Body.String(); got != string(body) {
		t.Errorf("body %q, want the stored bytes", got)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("HTML paste carries a CSP: %q", csp)
	}
	if got := w.Header().Get("ETag"); got != `"`+shaA+`"` {
		t.Errorf("ETag %q, want the content SHA", got)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control %q, want public, max-age=3600", cc)
	}
}

// Every whitelisted asset serves under /_hostthis/<name> with its declared
// type and the immutable cache header; a name outside the whitelist 404s.
func TestServeAsset_WhitelistAndDeny(t *testing.T) {
	mux := (&Server{ApexDomain: "paste.test"}).Handler()
	for name, sh := range assetSource {
		r := httptest.NewRequest("GET", "/_hostthis/"+name, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s: status %d, want 200", name, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != sh.assets[name] {
			t.Errorf("%s: Content-Type %q, want %q", name, ct, sh.assets[name])
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q, want immutable", name, cc)
		}
		if w.Body.Len() == 0 {
			t.Errorf("%s: empty body", name)
		}
	}
	for _, name := range []string{"evil.js", "shell.html"} {
		r := httptest.NewRequest("GET", "/_hostthis/"+name, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("%s: status %d, want 404", name, w.Code)
		}
	}
}

// Every shell's bootstrap fetches "<path>?raw=1", the suffix
// internal/cache/urls.go purges alongside the bare URL. A shell fetching any
// other query would serve stale bytes after an edit with nothing failing.
func TestShells_FetchRawQueryInLockstepWithCachePurge(t *testing.T) {
	seen := map[*clientShell]bool{}
	for kind, sh := range shells {
		if seen[sh] {
			continue
		}
		seen[sh] = true
		found := false
		for name := range sh.assets {
			if !strings.HasSuffix(name, ".js") || strings.Contains(name, ".min.") {
				continue
			}
			b, err := sh.fs.ReadFile(sh.dir + "/" + name)
			if err == nil && strings.Contains(string(b), `"?raw=1"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no first-party script under %s fetches \"?raw=1\"", kind, sh.dir)
		}
	}
}
