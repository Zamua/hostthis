package http

import (
	"net/http/httptest"
	"testing"
)

func serve(srv *Server, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// /healthz answers on any Host, uncached, and names the replica color only
// when one is configured.
func TestHealthz(t *testing.T) {
	for _, color := range []string{"blue", ""} {
		srv := &Server{ApexDomain: "paste.test", Color: color}
		for _, host := range []string{"paste.test", "abc23456.paste.test", "10.0.0.1:8080"} {
			w := serve(srv, host, "/healthz")
			if w.Code != 200 || w.Body.String() != "ok\n" {
				t.Errorf("color %q host %s: status %d body %q, want 200 ok", color, host, w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Backend-Color"); got != color {
				t.Errorf("X-Backend-Color: got %q, want %q", got, color)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control: got %q, want no-store", got)
			}
		}
	}
}

// The landing page answers at the apex root, never on a slug's host, and no
// other path serves content.
func TestLanding_ApexRootOnly(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test", LandingHTML: []byte("<h1>landing</h1>")}
	w := serve(srv, "paste.test", "/")
	if w.Code != 200 || w.Body.String() != "<h1>landing</h1>" ||
		w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("apex root: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	for _, c := range []struct{ host, path string }{
		{"abc23456.paste.test", "/"},
		{"paste.test", "/p/abc23456"},
		{"paste.test", "/_hostthis/md.js"},
		{"paste.test", "/_solid/id"},
		{"abc23456.paste.test", "/api/rooms"},
	} {
		if w := serve(srv, c.host, c.path); w.Code != 404 {
			t.Errorf("%s%s = %d, want 404", c.host, c.path, w.Code)
		}
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
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			got, ok := s.slugFromHost(c.host)
			if ok != c.wantOK || string(got) != c.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
	if _, ok := (&Server{}).slugFromHost("abc23456.anything.com"); ok {
		t.Fatal("matched a host without an apex configured")
	}
}
