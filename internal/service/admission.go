package service

import (
	"context"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// DefaultCreateAdmissionWidth is the number of same-identity creates
// admitted to the metadata commit concurrently (docs/SPEC.md
// "Same-identity create admission: a width-2 gate"). Width 2 keeps the
// lone create - and one overlapping follower - fully concurrent, so the
// common case pays nothing, while capping the same-owner write
// concurrency the storage tier sees: N same-owner commits contending at
// a CAS / transaction boundary amplify each other's retries (every loser
// re-runs a commit), so admission is bounded BEFORE the storage tier
// instead of amplifying inside it.
const DefaultCreateAdmissionWidth = 2

// CreateAdmission is a per-key counting semaphore: each key admits at
// most `width` concurrent holders, further acquirers for the SAME key
// queue (no FIFO fairness guarantee), and distinct keys are fully
// independent. Entries exist only while a key has holders or waiters -
// an idle key holds no memory.
//
// It is a generic keyed gate; the create path keys it by the paste's
// owning identity via GateCreates.
type CreateAdmission struct {
	width int
	mu    sync.Mutex
	keys  map[string]*admissionEntry
}

type admissionEntry struct {
	sem chan struct{} // buffered to width; a token is a held slot
	// refs counts holders + waiters. The entry is removed from the map
	// when it hits zero, so idle keys never leak. refs is guarded by the
	// owning CreateAdmission's mu.
	refs int
}

// NewCreateAdmission returns a gate admitting `width` concurrent holders
// per key. Widths below 1 clamp to 1 (a zero-width gate would deadlock
// every acquirer).
func NewCreateAdmission(width int) *CreateAdmission {
	if width < 1 {
		width = 1
	}
	return &CreateAdmission{width: width, keys: make(map[string]*admissionEntry)}
}

// Acquire admits the caller for key, blocking while `width` holders are
// already inside. It returns the release func the caller must invoke
// (exactly once; extra calls are no-ops) when it leaves the gated
// section. The uncontended path is one mutex-guarded map access plus a
// non-blocking channel send - no goroutine is spawned and no scheduler
// round-trip is taken.
func (a *CreateAdmission) Acquire(key string) (release func()) {
	a.mu.Lock()
	e := a.keys[key]
	if e == nil {
		e = &admissionEntry{sem: make(chan struct{}, a.width)}
		a.keys[key] = e
	}
	e.refs++
	a.mu.Unlock()

	// Take a slot; blocks only when width holders are already inside.
	// refs was incremented above, so the entry cannot be deleted out from
	// under a waiter parked here.
	e.sem <- struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-e.sem
			a.mu.Lock()
			e.refs--
			if e.refs == 0 {
				delete(a.keys, key)
			}
			a.mu.Unlock()
		})
	}
}

// keyCount reports how many keys currently hold gate entries (holders or
// waiters). Test seam for the no-leak pin.
func (a *CreateAdmission) keyCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.keys)
}

// gatedCreateRepo decorates a PasteRepo so InsertWithQuotaCheck - the
// create commit, the one same-owner-storm-amplifying write - passes
// through the per-identity admission gate. Every other method passes
// through untouched (reads, MarkReady/MarkFailed and the update / delete
// paths are not gated). The upload service stays unaware of admission:
// the decorator is wired at the composition root.
type gatedCreateRepo struct {
	PasteRepo
	gate *CreateAdmission
}

// GateCreates wraps repo so create commits are admitted through gate,
// keyed by the paste's owning identity.
func GateCreates(repo PasteRepo, gate *CreateAdmission) PasteRepo {
	return &gatedCreateRepo{PasteRepo: repo, gate: gate}
}

func (g *gatedCreateRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	release := g.gate.Acquire(p.Identity.String())
	defer release()
	return g.PasteRepo.InsertWithQuotaCheck(ctx, p, userCap, now)
}
