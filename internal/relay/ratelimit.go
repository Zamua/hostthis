package relay

import (
	"sync"
	"time"
)

// rateLimiter is a per-connection token bucket bounding the inbound
// message rate (the per-connection send rate limit). It is the relay's
// analogue of the room-creation rate limit on the KV side: a client over
// its ceiling is dropped, so one hostile connection cannot saturate a
// room's fan-out (each inbound frame is multiplied by the room's
// connection count on the way out).
//
// The bucket fills at maxPerSec tokens/second up to a burst of maxPerSec
// (one second of headroom), and each inbound frame spends one token.
// allow returns false when the bucket is empty, which the reader treats as
// "drop this connection." A maxPerSec of 0 disables the limit (used by
// tests isolating other axes).
type rateLimiter struct {
	mu        sync.Mutex
	maxPerSec float64
	tokens    float64
	last      time.Time
	now       func() time.Time // injectable for tests; defaults to time.Now
}

func newRateLimiter(maxPerSec int) *rateLimiter {
	return &rateLimiter{
		maxPerSec: float64(maxPerSec),
		tokens:    float64(maxPerSec),
		last:      time.Now(),
		now:       time.Now,
	}
}

// allow reports whether one more inbound frame is within the rate ceiling,
// spending a token if so. A disabled limiter (maxPerSec <= 0) always
// allows.
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
