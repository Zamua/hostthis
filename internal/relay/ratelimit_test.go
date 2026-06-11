package relay

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsBurstThenThrottles(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newRateLimiter(5)
	rl.now = func() time.Time { return now } // freeze the clock
	rl.last = now                            // align last with the frozen clock

	// A full bucket (5 tokens) allows 5 frames immediately, then refuses.
	for i := 0; i < 5; i++ {
		if !rl.allow() {
			t.Fatalf("frame %d refused within burst of 5", i)
		}
	}
	if rl.allow() {
		t.Fatal("6th frame within the same instant was allowed; rate limit not enforced")
	}

	// After one second, the bucket refills to its cap (5), allowing 5 more.
	now = now.Add(time.Second)
	for i := 0; i < 5; i++ {
		if !rl.allow() {
			t.Fatalf("frame %d after refill refused", i)
		}
	}
	if rl.allow() {
		t.Fatal("over-cap frame after refill allowed")
	}
}

func TestRateLimiter_DisabledAlwaysAllows(t *testing.T) {
	rl := newRateLimiter(0)
	for i := 0; i < 1000; i++ {
		if !rl.allow() {
			t.Fatalf("disabled limiter refused frame %d", i)
		}
	}
}
