package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Cloudflare purges a paste's cached representations from the Cloudflare CDN
// edge. The token needs only the zone "Cache:Purge" permission.
//
// Scheme/Apex/Mode map a slug to its public URL variants (pasteCacheURLs).
// That policy lives in the adapter; the service layer only names the slug.
type Cloudflare struct {
	ZoneID string
	Token  string

	Scheme string // public scheme, e.g. "https"
	Apex   string // apex domain, e.g. "hostthis.dev"
	Mode   string // url mode: "subdomain" (prod) | "path" (dev)

	Logger *log.Logger

	// Defaults to a 5-second-timeout client.
	HTTPClient *http.Client
}

func (c *Cloudflare) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// PurgePaste purges every CDN cache key the slug is reachable at. The variant
// policy is provider-agnostic (pasteCacheURLs in urls.go); this adapter only
// submits the list.
func (c *Cloudflare) PurgePaste(slug domain.Slug) error {
	return c.purgeURLs(pasteCacheURLs(c.Scheme, c.Apex, c.Mode, slug))
}

func (c *Cloudflare) purgeURLs(urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	if c.ZoneID == "" || c.Token == "" {
		return errors.New("cloudflare purger: missing zone id or token")
	}
	payload, err := json.Marshal(map[string]any{"files": urls})
	if err != nil {
		return fmt.Errorf("cloudflare purger: marshal: %w", err)
	}
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", c.ZoneID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("cloudflare purger: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		c.logf("cloudflare purge http: %v (urls=%v)", err, urls)
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("cloudflare purge non-2xx: %d %s", resp.StatusCode, string(body))
		c.logf("%v (urls=%v)", err, urls)
		return err
	}
	// Low-volume (one per edit/delete/pin/unpin), so success is logged too:
	// it keeps the integration observable before any edge caching exists.
	c.logf("cache: purged %v", urls)
	return nil
}

// logf drops messages when no Logger is wired: purge is best-effort.
func (c *Cloudflare) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}
