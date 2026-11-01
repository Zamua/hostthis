package service

import (
	"context"
	"log"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// DenylistSource reads the operator denylist. internal/worker.DenylistRepo
// satisfies it.
type DenylistSource interface {
	Denylist(ctx context.Context) ([]domain.DenyEntry, error)
}

// DefaultDenylistRefresh is how often a replica re-reads the list, and so how
// long a new ban takes to apply.
const DefaultDenylistRefresh = 30 * time.Second

// DenyGate checks sessions against the last list it loaded (docs/SPEC.md
// "Operator denylist"). Until a refresh succeeds the list is empty: it fails
// open, so a Worker outage does not refuse every session on its own.
type DenyGate struct {
	Source   DenylistSource
	Interval time.Duration
	Logger   *log.Logger
	list     atomic.Pointer[domain.Denylist]
}

func NewDenyGate(src DenylistSource) *DenyGate {
	return &DenyGate{Source: src, Interval: DefaultDenylistRefresh}
}

// Refresh replaces the list with the source's current one. On error the
// previous list stays.
func (g *DenyGate) Refresh(ctx context.Context) error {
	entries, err := g.Source.Denylist(ctx)
	if err != nil {
		return err
	}
	d := domain.NewDenylist(entries)
	g.list.Store(&d)
	return nil
}

// Run refreshes now and then every Interval until ctx ends.
func (g *DenyGate) Run(ctx context.Context) {
	t := time.NewTicker(g.Interval)
	defer t.Stop()
	for {
		if err := g.Refresh(ctx); err != nil && ctx.Err() == nil && g.Logger != nil {
			g.Logger.Printf("denylist: refresh: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check reports whether the session is banned, with the entry's reason.
func (g *DenyGate) Check(owner domain.Owner, addr netip.Addr) (string, bool) {
	if g == nil {
		return "", false
	}
	d := g.list.Load()
	if d == nil {
		return "", false
	}
	return d.Match(owner.Fingerprint(), addr)
}
