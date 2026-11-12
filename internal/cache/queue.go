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
	"strings"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

const (
	// enqueueMax is the most slugs the Worker takes in one enqueue.
	enqueueMax        = 100
	defaultBackoff    = time.Second
	defaultMaxBackoff = 30 * time.Second
)

// ErrClosed is returned by a purge after Shutdown.
var ErrClosed = errors.New("purge queue: shut down")

// Queue hands purges to the Worker's purge queue (docs/SPEC.md "Purge
// queue"), which owns the CDN credential, the pacing under the API's rate
// limit and the second purge that covers a read racing the first.
//
// A purge is held here only until the Worker has stored it: one loop sends
// what is waiting, removals ahead of changes, and offers a refused or failed
// enqueue again, with doubling backoff, until it is taken. What is still
// waiting when the process exits is lost; Shutdown sends it first.
type Queue struct {
	// Endpoint is the Worker's base URL. HTTPClient must carry the Worker
	// token for it.
	Endpoint   string
	HTTPClient *http.Client
	Logger     *log.Logger

	Backoff    time.Duration // wait after the first failure, doubled per failure
	MaxBackoff time.Duration // the most one wait grows to

	mu      sync.Mutex
	waiting map[domain.Slug]bool // slug -> urgent
	state   loopState
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// loopState is the send loop's lifecycle. The loop starts on the first purge,
// so a queue that never purges has no goroutine to stop.
type loopState int

const (
	loopIdle    loopState = iota // no purge yet; no loop
	loopRunning                  // loop started; accepting purges
	loopClosed                   // Shutdown called; refusing purges
)

// PurgePaste queues a purge of a slug whose content changed.
func (q *Queue) PurgePaste(slug domain.Slug) error { return q.add(slug, false) }

// PurgeRemoved queues a purge of a slug that no longer serves, ahead of every
// PurgePaste.
func (q *Queue) PurgeRemoved(slug domain.Slug) error { return q.add(slug, true) }

func (q *Queue) add(slug domain.Slug, urgent bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch q.state {
	case loopClosed:
		return ErrClosed
	case loopIdle:
		q.state = loopRunning
		q.waiting = map[domain.Slug]bool{}
		q.wake = make(chan struct{}, 1)
		q.stop = make(chan struct{})
		q.done = make(chan struct{})
		q.ctx, q.cancel = context.WithCancel(context.Background())
		go q.run()
	case loopRunning:
	}
	// A slug waiting twice is sent once, urgent if either asked.
	q.waiting[slug] = q.waiting[slug] || urgent
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// Shutdown refuses new purges and sends what is waiting, within ctx. Past the
// deadline the rest is dropped and logged.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	prev := q.state
	q.state = loopClosed
	q.mu.Unlock()
	if prev != loopRunning {
		return nil
	}
	close(q.stop)
	select {
	case <-q.done:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.cancel()
		<-q.done
		return ctx.Err()
	}
}

func (q *Queue) run() {
	defer close(q.done)
	backoff := q.backoff()
	for {
		slugs, urgent := q.take()
		if len(slugs) == 0 {
			select {
			case <-q.wake:
				continue
			case <-q.stop:
				if q.pending() == 0 {
					return
				}
				continue
			case <-q.ctx.Done():
				return
			}
		}
		err := q.enqueue(slugs, urgent)
		if err == nil {
			backoff = q.backoff()
			continue
		}
		var rej *rejection
		if errors.As(err, &rej) && rej.status == http.StatusBadRequest {
			// No retry can fix a request the Worker calls malformed.
			q.logf("cache: purge refused: %v (%v)", err, slugs)
			continue
		}
		q.giveBack(slugs, urgent)
		q.logf("cache: purge not queued, trying again in %s: %v", backoff, err)
		select {
		case <-time.After(backoff):
		case <-q.ctx.Done():
			q.logf("cache: %d purge(s) dropped at shutdown", q.pending())
			return
		}
		backoff = min(backoff*2, q.maxBackoff())
	}
}

func (q *Queue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// take removes up to one enqueue's worth of slugs of one urgency, the urgent
// ones first.
func (q *Queue) take() ([]string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, urgent := range []bool{true, false} {
		var out []string
		for slug, u := range q.waiting {
			if u == urgent && len(out) < enqueueMax {
				out = append(out, slug.String())
				delete(q.waiting, slug)
			}
		}
		if len(out) > 0 {
			return out, urgent
		}
	}
	return nil, false
}

func (q *Queue) giveBack(slugs []string, urgent bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, s := range slugs {
		slug := domain.Slug(s)
		q.waiting[slug] = q.waiting[slug] || urgent
	}
}

// rejection is an enqueue the Worker answered with a non-2xx status.
type rejection struct {
	status int
	body   string
}

func (r *rejection) Error() string {
	return fmt.Sprintf("purge enqueue rejected: %d %s", r.status, strings.TrimSpace(r.body))
}

func (q *Queue) enqueue(slugs []string, urgent bool) error {
	payload, err := json.Marshal(struct {
		Slugs  []string `json:"slugs"`
		Urgent bool     `json:"urgent"`
	}{slugs, urgent})
	if err != nil {
		return fmt.Errorf("purge queue: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(q.ctx, http.MethodPost,
		strings.TrimRight(q.Endpoint, "/")+"/purge/enqueue", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("purge queue: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &rejection{status: resp.StatusCode, body: string(b)}
}

func (q *Queue) backoff() time.Duration {
	if q.Backoff > 0 {
		return q.Backoff
	}
	return defaultBackoff
}

func (q *Queue) maxBackoff() time.Duration {
	if q.MaxBackoff > 0 {
		return q.MaxBackoff
	}
	return defaultMaxBackoff
}

func (q *Queue) client() *http.Client {
	if q.HTTPClient != nil {
		return q.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (q *Queue) logf(format string, args ...any) {
	if q.Logger != nil {
		q.Logger.Printf(format, args...)
	}
}
