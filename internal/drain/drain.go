// Package drain tracks in-flight work through a shutdown: admit work until
// stopped, then wait for what was admitted to finish.
package drain

import (
	"context"
	"sync"
)

// State is a Gate's lifecycle stage.
type State int

const (
	// Admitting accepts new work.
	Admitting State = iota
	// Draining refuses new work while admitted work is still in flight.
	Draining
	// Drained refuses new work and has none in flight. It is final.
	Drained
)

func (s State) String() string {
	switch s {
	case Admitting:
		return "admitting"
	case Draining:
		return "draining"
	case Drained:
		return "drained"
	}
	return "unknown"
}

type event int

const (
	admit event = iota
	release
	stop
)

// Gate counts admitted work. The zero value is Admitting with nothing in
// flight. Every Admit that returns true must be paired with one Release.
type Gate struct {
	mu       sync.Mutex
	state    State
	inflight int
	// zero is closed while inflight is 0; nil until the first Admit.
	zero chan struct{}
}

// Admit reserves a slot for one unit of work. False once stopped.
func (g *Gate) Admit() bool { return g.apply(admit) }

// Release ends one admitted unit. It panics without a matching Admit.
func (g *Gate) Release() { g.apply(release) }

// Stop refuses every later Admit. True only for the call that stopped it.
func (g *Gate) Stop() bool { return g.apply(stop) }

// State reports the current stage.
func (g *Gate) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Stopped reports whether Stop has been called.
func (g *Gate) Stopped() bool { return g.State() != Admitting }

// Wait blocks until nothing is in flight or ctx ends. It does not stop the
// gate: work admitted while Admitting extends the wait.
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.inflight == 0 {
		g.mu.Unlock()
		return nil
	}
	zero := g.zero
	g.mu.Unlock()
	select {
	case <-zero:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// apply is the only place the gate's state changes. It reports whether the
// event took effect.
func (g *Gate) apply(e event) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch e {
	case admit:
		if g.state != Admitting {
			return false
		}
		if g.inflight == 0 {
			g.zero = make(chan struct{})
		}
		g.inflight++
		return true
	case release:
		if g.inflight == 0 {
			panic("drain: Release without a matching Admit")
		}
		g.inflight--
		if g.inflight == 0 {
			close(g.zero)
			if g.state == Draining {
				g.state = Drained
			}
		}
		return true
	case stop:
		if g.state != Admitting {
			return false
		}
		if g.inflight == 0 {
			g.state = Drained
		} else {
			g.state = Draining
		}
		return true
	}
	panic("drain: unknown event")
}
