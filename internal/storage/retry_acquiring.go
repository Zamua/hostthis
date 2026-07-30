// Untagged on purpose: the callers are behind the slatedb tag, but CI builds no
// tags, and these constants are exactly what the pins exist to catch changing.

package storage

import (
	"errors"
	"log"
	"time"

	"github.com/Zamua/shale/pkg/cluster"
)

// retryPolicy bounds retries of the one shale refusal that resolves without
// external recovery: a routed op refusing with cluster.ErrAcquiring because its
// unit is mid-handoff.
//
// errors.Is(err, cluster.ErrAcquiring) is the ONLY retryable condition, and the
// exclusions are load-bearing:
//
//   - a bare codes.Unavailable is overloaded with genuine peer-down, and
//     retrying a real outage turns a fast failure slow while adding load.
//   - DeadlineExceeded is indistinguishable from real slowness from here.
type retryPolicy struct {
	attempts int           // total calls, including the first
	backoff  time.Duration // base delay; doubles per retry
}

var (
	// readRetry is the request-path policy. Each attempt can burn shale's whole
	// per-dispatch read budget, and the outer bound is the http.Server
	// WriteTimeout, so attempts x read-budget plus backoff must fit inside the
	// response deadline with margin for rendering.
	//
	// Two is sufficient because shale re-polls inside its own budget: this is a
	// second line, not the first, and a handoff outliving it is a cluster
	// problem rather than the blip this covers.
	readRetry = retryPolicy{attempts: 2, backoff: 150 * time.Millisecond}

	// backgroundRetry is for cross-shard background scans. Nothing waits on
	// them, so they can be patient.
	//
	// The span must EXCEED a real handoff (a node holds positions unmounted for
	// 17-21s) or it just postpones the same failure. 6 attempts at a 1s base
	// gives 31s of backoff.
	backgroundRetry = retryPolicy{attempts: 6, backoff: time.Second}
)

// retryAcquiring runs fn, retrying only while it refuses with
// cluster.ErrAcquiring. Any other error returns unchanged; on exhaustion the
// last acquiring error surfaces.
//
// A retry logs only when one actually happens. Unobserved, a retry firing
// constantly and one never firing look identical from outside: both are a
// quiet, green deploy.
//
// op names the operation rather than the key, since keys carry user content.
func retryAcquiring(p retryPolicy, lg *log.Logger, op string, fn func() error) error {
	if p.attempts < 1 {
		p.attempts = 1
	}
	var err error
	for attempt := 0; attempt < p.attempts; attempt++ {
		if attempt > 0 {
			// Sleep only between attempts, so a succeeding op pays no backoff.
			time.Sleep(p.backoff << (attempt - 1))
			if lg != nil {
				lg.Printf("shale: %s refused with the acquiring-window signal; retry %d/%d (a unit is mid-handoff)",
					op, attempt, p.attempts-1)
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		if !errors.Is(err, cluster.ErrAcquiring) {
			return err
		}
	}
	return err
}
