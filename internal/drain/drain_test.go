package drain

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type step struct {
	op        string // admit | release | stop
	wantOK    bool   // admit and stop only
	wantState State
}

// Each sequence pins the state and the accept/refuse outcome after every event.
func TestGateEventSequences(t *testing.T) {
	cases := []struct {
		name  string
		steps []step
	}{
		{"stop idle drains at once", []step{
			{"stop", true, Drained},
		}},
		{"admit release stays admitting", []step{
			{"admit", true, Admitting},
			{"admit", true, Admitting},
			{"release", true, Admitting},
			{"release", true, Admitting},
			{"admit", true, Admitting},
		}},
		{"stop with work in flight drains on last release", []step{
			{"admit", true, Admitting},
			{"admit", true, Admitting},
			{"stop", true, Draining},
			{"release", true, Draining},
			{"release", true, Drained},
		}},
		{"admit refused while draining", []step{
			{"admit", true, Admitting},
			{"stop", true, Draining},
			{"admit", false, Draining},
			{"release", true, Drained},
			{"admit", false, Drained},
		}},
		{"second stop is ignored", []step{
			{"admit", true, Admitting},
			{"stop", true, Draining},
			{"stop", false, Draining},
			{"release", true, Drained},
			{"stop", false, Drained},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g Gate
			for i, s := range tc.steps {
				switch s.op {
				case "admit":
					if got := g.Admit(); got != s.wantOK {
						t.Fatalf("step %d admit = %v, want %v", i, got, s.wantOK)
					}
				case "stop":
					if got := g.Stop(); got != s.wantOK {
						t.Fatalf("step %d stop = %v, want %v", i, got, s.wantOK)
					}
				case "release":
					g.Release()
				}
				if got := g.State(); got != s.wantState {
					t.Fatalf("step %d %s: state %v, want %v", i, s.op, got, s.wantState)
				}
			}
		})
	}
}

func TestGateReleaseWithoutAdmitPanics(t *testing.T) {
	for _, name := range []string{"idle", "after stop", "after balanced release"} {
		t.Run(name, func(t *testing.T) {
			var g Gate
			switch name {
			case "after stop":
				g.Stop()
			case "after balanced release":
				g.Admit()
				g.Release()
			}
			defer func() {
				if recover() == nil {
					t.Fatal("Release without Admit did not panic")
				}
			}()
			g.Release()
		})
	}
}

func TestGateWaitReturnsAtOnceWhenIdle(t *testing.T) {
	var g Gate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("Wait on idle gate = %v, want nil", err)
	}
}

func TestGateWaitBlocksUntilLastRelease(t *testing.T) {
	var g Gate
	g.Admit()
	g.Admit()
	g.Stop()
	done := make(chan error, 1)
	go func() { done <- g.Wait(context.Background()) }()
	g.Release()
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v with work in flight", err)
	case <-time.After(20 * time.Millisecond):
	}
	g.Release()
	if err := <-done; err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

func TestGateWaitHonoursContext(t *testing.T) {
	var g Gate
	g.Admit()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want deadline exceeded", err)
	}
}

func TestGateWaitAcrossRefill(t *testing.T) {
	var g Gate
	g.Admit()
	g.Release()
	g.Admit()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait after refill = %v, want deadline exceeded", err)
	}
}

// Concurrent admits racing Stop: every admit that won is released and Wait
// sees the gate drained.
func TestGateConcurrentAdmitsAndStop(t *testing.T) {
	var g Gate
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Go(func() {
			<-start
			for range 100 {
				if !g.Admit() {
					return
				}
				g.Release()
			}
		})
	}
	close(start)
	g.Stop()
	wg.Wait()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if got := g.State(); got != Drained {
		t.Fatalf("state %v, want drained", got)
	}
	if g.Admit() {
		t.Fatal("Admit succeeded after drain")
	}
}
