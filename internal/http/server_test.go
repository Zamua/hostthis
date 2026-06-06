package http

import (
	"net/http/httptest"
	"testing"
)

func TestSlugFromHost(t *testing.T) {
	s := &Server{ApexDomain: "hostthis.dev"}
	cases := []struct {
		host    string
		want    string // empty when expected ok=false
		wantOK  bool
	}{
		{"abc23456.hostthis.dev", "abc23456", true},
		{"abc23456.hostthis.dev:443", "abc23456", true},
		{"hostthis.dev", "", false},
		{"hostthis.dev:443", "", false},
		// Multi-level subdomain — ignored.
		{"foo.abc23456.hostthis.dev", "", false},
		// Wrong apex — ignored.
		{"abc23456.example.com", "", false},
		// Slug too short — fails ParseSlug.
		{"abc.hostthis.dev", "", false},
		// Slug uses uppercase — fails ParseSlug.
		{"ABC23456.hostthis.dev", "", false},
		// Reserved-ish but valid slug shape — accepted (apex landing
		// blocks reserved by never generating them, not by Host check).
		{"abcdefgh.hostthis.dev", "abcdefgh", true},
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
	srv := &Server{ApexDomain: "hostthis.dev"}
	mux := srv.Handler()

	// "/" on a slug subdomain — would call servePasteSlug. We don't
	// have a real PasteReader here so we'll get a 500 / panic. Skip
	// this case; we only care about the path-rejection path.

	// /favicon.ico on a slug subdomain → must be 404.
	r := httptest.NewRequest("GET", "/favicon.ico", nil)
	r.Host = "abc23456.hostthis.dev"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("/favicon.ico on slug subdomain: got %d, want 404", w.Code)
	}

	// /anything else on slug subdomain → 404.
	for _, path := range []string{"/style.css", "/wp-login.php", "/p/abc23456", "/foo/bar"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "abc23456.hostthis.dev"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s on slug subdomain: got %d, want 404", path, w.Code)
		}
	}
}
