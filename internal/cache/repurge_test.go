package cache

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// timedPurger records each purge with its time, and whether it was shut down.
type timedPurger struct {
	mu       sync.Mutex
	slugs    []domain.Slug
	at       []time.Time
	shutdown bool
}

func (p *timedPurger) PurgePaste(slug domain.Slug) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slugs = append(p.slugs, slug)
	p.at = append(p.at, time.Now())
	return nil
}

func (p *timedPurger) Shutdown(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shutdown = true
	return nil
}

func (p *timedPurger) snapshot() ([]domain.Slug, []time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.slugs), slices.Clone(p.at)
}

// waitFor polls until the inner purger holds n purges or the deadline passes.
func waitFor(t *testing.T, p *timedPurger, n int) []domain.Slug {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		slugs, _ := p.snapshot()
		if len(slugs) >= n || time.Now().After(deadline) {
			return slugs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const testDelay = 60 * time.Millisecond

// A purge goes out at once and again after the delay, so an edge put racing
// the first purge is dropped by the second.
func TestRepurging_PurgesNowAndAgainAfterTheDelay(t *testing.T) {
	inner := &timedPurger{}
	r := NewRepurging(inner, testDelay)
	start := time.Now()
	if err := r.PurgePaste("aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if slugs, _ := inner.snapshot(); !slices.Equal(slugs, []domain.Slug{"aaaaaaaa"}) {
		t.Fatalf("immediate purges = %v, want [aaaaaaaa]", slugs)
	}
	if slugs := waitFor(t, inner, 2); !slices.Equal(slugs, []domain.Slug{"aaaaaaaa", "aaaaaaaa"}) {
		t.Fatalf("purges = %v, want the slug twice", slugs)
	}
	if _, at := inner.snapshot(); at[1].Sub(start) < testDelay {
		t.Fatalf("second purge after %v, want at least %v", at[1].Sub(start), testDelay)
	}
}

// Writes in a burst each purge at once, and the slug gets one delayed purge, a
// full delay after the last of them.
func TestRepurging_ABurstGetsOneDelayedPurgeAfterItsLast(t *testing.T) {
	inner := &timedPurger{}
	r := NewRepurging(inner, testDelay)
	_ = r.PurgePaste("aaaaaaaa")
	time.Sleep(testDelay / 2)
	last := time.Now()
	_ = r.PurgePaste("aaaaaaaa")
	_ = r.PurgePaste("bbbbbbbb")
	waitFor(t, inner, 5)
	time.Sleep(2 * testDelay)
	slugs, at := inner.snapshot()
	if len(slugs) != 5 {
		t.Fatalf("purges = %v, want 3 immediate and one delayed per slug", slugs)
	}
	for i, s := range slugs[3:] {
		if gap := at[3+i].Sub(last); gap < testDelay {
			t.Errorf("delayed purge of %s came %v after the last write, want at least %v", s, gap, testDelay)
		}
	}
}

// Shutdown cancels the delayed purges still waiting, then shuts the inner
// purger down; a purge after it is not scheduled again.
func TestRepurging_ShutdownCancelsDelayedPurges(t *testing.T) {
	inner := &timedPurger{}
	r := NewRepurging(inner, testDelay)
	_ = r.PurgePaste("aaaaaaaa")
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = r.PurgePaste("bbbbbbbb")
	time.Sleep(2 * testDelay)
	slugs, _ := inner.snapshot()
	if !slices.Equal(slugs, []domain.Slug{"aaaaaaaa", "bbbbbbbb"}) {
		t.Fatalf("purges = %v, want only the immediate ones", slugs)
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if !inner.shutdown {
		t.Fatal("inner purger was not shut down")
	}
}
