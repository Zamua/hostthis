package cache

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// purgeCall is one request the fake purge API received.
type purgeCall struct {
	at         time.Time
	auth, path string
	body       map[string][]string
}

// purgeAPI is a fake purge endpoint answering each call with the next status
// in replies (the last one repeats). A call blocks while hold is set.
type purgeAPI struct {
	mu      sync.Mutex
	calls   []purgeCall
	replies []int
	hold    chan struct{}
	arrived chan struct{}
}

func (p *purgeAPI) start(t *testing.T, replies ...int) *http.Client {
	t.Helper()
	p.replies = replies
	p.arrived = make(chan struct{}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		call := purgeCall{at: time.Now(), auth: r.Header.Get("Authorization"), path: r.URL.Path}
		if err := json.Unmarshal(b, &call.body); err != nil {
			t.Errorf("purge body %q: %v", b, err)
		}
		p.mu.Lock()
		p.calls = append(p.calls, call)
		status := p.replies[min(len(p.calls), len(p.replies))-1]
		hold := p.hold
		p.mu.Unlock()
		p.arrived <- struct{}{}
		if hold != nil {
			<-hold
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":` + strconv.FormatBool(status/100 == 2) + `}`))
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: redirectTransport{base: srv.URL}}
}

func (p *purgeAPI) recorded() []purgeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// syncBuf is a log sink safe for the purge loop's goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestCloudflare(client *http.Client, logs io.Writer) *Cloudflare {
	return &Cloudflare{
		ZoneID: "zone123", Token: "tok-456", Apex: "paste.test", Mode: "subdomain",
		HTTPClient: client, Logger: log.New(logs, "", 0),
		Interval: 20 * time.Millisecond, Backoff: time.Millisecond, Attempts: 3,
	}
}

func shutdown(t *testing.T, c *Cloudflare) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func hosts(slugs ...string) []string {
	out := make([]string, len(slugs))
	for i, s := range slugs {
		out[i] = s + ".paste.test"
	}
	return out
}

func TestCloudflare_PurgesByHostname(t *testing.T) {
	var api purgeAPI
	c := newTestCloudflare(api.start(t, http.StatusOK), io.Discard)
	if err := c.PurgePaste("abc12345"); err != nil {
		t.Fatalf("PurgePaste: %v", err)
	}
	shutdown(t, c)
	calls := api.recorded()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].auth != "Bearer tok-456" || !strings.HasSuffix(calls[0].path, "/zones/zone123/purge_cache") {
		t.Errorf("auth %q path %q", calls[0].auth, calls[0].path)
	}
	if want := map[string][]string{"hosts": hosts("abc12345")}; !reflect.DeepEqual(calls[0].body, want) {
		t.Errorf("purge body = %v, want %v", calls[0].body, want)
	}
}

func TestCloudflare_PathModePurgesByPrefix(t *testing.T) {
	var api purgeAPI
	c := newTestCloudflare(api.start(t, http.StatusOK), io.Discard)
	c.Apex, c.Mode = "localhost:8080", "path"
	_ = c.PurgePaste("abc12345")
	shutdown(t, c)
	calls := api.recorded()
	if want := map[string][]string{"prefixes": {"localhost/p/abc12345"}}; len(calls) != 1 || !reflect.DeepEqual(calls[0].body, want) {
		t.Fatalf("calls = %+v, want one with %v", calls, want)
	}
}

// Purges queued while a call is in flight go out together in the next call,
// each slug once.
func TestCloudflare_CoalescesQueuedPurges(t *testing.T) {
	var api purgeAPI
	api.hold = make(chan struct{})
	c := newTestCloudflare(api.start(t, http.StatusOK), io.Discard)
	_ = c.PurgePaste("aaaaaaaa")
	<-api.arrived
	for _, s := range []domain.Slug{"bbbbbbbb", "cccccccc", "bbbbbbbb"} {
		_ = c.PurgePaste(s)
	}
	api.mu.Lock()
	close(api.hold)
	api.hold = nil
	api.mu.Unlock()
	shutdown(t, c)
	calls := api.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	got := slices.Sorted(slices.Values(calls[1].body["hosts"]))
	if want := hosts("bbbbbbbb", "cccccccc"); !slices.Equal(got, want) {
		t.Fatalf("second call hosts = %v, want %v", got, want)
	}
}

// A backlog drains in calls of at most 30 hosts, spaced by the interval.
func TestCloudflare_BatchesAndSpacesCalls(t *testing.T) {
	var api purgeAPI
	api.hold = make(chan struct{})
	c := newTestCloudflare(api.start(t, http.StatusOK), io.Discard)
	_ = c.PurgePaste("aaaaaaaa")
	<-api.arrived
	for i := range 65 {
		_ = c.PurgePaste(domain.Slug(strings.Repeat("b", 5) + strconv.Itoa(100+i)))
	}
	api.mu.Lock()
	close(api.hold)
	api.hold = nil
	api.mu.Unlock()
	shutdown(t, c)
	calls := api.recorded()
	var sizes []int
	for _, call := range calls {
		sizes = append(sizes, len(call.body["hosts"]))
	}
	if !slices.Equal(sizes, []int{1, 30, 30, 5}) {
		t.Fatalf("batch sizes = %v, want [1 30 30 5]", sizes)
	}
	for i := 1; i < len(calls); i++ {
		if gap := calls[i].at.Sub(calls[i-1].at); gap < c.Interval {
			t.Errorf("call %d came %v after the previous, want at least %v", i, gap, c.Interval)
		}
	}
}

// 429 and 5xx are retried with backoff until one succeeds.
func TestCloudflare_RetriesRateLimitsAndServerErrors(t *testing.T) {
	var api purgeAPI
	var logs syncBuf
	c := newTestCloudflare(api.start(t, http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusOK), &logs)
	_ = c.PurgePaste("abc12345")
	shutdown(t, c)
	if n := len(api.recorded()); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
	if !strings.Contains(logs.String(), "purged") {
		t.Fatalf("success not logged: %q", logs.String())
	}
}

// A batch that keeps failing is given up after its attempts and logged; a
// refusal that retrying cannot fix is not retried.
func TestCloudflare_LogsFailures(t *testing.T) {
	for _, tc := range []struct {
		status, calls int
	}{
		{http.StatusInternalServerError, 3},
		{http.StatusForbidden, 1},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			var api purgeAPI
			var logs syncBuf
			c := newTestCloudflare(api.start(t, tc.status), &logs)
			_ = c.PurgePaste("abc12345")
			shutdown(t, c)
			if n := len(api.recorded()); n != tc.calls {
				t.Fatalf("calls = %d, want %d", n, tc.calls)
			}
			if out := logs.String(); !strings.Contains(out, strconv.Itoa(tc.status)) || !strings.Contains(out, "abc12345.paste.test") {
				t.Fatalf("failure not logged with its status and hosts: %q", out)
			}
		})
	}
}

// Shutdown stops a loop stuck in a retry wait at its deadline, and a purge
// after shutdown is refused.
func TestCloudflare_ShutdownIsBounded(t *testing.T) {
	var api purgeAPI
	c := newTestCloudflare(api.start(t, http.StatusServiceUnavailable), io.Discard)
	c.Backoff = time.Hour
	_ = c.PurgePaste("abc12345")
	<-api.arrived
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown past its deadline returned nil")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Shutdown took %v", took)
	}
	if err := c.PurgePaste("abc12345"); err == nil {
		t.Fatal("PurgePaste after Shutdown returned nil")
	}
}

func TestCloudflare_MissingConfig(t *testing.T) {
	c := &Cloudflare{Apex: "paste.test"}
	if err := c.PurgePaste("abc12345"); err == nil {
		t.Fatal("expected error for missing config")
	}
}

// redirectTransport rewrites any URL to point at base, preserving path and method.
// Lets the test point Cloudflare's hard-coded api.cloudflare.com URL at httptest.
type redirectTransport struct{ base string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, t.base+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}
