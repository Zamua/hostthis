package cache

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// A purge covers the slug's whole host, so every URL it answers goes at once;
// path mode, which has no per-slug host, covers its path prefix instead.
func TestSlugScope(t *testing.T) {
	slug := domain.Slug("abc12345")
	cases := []struct {
		name, apex, mode string
		want             scope
	}{
		{"subdomain", "hostthis.dev", "subdomain", scope{Host: "abc12345.hostthis.dev"}},
		{"empty mode is subdomain", "hostthis.dev", "", scope{Host: "abc12345.hostthis.dev"}},
		{"port dropped", "paste.test:8443", "subdomain", scope{Host: "abc12345.paste.test"}},
		{"path", "localhost:8080", "path", scope{Prefix: "localhost/p/abc12345"}},
		{"path, any case", "paste.test", "PATH", scope{Prefix: "paste.test/p/abc12345"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slugScope(tc.apex, tc.mode, slug); got != tc.want {
				t.Errorf("slugScope = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// purgeServer records the one purge request it receives.
type purgeServer struct {
	auth, path string
	body       map[string][]string
}

func (p *purgeServer) start(t *testing.T, status int, reply string) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.auth = r.Header.Get("Authorization")
		p.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p.body); err != nil {
			t.Errorf("purge body %q: %v", b, err)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: redirectTransport{base: srv.URL}}
}

func TestCloudflare_PurgePaste_ByHostname(t *testing.T) {
	var got purgeServer
	c := &Cloudflare{
		ZoneID: "zone123", Token: "tok-456", Apex: "paste.test", Mode: "subdomain",
		HTTPClient: got.start(t, http.StatusOK, `{"success":true,"result":{"id":"abc"}}`),
	}
	if err := c.PurgePaste(domain.Slug("abc12345")); err != nil {
		t.Fatalf("PurgePaste returned %v, want nil", err)
	}
	if got.auth != "Bearer tok-456" {
		t.Errorf("auth header: got %q, want Bearer tok-456", got.auth)
	}
	if !strings.HasSuffix(got.path, "/zones/zone123/purge_cache") {
		t.Errorf("path: got %q, want the zone's purge_cache", got.path)
	}
	if want := map[string][]string{"hosts": {"abc12345.paste.test"}}; !reflect.DeepEqual(got.body, want) {
		t.Errorf("purge body = %v, want %v", got.body, want)
	}
}

func TestCloudflare_PurgePaste_PathModeByPrefix(t *testing.T) {
	var got purgeServer
	c := &Cloudflare{
		ZoneID: "zone", Token: "tok", Apex: "localhost:8080", Mode: "path",
		HTTPClient: got.start(t, http.StatusOK, `{"success":true}`),
	}
	if err := c.PurgePaste(domain.Slug("abc12345")); err != nil {
		t.Fatalf("PurgePaste returned %v, want nil", err)
	}
	if want := map[string][]string{"prefixes": {"localhost/p/abc12345"}}; !reflect.DeepEqual(got.body, want) {
		t.Errorf("purge body = %v, want %v", got.body, want)
	}
}

func TestCloudflare_PurgePaste_Non2xx(t *testing.T) {
	var got purgeServer
	var logbuf strings.Builder
	c := &Cloudflare{
		ZoneID: "zone", Token: "bad", Apex: "paste.test", Mode: "subdomain",
		Logger:     log.New(&logbuf, "", 0),
		HTTPClient: got.start(t, http.StatusForbidden, `{"errors":[{"code":10000,"message":"invalid token"}]}`),
	}
	err := c.PurgePaste(domain.Slug("abc12345"))
	if !errors.Is(err, ErrPurgeRejected) {
		t.Fatalf("PurgePaste on 403 = %v, want ErrPurgeRejected", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should carry the status", err.Error())
	}
	if !strings.Contains(logbuf.String(), "403") {
		t.Errorf("expected the failure logged with its status, got %q", logbuf.String())
	}
}

func TestCloudflare_PurgePaste_MissingConfig(t *testing.T) {
	c := &Cloudflare{Apex: "paste.test"} // no zone, no token
	if err := c.PurgePaste(domain.Slug("abc12345")); err == nil {
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
