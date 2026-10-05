package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/Zamua/hostthis/internal/domain"
)

// GatedBlobStore admits Puts to Inner through a byte-weighted FIFO queue, so
// the bytes in flight to Inner never exceed the budget (docs/SPEC.md "Celld
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
	w := g.weight(size)
	ctx, cancel := context.WithTimeout(context.Background(), g.wait)
	defer cancel()
	if err := g.sem.Acquire(ctx, w); err != nil {
		return fmt.Errorf("blob put %s: %w", key, domain.ErrStoreBusy)
	}
	defer g.sem.Release(w)
	return g.Inner.Put(key, r, size)
}

func (g *GatedBlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	return g.Inner.GetReader(key)
}

func (g *GatedBlobStore) DeletePrefix(prefix string) error {
	return g.Inner.DeletePrefix(prefix)
}

var _ InnerBlobStore = (*GatedBlobStore)(nil)
