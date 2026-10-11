package cache

import (
	"context"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RepurgeDelay is how long after a slug's last purge it is purged again. The
// Worker abandons an edge-cache put that completes more than 10 seconds after
// its read resolved, and such a read resolved before the write that purged,
// so a second purge this long after the first also drops a put that raced it.
const RepurgeDelay = 15 * time.Second

// Purger is the port Repurging decorates (service.CachePurger).
type Purger interface {
	PurgePaste(slug domain.Slug) error
}

// Repurging purges a slug at once and again Delay after its last purge. A read
// in flight across a write can still store the old response at the edge after
// the first purge; the second drops it. Writes in a burst share one delayed
// purge, timed from the last of them.
type Repurging struct {
	inner Purger
	delay time.Duration

	mu     sync.Mutex
	timers map[domain.Slug]*repurge
	closed bool
	// wg counts armed timers and running callbacks, so Shutdown can wait out
	// a callback that already fired.
	wg sync.WaitGroup
}

type repurge struct{ t *time.Timer }

func NewRepurging(inner Purger, delay time.Duration) *Repurging {
	return &Repurging{inner: inner, delay: delay, timers: map[domain.Slug]*repurge{}}
}

// PurgePaste purges now and schedules the delayed purge. The error is the
// immediate purge's.
func (r *Repurging) PurgePaste(slug domain.Slug) error {
	err := r.inner.PurgePaste(slug)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return err
	}
	if p, ok := r.timers[slug]; ok && p.t.Stop() {
		p.t.Reset(r.delay)
		return err
	}
	// None armed, or its callback already fired; that callback leaves this
	// entry in place.
	p := &repurge{}
	r.wg.Add(1)
	p.t = time.AfterFunc(r.delay, func() { r.fire(slug, p) })
	r.timers[slug] = p
	return err
}

func (r *Repurging) fire(slug domain.Slug, p *repurge) {
	defer r.wg.Done()
	r.mu.Lock()
	if r.timers[slug] == p {
		delete(r.timers, slug)
	}
	closed := r.closed
	r.mu.Unlock()
	if !closed {
		_ = r.inner.PurgePaste(slug)
	}
}

// Shutdown cancels every delayed purge not yet fired, waits for any firing
// within ctx, then shuts the inner purger down when it has a shutdown.
func (r *Repurging) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	for slug, p := range r.timers {
		if p.t.Stop() {
			r.wg.Done()
		}
		delete(r.timers, slug)
	}
	r.mu.Unlock()

	fired := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(fired)
	}()
	var err error
	select {
	case <-fired:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if s, ok := r.inner.(interface{ Shutdown(context.Context) error }); ok {
		if serr := s.Shutdown(ctx); err == nil {
			err = serr
		}
	}
	return err
}
