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
// Waiters are admitted FIFO. One owner may hold or wait for at most perOwner
// of the slots, so a single key cannot take them all.
type UploadAdmission struct {
	limit    int
	perOwner int
	wait     time.Duration
	sem      *semaphore.Weighted
	waiting  atomic.Int64

	mu      sync.Mutex
	byOwner map[domain.Owner]int // uploads admitted or waiting, per owner
}

// OwnerUploadsError refuses an upload whose owner already has Limit uploads
// admitted or waiting.
type OwnerUploadsError struct{ Limit int }

func (e *OwnerUploadsError) Error() string {
	return fmt.Sprintf("%d uploads already running for this key; try again when one finishes", e.Limit)
}

// NewUploadAdmission admits at most limit uploads at once, at most perOwner of
// them for one owner; an upload not admitted within wait fails with
// domain.ErrStoreBusy.
func NewUploadAdmission(limit, perOwner int, wait time.Duration) (*UploadAdmission, error) {
	if limit < 1 {
		return nil, fmt.Errorf("upload admission: limit must be >= 1, got %d", limit)
	}
	if perOwner < 1 || perOwner > limit {
		return nil, fmt.Errorf("upload admission: per-key limit must be between 1 and the limit %d, got %d", limit, perOwner)
	}
	if wait <= 0 {
		return nil, fmt.Errorf("upload admission: wait must be positive, got %s", wait)
	}
	return &UploadAdmission{
		limit: limit, perOwner: perOwner, wait: wait,
		sem: semaphore.NewWeighted(int64(limit)), byOwner: map[domain.Owner]int{},
	}, nil
}

// Limit is the configured number of concurrent uploads.
func (a *UploadAdmission) Limit() int { return a.limit }

// PerOwner is the configured number of concurrent uploads for one owner.
func (a *UploadAdmission) PerOwner() int { return a.perOwner }

// Wait is the configured admission wait limit.
func (a *UploadAdmission) Wait() time.Duration { return a.wait }

// Waiting reports how many callers are queued for a slot.
func (a *UploadAdmission) Waiting() int { return int(a.waiting.Load()) }

// Acquire blocks until a slot frees. It fails at once with *OwnerUploadsError
// when owner is at its limit, with domain.ErrStoreBusy once the wait limit
// passes, or with ctx's error if ctx ends first. The returned release is safe
// to call more than once.
func (a *UploadAdmission) Acquire(ctx context.Context, owner domain.Owner) (release func(), err error) {
	if !a.enter(owner) {
		return nil, &OwnerUploadsError{Limit: a.perOwner}
	}
	wctx, cancel := context.WithTimeout(ctx, a.wait)
	defer cancel()
	a.waiting.Add(1)
	err = a.sem.Acquire(wctx, 1)
	a.waiting.Add(-1)
	if err != nil {
		a.leave(owner)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("upload admission: %w", domain.ErrStoreBusy)
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			a.sem.Release(1)
			a.leave(owner)
		})
	}, nil
}

// enter counts one more upload for owner, or reports false at its limit.
func (a *UploadAdmission) enter(owner domain.Owner) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byOwner[owner] >= a.perOwner {
		return false
	}
	a.byOwner[owner]++
	return true
}

func (a *UploadAdmission) leave(owner domain.Owner) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byOwner[owner]--; a.byOwner[owner] <= 0 {
		delete(a.byOwner, owner)
	}
}
