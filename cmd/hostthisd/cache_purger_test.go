package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/cache"
	"github.com/Zamua/hostthis/internal/service"
)

type purgeRecorder struct {
	mu    sync.Mutex
	calls []*http.Request
	files [][]string
}

func (p *purgeRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string][]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.calls = append(p.calls, r)
	p.files = append(p.files, append(body["hosts"], body["prefixes"]...))
	p.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true}`)), Header: http.Header{}}, nil
}

// The cloudflare backend purges a slug's host through the zone's API with the
// token, shutdown sends what is queued, and purges after shutdown are refused.
func TestBuildCachePurgerSendsCloudflarePurges(t *testing.T) {
	rec := &purgeRecorder{}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	t.Cleanup(func() { http.DefaultTransport = orig })
	t.Setenv("HOSTTHIS_CACHE_BACKEND", "cloudflare")
	t.Setenv("HOSTTHIS_CF_ZONE_ID", "zone")
	t.Setenv("HOSTTHIS_CF_PURGE_TOKEN", "token")
	purger, shutdown := buildCachePurger(log.New(io.Discard, "", 0), "paste.test", "subdomain")

	if err := purger.PurgePaste("abcd2345"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("purge requests = %d, want 1", len(rec.calls))
	}
	r := rec.calls[0]
	if r.URL.String() != "https://api.cloudflare.com/client/v4/zones/zone/purge_cache" || r.Header.Get("Authorization") != "Bearer token" {
		t.Fatalf("purge sent to %s with %q", r.URL, r.Header.Get("Authorization"))
	}
	if got := rec.files[0]; len(got) != 1 || got[0] != "abcd2345.paste.test" {
		t.Fatalf("purged %v, want the slug's host", got)
	}
	if err := purger.PurgePaste("abcd2345"); !errors.Is(err, cache.ErrClosed) {
		t.Fatalf("purge after shutdown = %v, want ErrClosed", err)
	}
}

func TestParseCDNTTL(t *testing.T) {
	purging := cache.NewRepurging(cache.Noop{}, time.Hour)
	t.Cleanup(func() { _ = purging.Shutdown(context.Background()) })
	cases := []struct {
		name    string
		raw     string
		purger  service.CachePurger
		want    time.Duration
		wantErr bool
	}{
		{"unset", "", cache.Noop{}, 0, false},
		{"set with a purger", "1h", purging, time.Hour, false},
		{"a TTL nothing can purge", "1h", cache.Noop{}, 0, true},
		{"zero without a purger", "0s", cache.Noop{}, 0, false},
		{"not a duration", "soon", purging, 0, true},
		{"negative", "-1h", purging, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCDNTTL(c.raw, c.purger)
			if got != c.want || (err != nil) != c.wantErr {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, c.want, c.wantErr)
			}
		})
	}
}
