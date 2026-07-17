package service

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// countingCreateRepo is an instrumented PasteRepo fake for the admission
// pins: it counts how many InsertWithQuotaCheck calls are inside it
// simultaneously (the number the gate must bound) and can hold a given
// identity's inserts open on a channel so a test can wedge one owner
// while probing another.
type countingCreateRepo struct {
	mu        sync.Mutex
	inside    int
	maxInside int
	calls     int
	dwell     time.Duration
	holdFor   map[string]chan struct{} // identity -> block inserts until closed
}

func (r *countingCreateRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, _ int64, _ time.Time) error {
	r.mu.Lock()
	r.inside++
	if r.inside > r.maxInside {
		r.maxInside = r.inside
	}
	hold := r.holdFor[p.Identity.String()]
	r.mu.Unlock()

	if hold != nil {
		<-hold
	}
	if r.dwell > 0 {
		time.Sleep(r.dwell)
	}

	r.mu.Lock()
	r.inside--
	r.calls++
	r.mu.Unlock()
	return nil
}

func (r *countingCreateRepo) Get(domain.Slug) (domain.Paste, error) { return domain.Paste{}, nil }
func (r *countingCreateRepo) MarkReady(domain.Slug) error           { return nil }
func (r *countingCreateRepo) MarkFailed(domain.Slug) error          { return nil }

func (r *countingCreateRepo) insideNow() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inside
}

func (r *countingCreateRepo) stats() (maxInside, calls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxInside, r.calls
}

func gatedInsert(t *testing.T, repo PasteRepo, identity string) {
	t.Helper()
	p := domain.Paste{Identity: domain.Identity(identity), Slug: domain.NewRandomSlug()}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, time.Now()); err != nil {
		t.Errorf("insert for %s: %v", identity, err)
	}
}

// TestCreateAdmission_AtMostWidthInside pins the admission contract: K
// concurrent same-identity creates all complete, but at most width (2) are
// ever inside the gated section simultaneously. The ungated control run
// proves the instrumentation would catch a broken gate (without the gate the
// same burst overlaps well past 2).
func TestCreateAdmission_AtMostWidthInside(t *testing.T) {
	const K = 32

	burst := func(t *testing.T, repo PasteRepo) {
		t.Helper()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range K {
			wg.Go(func() {
				<-start
				gatedInsert(t, repo, "key:stormer")
			})
		}
		close(start)
		wg.Wait()
	}

	t.Run("gated: bounded at width 2, all complete", func(t *testing.T) {
		repo := &countingCreateRepo{dwell: 3 * time.Millisecond}
		gated := GateCreates(repo, NewCreateAdmission(2))
		burst(t, gated)
		maxInside, calls := repo.stats()
		if calls != K {
			t.Fatalf("completed creates: got %d, want %d", calls, K)
		}
		if maxInside > 2 {
			t.Fatalf("concurrent entrants: got %d, want <= 2 (gate width)", maxInside)
		}
	})

	t.Run("ungated control: the instrument sees the storm", func(t *testing.T) {
		repo := &countingCreateRepo{dwell: 3 * time.Millisecond}
		burst(t, repo)
		maxInside, _ := repo.stats()
		if maxInside <= 2 {
			t.Fatalf("ungated burst overlapped only %d deep; the concurrency instrument is not observing overlap", maxInside)
		}
	})

	t.Run("width below 1 clamps to 1", func(t *testing.T) {
		repo := &countingCreateRepo{dwell: time.Millisecond}
		gated := GateCreates(repo, NewCreateAdmission(0))
		burst(t, gated)
		maxInside, calls := repo.stats()
		if calls != K {
			t.Fatalf("completed creates: got %d, want %d", calls, K)
		}
		if maxInside > 1 {
			t.Fatalf("concurrent entrants: got %d, want <= 1 (clamped width)", maxInside)
		}
	})
}

// TestCreateAdmission_IdentitiesIndependent pins independence: with owner A
// saturated (both slots held inside the repo and a third create queued at
// the gate), owner B's create completes promptly - one identity's storm
// never delays another identity.
func TestCreateAdmission_IdentitiesIndependent(t *testing.T) {
	unblockA := make(chan struct{})
	repo := &countingCreateRepo{holdFor: map[string]chan struct{}{"key:a": unblockA}}
	gated := GateCreates(repo, NewCreateAdmission(2))

	var wgA sync.WaitGroup
	for range 3 {
		wgA.Go(func() {
			gatedInsert(t, gated, "key:a")
		})
	}
	// Wait until A is genuinely wedged: both slots held inside the repo.
	deadline := time.Now().Add(5 * time.Second)
	for repo.insideNow() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("owner A never saturated the gate: %d inside", repo.insideNow())
		}
		time.Sleep(time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		gatedInsert(t, gated, "key:b")
		close(done)
	}()
	select {
	case <-done:
		// Owner B got through while A was wedged: independent.
	case <-time.After(2 * time.Second):
		t.Fatal("owner B's create was delayed behind owner A's storm")
	}

	close(unblockA)
	wgA.Wait()
}

// TestCreateAdmission_NoIdleEntries pins the no-leak property: once every
// holder and waiter for an identity has released, the gate retains no entry
// for it - idle owners cost nothing.
func TestCreateAdmission_NoIdleEntries(t *testing.T) {
	repo := &countingCreateRepo{dwell: time.Millisecond}
	gate := NewCreateAdmission(2)
	gated := GateCreates(repo, gate)

	owners := []string{"key:a", "key:b", "key:c", "key:d"}
	var wg sync.WaitGroup
	for _, owner := range owners {
		for range 6 {
			wg.Go(func() {
				gatedInsert(t, gated, owner)
			})
		}
	}
	wg.Wait()

	if n := gate.keyCount(); n != 0 {
		t.Fatalf("gate retains %d idle owner entries, want 0", n)
	}
}

// TestCreateAdmission_LoneCreatePassesThrough pins the fast path: an
// uncontended create acquires with a map access - no goroutine spawn, no
// scheduler round-trip - so lone-create latency is unaffected by the gate.
func TestCreateAdmission_LoneCreatePassesThrough(t *testing.T) {
	repo := &countingCreateRepo{}
	gated := GateCreates(repo, NewCreateAdmission(2))

	before := runtime.NumGoroutine()
	start := time.Now()
	const N = 1000
	for range N {
		gatedInsert(t, gated, "key:lone")
	}
	elapsed := time.Since(start)
	after := runtime.NumGoroutine()

	if after > before {
		t.Fatalf("fast path grew the goroutine count: %d -> %d", before, after)
	}
	// 1000 uncontended acquire/release cycles are map accesses; even a very
	// generous bound catches an implementation that blocks or schedules.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("%d uncontended gated creates took %v; the uncontended path must not block", N, elapsed)
	}
	if _, calls := repo.stats(); calls != N {
		t.Fatalf("completed creates: got %d, want %d", calls, N)
	}
}
