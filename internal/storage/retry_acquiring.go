//go:build slatedb

package storage

import (
	"errors"
	"time"

	"github.com/Zamua/shale/pkg/cluster"
)

// Bounded retry for the ONE shale refusal that is bounded by a mount rather
// than by an outage.
//
// During a handoff a unit can be routed to a node that is mid-acquire, and a
// routed op there refuses with cluster.ErrAcquiring: the work will become
// possible once the handoff completes, with nothing external needing to
// recover. shale signals that clearly, but does NOT re-route within the same
// op, so without this the refusal reaches the caller as a request failure.
//
// Deliberately narrow. errors.Is(err, cluster.ErrAcquiring) is the ONLY
// retryable condition:
//
//   - A bare codes.Unavailable is NOT retried. It is overloaded with genuine
//     peer-down, and retrying a real outage converts a clean fast failure into
//     a slow one while adding load exactly when the cluster is struggling.
//   - DeadlineExceeded is NOT retried. A permanently-mixed read (some legs
//     acquiring, some genuinely down, window outliving the read budget)
//     terminates that way, and it is indistinguishable from real slowness from
//     out here, so it falls through to normal error handling.
//
// Both exclusions are load-bearing and pinned by tests.
type retryPolicy struct {
	attempts int           // total calls, including the first
	backoff  time.Duration // base delay; doubles per retry
}

var (
	// readRetry is the REQUEST-PATH policy. Each attempt can burn shale's
	// entire per-dispatch read budget (HOSTTHIS_SHALE_READ_TIMEOUT) before
	// refusing, and the outer bound is the http.Server WriteTimeout, so the
	// attempt count follows from that arithmetic rather than being chosen for
	// feel: attempts x read-budget, plus backoff, must fit inside the response
	// deadline with margin left for rendering and write-out.
	//
	// Two is also sufficient rather than merely safe. shale already re-polls
	// INSIDE its own budget, so this is a second line, not the first: one
	// extra attempt roughly doubles the covered window, and a handoff that
	// outlives ~16s is a genuine cluster problem rather than the sub-second
	// blip this exists for. TestReadRetryPolicy_FitsInsideRequestDeadline
	// fails if the arithmetic stops holding.
	readRetry = retryPolicy{attempts: 2, backoff: 150 * time.Millisecond}

	// backgroundRetry is for the cross-shard background scans (expiry sweep,
	// referenced-blob set, keygate prune). They are NOT request-path, so no
	// outer deadline constrains them and they can afford to be patient. A
	// retried background fan-out costs some seconds; the alternative on the
	// blob-GC consumer is acting on an incomplete answer.
	backgroundRetry = retryPolicy{attempts: 3, backoff: 500 * time.Millisecond}
)

// retryAcquiring runs op, retrying ONLY while it refuses with
// cluster.ErrAcquiring, up to p.attempts total calls with exponential
// backoff. Any other error (and success) returns immediately, unchanged, so
// callers keep their existing error handling. On exhaustion the last
// acquiring error is returned so the caller still sees the real reason.
func retryAcquiring(p retryPolicy, op func() error) error {
	if p.attempts < 1 {
		p.attempts = 1
	}
	var err error
	for attempt := 0; attempt < p.attempts; attempt++ {
		if attempt > 0 {
			// Sleep only between attempts: a succeeding op must never pay
			// backoff, because that would tax every healthy read.
			time.Sleep(p.backoff << (attempt - 1))
		}
		if err = op(); err == nil {
			return nil
		}
		if !errors.Is(err, cluster.ErrAcquiring) {
			return err
		}
	}
	return err
}
