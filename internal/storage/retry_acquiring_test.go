package storage

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/cluster"
)

// The retry exists for ONE shale contract: a routed op can refuse with
// cluster.ErrAcquiring while a unit is mid-handoff, and that refusal is bounded
// by the handoff completing rather than by an outage. Each test below names the
// failure it prevents rather than the branch it covers.

func TestRetryAcquiring_SucceedsFirstTry_NoSleep(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryAcquiring(fastRetry, nil, "test", func() error {
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
	err := retryAcquiring(fastRetry, nil, "test", func() error {
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

// Every chokepoint wraps with fmt.Errorf("...: %w", err), so a matcher testing
// == would never fire in production while passing a naive test.
func TestRetryAcquiring_MatchesThroughWrapping(t *testing.T) {
	calls := 0
	deep := fmt.Errorf("aggregate p: %w", fmt.Errorf("leg 3: %w", cluster.ErrAcquiring))
	_ = retryAcquiring(fastRetry, nil, "test", func() error {
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

// A genuine outage must fail FAST: retrying a real peer-down converts a clean
// fast failure into a slow one and amplifies load on a struggling cluster.
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
			err := retryAcquiring(fastRetry, nil, "test", func() error {
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

// Bounded: each read attempt can burn shale's full read budget and the outer
// http.Server WriteTimeout bounds the whole response, so an unbounded retry
// would outlive the deadline and turn a fast typed failure into a slow one.
func TestRetryAcquiring_IsBounded(t *testing.T) {
	calls := 0
	err := retryAcquiring(fastRetry, nil, "test", func() error {
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

// Pins the ARITHMETIC, not the constant: raising the attempt count or the shale
// read budget without re-checking the outer deadline fails here rather than
// shipping a retry that outlives the response.
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

var fastRetry = retryPolicy{attempts: 3, backoff: time.Millisecond}

// The retry must be OBSERVABLE: unobserved, a retry that fires constantly
// (window wider than believed) and one that never fires (sentinel silently not
// matching) look identical from outside. Pins one line per real retry, and
// silence on the hot path.
func TestRetryAcquiring_LogsOnlyWhenItActuallyRetries(t *testing.T) {
	t.Run("success path is silent", func(t *testing.T) {
		var buf strings.Builder
		lg := log.New(&buf, "", 0)
		_ = retryAcquiring(fastRetry, lg, "get", func() error { return nil })
		if buf.Len() != 0 {
			t.Fatalf("healthy reads must not log; got %q", buf.String())
		}
	})

	t.Run("non-acquiring failure is silent", func(t *testing.T) {
		var buf strings.Builder
		lg := log.New(&buf, "", 0)
		_ = retryAcquiring(fastRetry, lg, "get", func() error { return ErrNotFound })
		if buf.Len() != 0 {
			t.Fatalf("a non-retryable error must not log a retry; got %q", buf.String())
		}
	})

	t.Run("each retry logs once", func(t *testing.T) {
		var buf strings.Builder
		lg := log.New(&buf, "", 0)
		calls := 0
		_ = retryAcquiring(fastRetry, lg, "aggregate", func() error {
			calls++
			return fmt.Errorf("leg: %w", cluster.ErrAcquiring)
		})
		got := strings.Count(buf.String(), "\n")
		if want := fastRetry.attempts - 1; got != want {
			t.Fatalf("want %d retry lines for %d attempts, got %d: %q", want, fastRetry.attempts, got, buf.String())
		}
		if !strings.Contains(buf.String(), "aggregate") {
			t.Fatalf("retry line must name the operation so a spike is attributable; got %q", buf.String())
		}
	})

	t.Run("nil logger is safe", func(t *testing.T) {
		if err := retryAcquiring(fastRetry, nil, "get", func() error { return nil }); err != nil {
			t.Fatalf("nil logger must not panic or error: %v", err)
		}
	})
}

// The retry budget belongs to the CALLER's context, not the mechanism: a
// cross-shard fan-out is "background", but whoami reaches one for best-effort
// session info and a call whose error the caller discards must never block an
// interactive command for the background span.
func TestRequestPathBudget_FitsAnInteractiveCommand(t *testing.T) {
	const interactiveCeiling = 10 * time.Second

	span := func(p retryPolicy) time.Duration {
		var d time.Duration
		for i := 0; i < p.attempts-1; i++ {
			d += p.backoff << i
		}
		return d
	}

	if got := span(readRetry); got > interactiveCeiling {
		t.Fatalf("request-path retry spans %v, too long to sit in front of an interactive command (ceiling %v)",
			got, interactiveCeiling)
	}
}
