package storage_test

// Backend-agnostic conformance for the paste STATUS lifecycle.
//
// This closes a hole the suite carried: MarkReady, MarkFailed and
// PasteStatusPending appeared nowhere in conformance_test.go,
// conformance_sites_test.go or conformance_rooms_test.go, so a backend could
// pass every suite while implementing the status machine wrongly, or not at
// all. That machine is not decoration - it is what makes a create observable as
// finished, and what stops a late finalizer resurrecting a paste the reconciler
// already failed.
//
// The properties are stated in OBSERVABLE terms, not in either write protocol's
// vocabulary. An adapter that commits READY in one shot and an adapter that
// commits PENDING and transitions must both satisfy them; how a backend gets
// there is its own business (docs/SPEC.md "Pending-collapse").
//
// House rules match conformance_test.go: reach the backend only through the
// port, fixed slugs and clock, and pin surprising behaviour as-is with a
// comment rather than "fixing" it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// lifecycleRepo is the NARROW surface these assertions actually touch, rather
// than the whole lifecycleRepo. Stating it separately is what lets a partial
// adapter be gated by this suite before it implements every port method: a
// backend under construction can satisfy the create lifecycle and be held to it
// immediately, instead of the suite being unusable until the last method lands.
type lifecycleRepo interface {
	InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
	MarkReady(domain.Paste) error
	MarkFailed(domain.Paste) error
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
}

func lifecyclePaste(t *testing.T, r lifecycleRepo, slug, identity string, size int) domain.Paste {
	t.Helper()
	p := pasteOf(slug, identity, size)
	p.Status = domain.PasteStatusPending
	insert(t, r, p)
	return p
}

func statusOf(t *testing.T, r lifecycleRepo, slug domain.Slug) domain.PasteStatus {
	t.Helper()
	got, err := r.Get(slug)
	if err != nil {
		t.Fatalf("get %s: %v", slug, err)
	}
	return got.Status
}

// A paste inserted PENDING is readable as PENDING. The status is persisted
// state, not something derived at read time.
func conformPendingIsObservable(t *testing.T, r lifecycleRepo) {
	p := lifecyclePaste(t, r, "lc123456", "key:lifecycle", 100)
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusPending {
		t.Fatalf("status after pending insert = %q; want %q", got, domain.PasteStatusPending)
	}
}

// The forward transition every create must be able to reach.
func conformPendingReachesReady(t *testing.T, r lifecycleRepo) {
	p := lifecyclePaste(t, r, "lc223456", "key:lifecycle", 100)
	if err := r.MarkReady(p); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusReady {
		t.Fatalf("status after MarkReady = %q; want %q", got, domain.PasteStatusReady)
	}
}

// The failure transition, and its side effect: a failed paste stops charging
// the owner. Without the release, a backend could report the right status while
// silently holding quota an owner cannot free.
func conformPendingReachesFailedAndReleasesQuota(t *testing.T, r lifecycleRepo) {
	const owner = "key:lifecycle-quota"
	p := lifecyclePaste(t, r, "lc323456", owner, 700)

	before, err := r.SumActiveBytesByOwner(owner, fixedNow)
	if err != nil {
		t.Fatalf("sum before: %v", err)
	}
	if before != 700 {
		t.Fatalf("charged bytes before failure = %d; want 700, a pending paste charges immediately", before)
	}
	if err := r.MarkFailed(p); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusFailed {
		t.Fatalf("status after MarkFailed = %q; want %q", got, domain.PasteStatusFailed)
	}
	after, err := r.SumActiveBytesByOwner(owner, fixedNow)
	if err != nil {
		t.Fatalf("sum after: %v", err)
	}
	if after != 0 {
		t.Fatalf("charged bytes after failure = %d; want 0, the reservation is released", after)
	}
}

// The property that makes the whole machine safe: a paste that has FAILED
// cannot be walked back to READY. A finalizer that was slow enough for the
// reconciler to age its paste out must not resurrect it, or a reader sees a
// paste whose bytes were already reclaimed.
func conformFailedIsTerminal(t *testing.T, r lifecycleRepo) {
	p := lifecyclePaste(t, r, "lc423456", "key:lifecycle", 100)
	if err := r.MarkFailed(p); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	// Not an error: a late finalizer calling this is a race, not a fault.
	if err := r.MarkReady(p); err != nil {
		t.Fatalf("MarkReady on a failed paste = %v; want nil, a late finalizer is a race not a fault", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusFailed {
		t.Fatalf("status = %q after MarkReady on a FAILED paste; want it to stay %q",
			got, domain.PasteStatusFailed)
	}
}

// Repetition is harmless in both directions. Two resolvers may race, and a
// retry must not turn a settled paste into an error.
func conformStatusTransitionsAreIdempotent(t *testing.T, r lifecycleRepo) {
	p := lifecyclePaste(t, r, "lc523456", "key:lifecycle", 100)
	for i := range 3 {
		if err := r.MarkReady(p); err != nil {
			t.Fatalf("MarkReady #%d: %v", i, err)
		}
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusReady {
		t.Fatalf("status after repeated MarkReady = %q; want %q", got, domain.PasteStatusReady)
	}
	// A READY paste does not fall back to FAILED either: only a PENDING paste
	// is still in flight, so this is the same guard as FailedIsTerminal from
	// the other side.
	if err := r.MarkFailed(p); err != nil {
		t.Fatalf("MarkFailed on a ready paste = %v; want nil", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusReady {
		t.Fatalf("status = %q after MarkFailed on a READY paste; want it to stay %q",
			got, domain.PasteStatusReady)
	}
}

// MarkReady on a paste that is gone, absent or under another generation,
// reports not found so a finalizer can discard the bytes it wrote. MarkFailed
// there is a no-op, and neither transition invents or touches a paste.
func conformStatusOnGonePaste(t *testing.T, r lifecycleRepo) {
	missing := domain.Paste{Slug: "lc623456", Generation: "generation-missing"}
	if err := r.MarkReady(missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("MarkReady on a missing paste = %v; want ErrNotFound", err)
	}
	if err := r.MarkFailed(missing); err != nil {
		t.Fatalf("MarkFailed on a missing paste = %v; want nil", err)
	}
	if _, err := r.Get(missing.Slug); err == nil {
		t.Fatal("a status transition created a paste that was never inserted")
	}

	p := lifecyclePaste(t, r, "lc823456", "key:lifecycle", 100)
	other := p
	other.Generation = "generation-other"
	if err := r.MarkReady(other); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("MarkReady under another generation = %v; want ErrNotFound", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusPending {
		t.Fatalf("status = %q after MarkReady under another generation; want %q", got, domain.PasteStatusPending)
	}
}

// A paste committed READY outright is a legal starting state, not only an
// endpoint reached from PENDING. This is what lets an adapter that binds bytes
// inside the metadata commit skip the pending window entirely while satisfying
// the same contract.
func conformReadyAtInsertIsLegal(t *testing.T, r lifecycleRepo) {
	p := pasteOf("lc723456", "key:lifecycle", 100)
	p.Status = domain.PasteStatusReady
	insert(t, r, p)
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusReady {
		t.Fatalf("status after a ready insert = %q; want %q", got, domain.PasteStatusReady)
	}
	if err := r.MarkReady(p); err != nil {
		t.Fatalf("MarkReady on an already-ready paste = %v; want nil", err)
	}
	if got := statusOf(t, r, p.Slug); got != domain.PasteStatusReady {
		t.Fatalf("status = %q; want it to stay %q", got, domain.PasteStatusReady)
	}
}

// runLifecycleConformance is the entry point, called from the same place the
// other suites are.
func runLifecycleConformance(t *testing.T, name string, newRepo func(t *testing.T) lifecycleRepo) {
	t.Helper()
	t.Run(name+"/PendingIsObservable", func(t *testing.T) { conformPendingIsObservable(t, newRepo(t)) })
	t.Run(name+"/PendingReachesReady", func(t *testing.T) { conformPendingReachesReady(t, newRepo(t)) })
	t.Run(name+"/PendingReachesFailedAndReleasesQuota", func(t *testing.T) {
		conformPendingReachesFailedAndReleasesQuota(t, newRepo(t))
	})
	t.Run(name+"/FailedIsTerminal", func(t *testing.T) { conformFailedIsTerminal(t, newRepo(t)) })
	t.Run(name+"/StatusTransitionsAreIdempotent", func(t *testing.T) {
		conformStatusTransitionsAreIdempotent(t, newRepo(t))
	})
	t.Run(name+"/StatusOnGonePaste", func(t *testing.T) { conformStatusOnGonePaste(t, newRepo(t)) })
	t.Run(name+"/ReadyAtInsertIsLegal", func(t *testing.T) { conformReadyAtInsertIsLegal(t, newRepo(t)) })
}
