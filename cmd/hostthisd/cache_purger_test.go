package main

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/Zamua/hostthis/internal/cache"
)

// The cloudflare backend purges again after the repurge delay, and its
// shutdown cancels what is still scheduled.
func TestBuildCachePurgerRepurgesCloudflare(t *testing.T) {
	t.Setenv("HOSTTHIS_CACHE_BACKEND", "cloudflare")
	t.Setenv("HOSTTHIS_CF_ZONE_ID", "zone")
	t.Setenv("HOSTTHIS_CF_PURGE_TOKEN", "token")
	purger, shutdown := buildCachePurger(log.New(io.Discard, "", 0), "paste.test", "subdomain")
	if _, ok := purger.(*cache.Repurging); !ok {
		t.Fatalf("cloudflare purger is %T, want *cache.Repurging", purger)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
