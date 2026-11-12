package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/cache"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// The worker backend enqueues a slug's purge at the Worker with the Worker
// token, shutdown sends what is waiting, and purges after shutdown are refused.
func TestBuildCachePurgerEnqueuesAtTheWorker(t *testing.T) {
	type call struct {
		path, auth string
		body       struct {
			Slugs  []string `json:"slugs"`
			Urgent bool     `json:"urgent"`
		}
	}
	calls := make(chan call, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		calls <- c
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOSTTHIS_CACHE_BACKEND", "worker")
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", srv.URL)
	t.Setenv("HOSTTHIS_WORKER_TOKEN", "token")
	purger, shutdown, err := buildCachePurger(log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if err := purger.PurgeRemoved("abcd2345"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	c := <-calls
	if c.path != "/purge/enqueue" || c.auth != "Bearer token" {
		t.Fatalf("enqueue sent to %s with %q", c.path, c.auth)
	}
	if len(c.body.Slugs) != 1 || c.body.Slugs[0] != "abcd2345" || !c.body.Urgent {
		t.Fatalf("enqueued %+v, want the slug, urgent", c.body)
	}
	if err := purger.PurgePaste("abcd2345"); !errors.Is(err, cache.ErrClosed) {
		t.Fatalf("purge after shutdown = %v, want ErrClosed", err)
	}
}

func TestBuildCachePurgerRefusesUnknownBackends(t *testing.T) {
	t.Setenv("HOSTTHIS_CACHE_BACKEND", "cloudflare")
	if _, _, err := buildCachePurger(log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("an unknown backend was accepted")
	}
}

func TestPurgeExempt(t *testing.T) {
	if purgeExempt(" , ") != nil {
		t.Fatal("an empty list exempts someone")
	}
	listed, _ := domain.OwnerFromKeyFingerprint("SHA256:aaaa")
	other, _ := domain.OwnerFromKeyFingerprint("SHA256:bbbb")
	exempt := purgeExempt("key:SHA256:cccc, key:SHA256:aaaa")
	if !exempt(listed) || exempt(other) {
		t.Fatalf("exempt(listed)=%v exempt(other)=%v", exempt(listed), exempt(other))
	}
}

func TestParseCDNTTL(t *testing.T) {
	purging := &cache.Queue{}
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
