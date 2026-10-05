package ssh

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/Zamua/hostthis/internal/domain"
)

// UploadAdmission bounds how many uploads one process runs at once, since each
// holds an encoder and stream buffers (docs/SPEC.md "Upload admission").
// Waiters are admitted FIFO.
type UploadAdmission struct {
	limit   int
	wait    time.Duration
	sem     *semaphore.Weighted
	waiting atomic.Int64
}

// NewUploadAdmission admits at most limit uploads at once; an upload not
// admitted within wait fails with domain.ErrStoreBusy.
func NewUploadAdmission(limit int, wait time.Duration) (*UploadAdmission, error) {
	if limit < 1 {
		return nil, fmt.Errorf("upload admission: limit must be >= 1, got %d", limit)
	}
	if wait <= 0 {
		return nil, fmt.Errorf("upload admission: wait must be positive, got %s", wait)
	}
	return &UploadAdmission{limit: limit, wait: wait, sem: semaphore.NewWeighted(int64(limit))}, nil
}

// Limit is the configured number of concurrent uploads.
func (a *UploadAdmission) Limit() int { return a.limit }

// Wait is the configured admission wait limit.
func (a *UploadAdmission) Wait() time.Duration { return a.wait }

// Waiting reports how many callers are queued for a slot.
func (a *UploadAdmission) Waiting() int { return int(a.waiting.Load()) }

// Acquire blocks until a slot frees. It fails with domain.ErrStoreBusy once
// the wait limit passes, or with ctx's error if ctx ends first. The returned
// release is safe to call more than once.
func (a *UploadAdmission) Acquire(ctx context.Context) (release func(), err error) {
	wctx, cancel := context.WithTimeout(ctx, a.wait)
	defer cancel()
	a.waiting.Add(1)
	err = a.sem.Acquire(wctx, 1)
	a.waiting.Add(-1)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("upload admission: %w", domain.ErrStoreBusy)
		}
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { a.sem.Release(1) }) }, nil
}
