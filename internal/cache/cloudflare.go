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
	"strconv"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Purge pacing defaults. Hostname and prefix purges carry tighter API rate
// limits than URL purges below the enterprise plan, and accept at most 30
// entries per call there.
const (
	DefaultPurgeInterval = 2 * time.Second
	DefaultPurgeBatch    = 30
	defaultBackoff       = time.Second
	defaultAttempts      = 5
)

// ErrClosed is returned by PurgePaste after Shutdown.
var ErrClosed = errors.New("cloudflare purger: shut down")

// Cloudflare purges a paste's cached responses from the Cloudflare edge, by
// hostname (or path prefix in path mode): every URL the slug answers, and any
// Cache API entry a Worker stored under that host. The token needs only the
// zone "Cache:Purge" permission.
//
// PurgePaste only queues the slug's scope. One loop sends the queue in calls of
// at most MaxBatch entries, no closer together than Interval, so a burst of
// writes costs a few calls instead of one per write, and retries a 429, 5xx or
// transport failure with doubling backoff. A batch still failing after Attempts
// calls is logged and dropped: the edge TTL bounds it.
type Cloudflare struct {
	ZoneID string
	Token  string

	Apex string // apex domain, e.g. "hostthis.dev"
	Mode string // url mode: "subdomain" (prod) | "path" (dev)

	Logger *log.Logger

	// Defaults to a 5-second-timeout client.
	HTTPClient *http.Client

	Interval time.Duration // least time between calls; DefaultPurgeInterval when zero
	MaxBatch int           // entries per call; DefaultPurgeBatch when zero
	Backoff  time.Duration // wait before the first retry, doubled per retry
	Attempts int           // calls per batch before it is dropped

	mu      sync.Mutex
	queue   []scope
	queued  map[scope]bool
	started bool
	closed  bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// PurgePaste queues everything cached under the slug's scope (slugScope) for
// the next call. A scope already queued is not queued twice.
func (c *Cloudflare) PurgePaste(slug domain.Slug) error {
	if c.ZoneID == "" || c.Token == "" {
		return errors.New("cloudflare purger: missing zone id or token")
	}
	sc := slugScope(c.Apex, c.Mode, slug)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if !c.started {
		c.started = true
		c.queued = map[scope]bool{}
		c.wake = make(chan struct{}, 1)
		c.stop = make(chan struct{})
		c.done = make(chan struct{})
		c.ctx, c.cancel = context.WithCancel(context.Background())
		go c.run()
	}
	if !c.queued[sc] {
		c.queued[sc] = true
		c.queue = append(c.queue, sc)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

// Shutdown refuses new purges and sends what is queued, within ctx. Past the
// deadline the loop is cancelled and the rest dropped.
func (c *Cloudflare) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	started, already := c.started, c.closed
	c.closed = true
	c.mu.Unlock()
	if !started || already {
		return nil
	}
	close(c.stop)
	select {
	case <-c.done:
		c.cancel()
		return nil
	case <-ctx.Done():
		c.cancel()
		<-c.done
		return ctx.Err()
	}
}

func (c *Cloudflare) run() {
	defer close(c.done)
	var last time.Time
	for {
		if c.pending() == 0 {
			select {
			case <-c.wake:
				continue
			case <-c.stop:
				if c.pending() == 0 {
					return
				}
			case <-c.ctx.Done():
				return
			}
		}
		if !c.sleep(time.Until(last.Add(c.interval()))) {
			return
		}
		batch := c.take()
		last = time.Now()
		if !c.send(batch) {
			return
		}
	}
}

func (c *Cloudflare) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// take dequeues up to one batch.
func (c *Cloudflare) take() []scope {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(len(c.queue), c.maxBatch())
	batch := c.queue[:n:n]
	c.queue = c.queue[n:]
	for _, sc := range batch {
		delete(c.queued, sc)
	}
	return batch
}

// send makes one purge call for batch, retrying what a retry can fix. False
// means the loop was cancelled.
func (c *Cloudflare) send(batch []scope) bool {
	body := purgeBody(batch)
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	for attempt := 1; ; attempt++ {
		retryAfter, err := c.post(body)
		if err == nil {
			// Low volume (writes, coalesced), so success is logged too.
			c.logf("cache: purged %v", body)
			return true
		}
		if c.ctx.Err() != nil {
			c.logf("cache: purge abandoned at shutdown: %v (%v)", err, body)
			return false
		}
		var rej *rejection
		if attempt >= attempts || (errors.As(err, &rej) && !rej.retryable()) {
			c.logf("cache: purge failed after %d attempt(s): %v (%v)", attempt, err, body)
			return true
		}
		if !c.sleep(max(backoff, retryAfter)) {
			c.logf("cache: purge abandoned at shutdown: %v (%v)", err, body)
			return false
		}
		backoff *= 2
	}
}

// rejection is a purge call the API answered with a non-2xx status.
type rejection struct {
	status int
	body   string
}

func (r *rejection) Error() string {
	return fmt.Sprintf("cloudflare purge rejected: %d %s", r.status, r.body)
}

func (r *rejection) retryable() bool {
	return r.status == http.StatusTooManyRequests || r.status >= 500
}

// post makes one call. It returns the Retry-After the API asked for, if any.
func (c *Cloudflare) post(body map[string][]string) (time.Duration, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("cloudflare purger: marshal: %w", err)
	}
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", c.ZoneID)
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("cloudflare purger: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return 0, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var retryAfter time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	return retryAfter, &rejection{status: resp.StatusCode, body: string(b)}
}

// purgeBody names a batch's hosts, or its prefixes in path mode. A deployment
// has one URL mode, so a batch never mixes the two.
func purgeBody(batch []scope) map[string][]string {
	var hosts, prefixes []string
	for _, sc := range batch {
		if sc.Prefix != "" {
			prefixes = append(prefixes, sc.Prefix)
		} else {
			hosts = append(hosts, sc.Host)
		}
	}
	if len(prefixes) > 0 {
		return map[string][]string{"prefixes": prefixes}
	}
	return map[string][]string{"hosts": hosts}
}

// sleep waits d, false when the loop is cancelled first.
func (c *Cloudflare) sleep(d time.Duration) bool {
	if d <= 0 {
		return c.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

func (c *Cloudflare) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultPurgeInterval
}

func (c *Cloudflare) maxBatch() int {
	if c.MaxBatch > 0 {
		return c.MaxBatch
	}
	return DefaultPurgeBatch
}

func (c *Cloudflare) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// logf drops messages when no Logger is wired: purge is best-effort.
func (c *Cloudflare) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}
