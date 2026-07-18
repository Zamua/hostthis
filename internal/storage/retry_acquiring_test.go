//go:build slatedb

package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/cluster"
)

// The retry exists for ONE shale contract: a routed op can refuse with
// cluster.ErrAcquiring while a unit is mid-handoff, and that refusal is
// bounded by the handoff completing rather than by an outage. Everything
// pinned below is a property the deploy depends on, so each test names the
// failure it prevents rather than the branch it covers.

func TestRetryAcquiring_SucceedsFirstTry_NoSleep(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryAcquiring(fastRetry, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a succeeding op must be called exactly once, got %d", calls)
	}
	// Guards against a backoff that sleeps BEFORE deciding to retry, which
	// would tax every healthy read on the hot path.
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("success path must not sleep; took %v", elapsed)
	}
}

func TestRetryAcquiring_RetriesAcquiringThenSucceeds(t *testing.T) {
	calls := 0
	err := retryAcquiring(fastRetry, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("get k: %w", cluster.ErrAcquiring)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want nil after a retried handoff, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls (refused, then served), got %d", calls)
	}
}

// The wrapped form is what callers actually produce: every chokepoint wraps
// with fmt.Errorf("...: %w", err), so a matcher testing == would silently
// never fire in production while passing a naive test.
func TestRetryAcquiring_MatchesThroughWrapping(t *testing.T) {
	calls := 0
	deep := fmt.Errorf("aggregate p: %w", fmt.Errorf("leg 3: %w", cluster.ErrAcquiring))
	_ = retryAcquiring(fastRetry, func() error {
		calls++
		if calls < 2 {
			return deep
		}
		return nil
	})
	if calls != 2 {
		t.Fatalf("errors.Is must see ErrAcquiring through nested wrapping; calls=%d", calls)
	}
}

// The single most important negative: a genuine outage must fail FAST.
// Retrying a real peer-down converts a clean fast failure into a slow one
// and amplifies load exactly when the cluster is already struggling.
func TestRetryAcquiring_DoesNotRetryNonAcquiring(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"peer down", errors.New("rpc error: code = Unavailable desc = connection refused")},
		{"deadline", errors.New("context deadline exceeded")},
		{"not found", ErrNotFound},
		{"corrupt", errors.New("strip k: truncated envelope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryAcquiring(fastRetry, func() error {
				calls++
				return tc.err
			})
			if calls != 1 {
				t.Fatalf("%s must NOT be retried, got %d calls", tc.name, calls)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("terminal error must pass through unchanged, got %v", err)
			}
		})
	}
}

// Bounded, because each read attempt can burn shale's full ReadTimeout (8s in
// a deployment may raise) and the outer http.Server WriteTimeout bounds it. An unbounded retry
// would outlive the response deadline and turn a fast typed failure into a
// slow one, which is the same trap as gating on bare Unavailable.
func TestRetryAcquiring_IsBounded(t *testing.T) {
	calls := 0
	err := retryAcquiring(fastRetry, func() error {
		calls++
		return fmt.Errorf("get k: %w", cluster.ErrAcquiring)
	})
	if calls != fastRetry.attempts {
		t.Fatalf("want exactly %d attempts, got %d", fastRetry.attempts, calls)
	}
	if !errors.Is(err, cluster.ErrAcquiring) {
		t.Fatalf("exhausted retry must surface the acquiring error, got %v", err)
	}
}

// The read policy must fit inside the request deadline with margin. This
// pins the ARITHMETIC, not the constant: if someone raises the attempt count
// or the shale read budget without re-checking the outer deadline, this
// fails rather than silently shipping a retry that outlives the response.
func TestReadRetryPolicy_FitsInsideRequestDeadline(t *testing.T) {
	const (
		shaleReadBudget   = 8 * time.Second  // a representative HOSTTHIS_SHALE_READ_TIMEOUT
		outerWriteTimeout = 30 * time.Second // the http.Server WriteTimeout the daemon sets
		requiredMargin    = 5 * time.Second  // room for render + write-out
	)
	worst := time.Duration(readRetry.attempts) * shaleReadBudget
	for i := 0; i < readRetry.attempts-1; i++ {
		worst += readRetry.backoff << i
	}
	if worst+requiredMargin > outerWriteTimeout {
		t.Fatalf("read retry worst case %v + %v margin exceeds the %v request deadline; "+
			"lower readRetry.attempts or raise the deadline deliberately", worst, requiredMargin, outerWriteTimeout)
	}
}

// Background fan-outs are NOT request-path, so they may retry more patiently.
// Pinned so a future edit does not accidentally collapse the two policies and
// silently make background scans as impatient as reads.
func TestBackgroundRetryPolicy_IsMorePatientThanRead(t *testing.T) {
	if backgroundRetry.attempts < readRetry.attempts {
		t.Fatalf("background retry (%d) must be at least as persistent as read (%d)",
			backgroundRetry.attempts, readRetry.attempts)
	}
}

var fastRetry = retryPolicy{attempts: 3, backoff: time.Millisecond}
