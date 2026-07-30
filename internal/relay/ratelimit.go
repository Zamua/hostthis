package relay

import (
	"sync"
	"time"
)

// rateLimiter is a per-connection token bucket over inbound frames. Fan-out
// multiplies every inbound frame by the room's connection count, so without a
// per-connection ceiling one hostile client saturates the whole room. Fills at
// maxPerSec tokens/second with a burst of maxPerSec; maxPerSec <= 0 disables
// the limit.
type rateLimiter struct {
	mu        sync.Mutex
	maxPerSec float64
	tokens    float64
	last      time.Time
	now       func() time.Time // injectable for tests
}

func newRateLimiter(maxPerSec int) *rateLimiter {
	return &rateLimiter{
		maxPerSec: float64(maxPerSec),
		tokens:    float64(maxPerSec),
		last:      time.Now(),
		now:       time.Now,
	}
}

// allow spends a token for one inbound frame. False means the connection is
// over its ceiling, which the reader treats as "drop this connection".
func (r *rateLimiter) allow() bool {
	if r.maxPerSec <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	elapsed := now.Sub(r.last).Seconds()
	r.last = now
	r.tokens += elapsed * r.maxPerSec
	if r.tokens > r.maxPerSec {
		r.tokens = r.maxPerSec
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
