// The legacy-site migration's schedule.

package service_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/service"
)

// The legacy-site migration runs on EVERY tick, not once. Which units a node
// owns is still settling during a rollout, so a single early pass can see
// nothing and nothing would run again.
func TestSweep_DrainsLegacySitesEveryTick(t *testing.T) {
	var calls int
	s := service.NewSweep(log.New(io.Discard, "", 0))
	s.LegacySites = legacySweeperFunc(func(context.Context, time.Time) (int, error) {
		calls++
		return 0, nil
	})
	s.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if calls < 3 {
		t.Fatalf("legacy sweep ran %d time(s); a once-at-boot pass cannot converge", calls)
	}
}

type legacySweeperFunc func(context.Context, time.Time) (int, error)

func (f legacySweeperFunc) SweepLegacySites(ctx context.Context, now time.Time) (int, error) {
	return f(ctx, now)
}
