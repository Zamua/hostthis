package storage

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/Zamua/hostthis/internal/domain"
)

// GatedBlobStore admits Puts to Inner through a byte-weighted FIFO queue, so
// the bytes in flight to Inner never exceed the budget (docs/SPEC.md "Worker
// upload admission"). Reads and deletes pass straight through.
type GatedBlobStore struct {
	Inner  InnerBlobStore
	budget int64
	wait   time.Duration
	sem    *semaphore.Weighted
}

// NewGatedBlobStore admits at most budget bytes of Puts at once; a Put not
// admitted within wait fails with domain.ErrStoreBusy.
func NewGatedBlobStore(inner InnerBlobStore, budget int64, wait time.Duration) (*GatedBlobStore, error) {
	if budget < 1 {
		return nil, fmt.Errorf("blob gate: budget must be >= 1, got %d", budget)
	}
	if wait <= 0 {
		return nil, fmt.Errorf("blob gate: wait must be positive, got %s", wait)
	}
	return &GatedBlobStore{Inner: inner, budget: budget, wait: wait, sem: semaphore.NewWeighted(budget)}, nil
}

// weight clamps size to [1, budget]: an oversized body is admitted alone rather
// than never, and an unknown (negative) size is assumed to be the worst case.
func (g *GatedBlobStore) weight(size int64) int64 {
	switch {
	case size < 0 || size > g.budget:
		return g.budget
	case size == 0:
		return 1
	default:
		return size
	}
}

// Put waits for admission without touching r, then streams it to Inner.
func (g *GatedBlobStore) Put(key string, r io.Reader, size int64) error {
	return g.PutContext(context.Background(), key, r, size)
}

// PutContext is Put for a caller that may already hold the write's admission
// (from Admit, carried in ctx): then nothing more is acquired or released.
func (g *GatedBlobStore) PutContext(ctx context.Context, key string, r io.Reader, size int64) error {
	if held, _ := ctx.Value(admissionKey{}).(*admission); held != nil && held.gate == g {
		return g.Inner.Put(key, r, size)
	}
	release, err := g.acquire(size)
	if err != nil {
		return fmt.Errorf("blob put %s: %w", key, err)
	}
	defer release()
	return g.Inner.Put(key, r, size)
}

// Admit takes the admission of a later write of size bytes now, so the wait
// (and a busy refusal) happens before the caller commits to the write. The
// returned context carries it to PutContext; release frees it and is safe to
// call more than once.
func (g *GatedBlobStore) Admit(ctx context.Context, size int64) (context.Context, func(), error) {
	release, err := g.acquire(size)
	if err != nil {
		return ctx, nil, fmt.Errorf("blob admission: %w", err)
	}
	return context.WithValue(ctx, admissionKey{}, &admission{gate: g}), release, nil
}

// acquire waits up to the wait limit for size bytes of budget.
func (g *GatedBlobStore) acquire(size int64) (release func(), err error) {
	w := g.weight(size)
	ctx, cancel := context.WithTimeout(context.Background(), g.wait)
	defer cancel()
	if err := g.sem.Acquire(ctx, w); err != nil {
		return nil, domain.ErrStoreBusy
	}
	var once sync.Once
	return func() { once.Do(func() { g.sem.Release(w) }) }, nil
}

type admissionKey struct{}

// admission marks a context whose write gate already admitted.
type admission struct{ gate *GatedBlobStore }

func (g *GatedBlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	return g.Inner.GetReader(key)
}

func (g *GatedBlobStore) GetRange(key string, offset, length int64) (io.ReadCloser, error) {
	return g.Inner.GetRange(key, offset, length)
}

func (g *GatedBlobStore) DeletePrefix(prefix string) error {
	return g.Inner.DeletePrefix(prefix)
}

func (g *GatedBlobStore) Delete(key string) error {
	return g.Inner.Delete(key)
}

var _ InnerBlobStore = (*GatedBlobStore)(nil)
