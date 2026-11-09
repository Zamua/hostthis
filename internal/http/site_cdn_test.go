package http

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestSetCDNCacheControl(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration
		size int
		want string
	}{
		{"off by default", 0, 10, ""},
		{"small file", time.Hour, 10, "public, max-age=3600"},
		{"at the limit", time.Hour, cdnCacheMaxBytes, "public, max-age=3600"},
		{"over the limit", time.Hour, cdnCacheMaxBytes + 1, ""},
		{"unknown size", time.Hour, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s := &Server{CDNTTL: c.ttl}
			s.setCDNCacheControl(w.Header(), domain.ManifestEntry{DecodedSize: c.size})
			if got := w.Header().Get("CDN-Cache-Control"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A browser must keep revalidating whatever a CDN is told, on a 200 and a 304.
func TestSite_CDNTTL_LeavesBrowsersRevalidating(t *testing.T) {
	srv := buildSiteServer(t)
	srv.CDNTTL = time.Hour
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{Key: "key-index", Size: 10, DecodedSize: 10, ContentType: "text/html; charset=utf-8"})
	now := time.Now().UTC()
	srv.Sites = stubSiteReader{s: domain.Site{Slug: "abc23456", Identity: "key:test", Manifest: m, CreatedAt: now, UpdatedAt: now}}
	mux := srv.Handler()

	get := func(etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = "abc23456.paste.test"
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	first := get("")
	second := get(first.Header().Get("ETag"))
	if first.Code != 200 || second.Code != 304 {
		t.Fatalf("codes: %d then %d, want 200 then 304", first.Code, second.Code)
	}
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		if got := w.Header().Get("Cache-Control"); got != "public, no-cache" {
			t.Fatalf("Cache-Control: %q", got)
		}
		if got := w.Header().Get("CDN-Cache-Control"); got != "public, max-age=3600" {
			t.Fatalf("CDN-Cache-Control: %q", got)
		}
	}
}

// RFC 9213 lets the CDN header outrank Cache-Control, so a failure carrying it
// would be stored for the whole TTL.
func TestSite_CDNTTL_NeverOnAFailure(t *testing.T) {
	srv := buildSiteServer(t)
	srv.CDNTTL = time.Hour
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{Key: "key-index", Size: 13, DecodedSize: 13, ContentType: "text/html; charset=utf-8"})
	m.Add("gone.js", domain.ManifestEntry{Key: "key-missing", Size: 3, DecodedSize: 3, ContentType: "text/javascript"})
	now := time.Now().UTC()
	srv.Sites = stubSiteReader{s: domain.Site{Slug: "abc23456", Identity: "key:test", Manifest: m, CreatedAt: now, UpdatedAt: now}}
	mux := srv.Handler()

	cases := []struct {
		name, path, rng string
		want            int
	}{
		{"blob read fails", "/gone.js", "", 500},
		{"range past the end", "/", "bytes=500-", 416},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.path, nil)
			r.Host = "abc23456.paste.test"
			if c.rng != "" {
				r.Header.Set("Range", c.rng)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("code %d, want %d", w.Code, c.want)
			}
			if got := w.Header().Get("CDN-Cache-Control"); got != "" {
				t.Fatalf("CDN-Cache-Control on a %d: %q", w.Code, got)
			}
		})
	}
}
