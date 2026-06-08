package cache

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudflare_PurgeURLs_Success(t *testing.T) {
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
		HTTPClient: &http.Client{
			Transport: redirectTransport{base: srv.URL},
		},
	}
	if err := c.PurgeURLs([]string{"https://paste.test/p/abc", "https://abc.paste.test/"}); err != nil {
		t.Fatalf("PurgeURLs returned %v, want nil", err)
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
	if len(parsed.Files) != 2 || parsed.Files[0] != "https://paste.test/p/abc" {
		t.Errorf("body files: got %v", parsed.Files)
	}
}

func TestCloudflare_PurgeURLs_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":10000,"message":"invalid token"}]}`))
	}))
	defer srv.Close()

	var logbuf strings.Builder
	c := &Cloudflare{
		ZoneID: "zone",
		Token:  "bad",
		Logger: log.New(&logbuf, "", 0),
		HTTPClient: &http.Client{
			Transport: redirectTransport{base: srv.URL},
		},
	}
	err := c.PurgeURLs([]string{"https://paste.test/p/abc"})
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

func TestCloudflare_PurgeURLs_Empty(t *testing.T) {
	c := &Cloudflare{ZoneID: "z", Token: "t"}
	if err := c.PurgeURLs(nil); err != nil {
		t.Errorf("empty urls: got %v, want nil", err)
	}
}

func TestCloudflare_PurgeURLs_MissingConfig(t *testing.T) {
	c := &Cloudflare{} // no zone, no token
	err := c.PurgeURLs([]string{"https://x.example/"})
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
