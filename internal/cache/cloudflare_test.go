package cache

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// pasteCacheURLs must return EVERY cache key a paste is reachable at. The
// markdown shell fetches "?raw=1" as a separate cache entry, so purging
// only the base URL would leave an edited markdown paste serving stale
// content - this test pins that both variants are produced.
func TestPasteCacheURLs_Variants(t *testing.T) {
	slug := domain.Slug("abc12345")
	cases := []struct {
		name, scheme, apex, mode string
		want                     []string
	}{
		{
			name: "subdomain (prod)", scheme: "https", apex: "hostthis.dev", mode: "subdomain",
			want: []string{
				"https://abc12345.hostthis.dev/",
				"https://abc12345.hostthis.dev/?raw=1",
			},
		},
		{
			name: "empty mode defaults to subdomain", scheme: "https", apex: "hostthis.dev", mode: "",
			want: []string{
				"https://abc12345.hostthis.dev/",
				"https://abc12345.hostthis.dev/?raw=1",
			},
		},
		{
			name: "path (dev)", scheme: "http", apex: "localhost:8080", mode: "path",
			want: []string{
				"http://localhost:8080/p/abc12345",
				"http://localhost:8080/p/abc12345?raw=1",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pasteCacheURLs(tc.scheme, tc.apex, tc.mode, slug)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pasteCacheURLs:\n got  %v\n want %v", got, tc.want)
			}
		})
	}
}

func TestCloudflare_PurgePaste_Success(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c := &Cloudflare{
		ZoneID: "zone123",
		Token:  "tok-456",
		Scheme: "https",
		Apex:   "paste.test",
		Mode:   "subdomain",
		HTTPClient: &http.Client{
			Transport: redirectTransport{base: srv.URL},
		},
	}
	if err := c.PurgePaste(domain.Slug("abc12345")); err != nil {
		t.Fatalf("PurgePaste returned %v, want nil", err)
	}
	if gotAuth != "Bearer tok-456" {
		t.Errorf("auth header: got %q, want Bearer tok-456", gotAuth)
	}
	if !strings.Contains(gotPath, "zone123/purge_cache") {
		t.Errorf("path: got %q, want contains zone123/purge_cache", gotPath)
	}
	var parsed struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	want := []string{
		"https://abc12345.paste.test/",
		"https://abc12345.paste.test/?raw=1",
	}
	if !reflect.DeepEqual(parsed.Files, want) {
		t.Errorf("purged files:\n got  %v\n want %v", parsed.Files, want)
	}
}

func TestCloudflare_PurgePaste_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":10000,"message":"invalid token"}]}`))
	}))
	defer srv.Close()

	var logbuf strings.Builder
	c := &Cloudflare{
		ZoneID: "zone", Token: "bad", Scheme: "https", Apex: "paste.test", Mode: "subdomain",
		Logger: log.New(&logbuf, "", 0),
		HTTPClient: &http.Client{
			Transport: redirectTransport{base: srv.URL},
		},
	}
	err := c.PurgePaste(domain.Slug("abc12345"))
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "non-2xx") {
		t.Errorf("error %q should mention non-2xx", err.Error())
	}
	if !strings.Contains(logbuf.String(), "non-2xx") {
		t.Errorf("expected log line on failure, got %q", logbuf.String())
	}
}

func TestCloudflare_purgeURLs_Empty(t *testing.T) {
	c := &Cloudflare{ZoneID: "z", Token: "t"}
	if err := c.purgeURLs(nil); err != nil {
		t.Errorf("empty urls: got %v, want nil", err)
	}
}

func TestCloudflare_purgeURLs_MissingConfig(t *testing.T) {
	c := &Cloudflare{} // no zone, no token
	err := c.purgeURLs([]string{"https://x.example/"})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

// redirectTransport rewrites any URL to point at base, preserving path and method.
// Lets the test point Cloudflare's hard-coded api.cloudflare.com URL at httptest.
type redirectTransport struct{ base string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequest(req.Method, t.base+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}
