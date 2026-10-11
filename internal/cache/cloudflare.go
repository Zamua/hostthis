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

// ErrPurgeRejected is returned when the edge answers a purge with a non-2xx
// status; the wrapped message carries the status and body.
var ErrPurgeRejected = errors.New("cloudflare purge rejected")

// Cloudflare purges a paste's cached responses from the Cloudflare edge, by
// hostname (or path prefix in path mode): every URL the slug answers, and any
// Cache API entry a Worker stored under that host. The token needs only the
// zone "Cache:Purge" permission.
type Cloudflare struct {
	ZoneID string
	Token  string

	Apex string // apex domain, e.g. "hostthis.dev"
	Mode string // url mode: "subdomain" (prod) | "path" (dev)

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

// PurgePaste purges everything cached under the slug's scope (slugScope).
func (c *Cloudflare) PurgePaste(slug domain.Slug) error {
	if c.ZoneID == "" || c.Token == "" {
		return errors.New("cloudflare purger: missing zone id or token")
	}
	sc := slugScope(c.Apex, c.Mode, slug)
	body := map[string][]string{"hosts": {sc.Host}}
	if sc.Prefix != "" {
		body = map[string][]string{"prefixes": {sc.Prefix}}
	}
	payload, err := json.Marshal(body)
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
		c.logf("cloudflare purge http: %v (%v)", err, body)
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("%w: %d %s", ErrPurgeRejected, resp.StatusCode, string(b))
		c.logf("%v (%v)", err, body)
		return err
	}
	// Low-volume (one per mutation), so success is logged too.
	c.logf("cache: purged %v", body)
	return nil
}

// logf drops messages when no Logger is wired: purge is best-effort.
func (c *Cloudflare) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}
