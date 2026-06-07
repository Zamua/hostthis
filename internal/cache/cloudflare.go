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
)

// Cloudflare purges paste URLs from the Cloudflare CDN edge by POSTing
// to the zone's purge_cache endpoint. The token only needs the
// "Cache:Purge" zone permission — narrowest scope possible.
type Cloudflare struct {
	ZoneID string
	Token  string
	Logger *log.Logger

	// Optional override for tests; defaults to a 5-second-timeout client.
	HTTPClient *http.Client
}

func (c *Cloudflare) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (c *Cloudflare) PurgeURLs(urls []string) error {
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
		c.logErr("cloudflare purge http: %v (urls=%v)", err, urls)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("cloudflare purge non-2xx: %d %s", resp.StatusCode, string(body))
		c.logErr("%v (urls=%v)", err, urls)
		return err
	}
	return nil
}

func (c *Cloudflare) logErr(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}
