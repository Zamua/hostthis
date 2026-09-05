package storage_test

// Backend-agnostic conformance for the Sybil admission gate. The subnet cap is
// exact under concurrency; the identity reverse index is derived after admission.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

const kgWindow = 24 * time.Hour

type keygateRepo interface {
	AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (bool, error)
	SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error)
	SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error)
}

func kgAdmit(t *testing.T, r keygateRepo, identity, subnet string, at time.Time, limit int) bool {
	t.Helper()
	known, err := r.AdmitNewKey(identity, subnet, at, limit, kgWindow)
	if err != nil {
		t.Fatalf("admit %s on %s: %v", identity, subnet, err)
	}
	return known
}

// A pair is unknown the first time and known every time after.
func conformKeygateFirstAdmitIsFresh(t *testing.T, r keygateRepo) {
	if known := kgAdmit(t, r, "kg:a", "10.1.0.0/24", fixedNow, 20); known {
		t.Fatal("first admit of a pair reported knownAlready=true; a pair the gate has never seen is fresh")
	}
	if known := kgAdmit(t, r, "kg:a", "10.1.0.0/24", fixedNow, 20); !known {
		t.Fatal("re-admit of the same pair reported knownAlready=false; a recorded pair must not be re-charged")
	}
}

// Re-admitting a known pair consumes no budget, so it can be repeated without
// starving the subnet of slots for genuinely new keys.
func conformKeygateKnownPairIsFree(t *testing.T, r keygateRepo) {
	const subnet = "10.2.0.0/24"
	kgAdmit(t, r, "kg:known", subnet, fixedNow, 2)
	for range 5 {
		if known := kgAdmit(t, r, "kg:known", subnet, fixedNow, 2); !known {
			t.Fatal("a repeated admit of a recorded pair reported fresh")
		}
	}
	// One slot of two was spent by the single fresh admit above, so a second
	// distinct key must still fit.
	if known := kgAdmit(t, r, "kg:other", subnet, fixedNow, 2); known {
		t.Fatal("a never-seen key reported knownAlready")
	}
}

// The cap refuses the key past the limit, with the sentinel the service
// classifies on.
func conformKeygateCapRefuses(t *testing.T, r keygateRepo) {
	const subnet, limit = "10.3.0.0/24", 3
	for i := range limit {
		kgAdmit(t, r, "kg:cap"+string(rune('a'+i)), subnet, fixedNow, limit)
	}
	_, err := r.AdmitNewKey("kg:capX", subnet, fixedNow, limit, kgWindow)
	if !errors.Is(err, domain.ErrTooManyNewKeys) {
		t.Fatalf("admit past the cap returned %v; want domain.ErrTooManyNewKeys, which is what the "+
			"service matches on to refuse the session", err)
	}
	// A key already on file is still admitted: the cap gates NEW keys, and
	// locking out established users would be a far worse failure than a Sybil.
	if known := kgAdmit(t, r, "kg:capa", subnet, fixedNow, limit); !known {
		t.Fatal("a recorded key was refused once the subnet hit its cap; the cap gates fresh keys only")
	}
}

// Budgets do not leak between subnets.
func conformKeygateCapIsPerSubnet(t *testing.T, r keygateRepo) {
	const limit = 2
	kgAdmit(t, r, "kg:s1", "10.4.0.0/24", fixedNow, limit)
	kgAdmit(t, r, "kg:s2", "10.4.0.0/24", fixedNow, limit)
	if _, err := r.AdmitNewKey("kg:s3", "10.4.0.0/24", fixedNow, limit, kgWindow); !errors.Is(err, domain.ErrTooManyNewKeys) {
		t.Fatalf("setup: third key on a full subnet should be refused, got %v", err)
	}
	if known := kgAdmit(t, r, "kg:s3", "10.5.0.0/24", fixedNow, limit); known {
		t.Fatal("a fresh key was refused or reported known on an EMPTY subnet while a different " +
			"subnet was full; the budget is per subnet")
	}
}

// The snapshot reports the in-window rows and the oldest stamp among them,
// which is what tells a refused client when the next slot frees.
func conformKeygateSnapshotReportsWindow(t *testing.T, r keygateRepo) {
	const subnet = "10.6.0.0/24"
	first := fixedNow
	kgAdmit(t, r, "kg:snap1", subnet, first, 20)
	kgAdmit(t, r, "kg:snap2", subnet, first.Add(time.Hour), 20)

	n, oldest, err := r.SubnetSnapshot(subnet, first.Add(2*time.Hour), kgWindow)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n != 2 {
		t.Fatalf("snapshot counted %d in-window rows after two admits; want 2", n)
	}
	if !oldest.Equal(first) {
		t.Fatalf("oldest first-seen = %v; want the EARLIER admit at %v. Adding the window to this "+
			"value is how a refused client is told when a slot frees, so the later stamp would "+
			"under-report the wait.", oldest, first)
	}
}

// An empty subnet reports no rows and a zero time, rather than an error or a
// fabricated stamp.
func conformKeygateSnapshotOfEmptySubnet(t *testing.T, r keygateRepo) {
	n, oldest, err := r.SubnetSnapshot("10.7.0.0/24", fixedNow, kgWindow)
	if err != nil {
		t.Fatalf("snapshot of an untouched subnet: %v", err)
	}
	if n != 0 || !oldest.IsZero() {
		t.Fatalf("snapshot of an untouched subnet = (%d, %v); want (0, zero time)", n, oldest)
	}
}

// Rows age out of the window, and the slot they held comes back.
func conformKeygateWindowExpiresRows(t *testing.T, r keygateRepo) {
	const subnet, limit = "10.8.0.0/24", 1
	kgAdmit(t, r, "kg:old", subnet, fixedNow, limit)
	if _, err := r.AdmitNewKey("kg:new", subnet, fixedNow, limit, kgWindow); !errors.Is(err, domain.ErrTooManyNewKeys) {
		t.Fatalf("setup: the single slot should be taken, got %v", err)
	}

	later := fixedNow.Add(kgWindow + time.Minute)
	n, _, err := r.SubnetSnapshot(subnet, later, kgWindow)
	if err != nil {
		t.Fatalf("snapshot past the window: %v", err)
	}
	if n != 0 {
		t.Fatalf("snapshot counted %d rows a window later; want 0. There is no background prune, so "+
			"a row that never stops counting is a permanent lockout for its subnet.", n)
	}
	if known := kgAdmit(t, r, "kg:new", subnet, later, limit); known {
		t.Fatal("a never-seen key reported knownAlready after the window rolled")
	}
}

// The count of networks a key is grandfathered on rises with each new subnet
// and ignores other keys.
func conformKeygateSubnetsForIdentity(t *testing.T, r keygateRepo) {
	const me, them = "kg:multi", "kg:someoneelse"
	if n, err := r.SubnetsForIdentity(me, fixedNow, kgWindow); err != nil || n != 0 {
		t.Fatalf("subnets for an unseen identity = (%d, %v); want (0, nil)", n, err)
	}
	for _, s := range []string{"10.9.0.0/24", "10.10.0.0/24", "10.11.0.0/24"} {
		kgAdmit(t, r, me, s, fixedNow, 20)
		kgAdmit(t, r, them, s, fixedNow, 20)
	}
	n, err := r.SubnetsForIdentity(me, fixedNow, kgWindow)
	if err != nil {
		t.Fatalf("subnets for identity: %v", err)
	}
	if n != 3 {
		t.Fatalf("identity seen on 3 subnets reported %d; a fan-out over subnet rows and a "+
			"maintained reverse index must agree here", n)
	}
	// Re-admitting where it is already known adds no network.
	kgAdmit(t, r, me, "10.9.0.0/24", fixedNow, 20)
	if n, _ := r.SubnetsForIdentity(me, fixedNow, kgWindow); n != 3 {
		t.Fatalf("re-admitting on a known subnet changed the count to %d; want 3", n)
	}
}

// The identity view ages out on the same window as the rows it mirrors.
func conformKeygateSubnetsForIdentityExpires(t *testing.T, r keygateRepo) {
	const me = "kg:expiring"
	kgAdmit(t, r, me, "10.12.0.0/24", fixedNow, 20)
	if n, _ := r.SubnetsForIdentity(me, fixedNow, kgWindow); n != 1 {
		t.Fatalf("setup: want 1 subnet, got %d", n)
	}
	later := fixedNow.Add(kgWindow + time.Minute)
	if n, err := r.SubnetsForIdentity(me, later, kgWindow); err != nil || n != 0 {
		t.Fatalf("subnets a window later = (%d, %v); want (0, nil). The derived view must age out "+
			"on the same window as the authoritative rows or whoami over-reports forever.", n, err)
	}
}

// Re-admitting must not refresh a pair's own stamp, or a key that reconnects
// often would hold its slot indefinitely.
func conformKeygateReadmitDoesNotRefresh(t *testing.T, r keygateRepo) {
	const subnet = "10.13.0.0/24"
	kgAdmit(t, r, "kg:sticky", subnet, fixedNow, 20)
	kgAdmit(t, r, "kg:sticky", subnet, fixedNow.Add(kgWindow/2), 20)

	later := fixedNow.Add(kgWindow + time.Minute)
	n, _, err := r.SubnetSnapshot(subnet, later, kgWindow)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n != 0 {
		t.Fatalf("the row still counted a window after its FIRST sighting (%d rows); a re-admit "+
			"moved the stamp, which lets a reconnecting key hold a slot forever", n)
	}
}

// An aged-out pair is fresh on its next admission regardless of when physical
// deletion occurs; an in-window pair stays known.
func conformKeygateForgetsOutOfWindow(t *testing.T, r keygateRepo) {
	const subnet = "10.15.0.0/24"
	old := fixedNow.Add(-2 * kgWindow)
	kgAdmit(t, r, "kg:stale", subnet, old, 20)
	kgAdmit(t, r, "kg:keep", subnet, fixedNow, 20)
	if known := kgAdmit(t, r, "kg:stale", subnet, fixedNow, 20); known {
		t.Fatal("a pair whose row aged past the window must re-admit as fresh")
	}
	if known := kgAdmit(t, r, "kg:keep", subnet, fixedNow, 20); !known {
		t.Fatal("an in-window pair must stay known")
	}
}

func conformKeygateConcurrentCapIsExact(t *testing.T, r keygateRepo) {
	const subnet, limit, attempts = "10.14.0.0/24", 7, 32
	type result struct {
		known bool
		err   error
	}

	start := make(chan struct{})
	results := make(chan result, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			known, err := r.AdmitNewKey(fmt.Sprintf("kg:race-%02d", i), subnet, fixedNow, limit, kgWindow)
			results <- result{known: known, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	admitted := 0
	refused := 0
	for got := range results {
		switch {
		case got.err == nil && !got.known:
			admitted++
		case errors.Is(got.err, domain.ErrTooManyNewKeys):
			refused++
		default:
			t.Fatalf("concurrent admission returned (known=%v, err=%v)", got.known, got.err)
		}
	}
	if admitted != limit || refused != attempts-limit {
		t.Fatalf("concurrent admissions = %d admitted, %d refused; want %d and %d",
			admitted, refused, limit, attempts-limit)
	}
	n, _, err := r.SubnetSnapshot(subnet, fixedNow, kgWindow)
	if err != nil {
		t.Fatalf("snapshot after concurrent admissions: %v", err)
	}
	if n != limit {
		t.Fatalf("snapshot after concurrent admissions = %d rows; want exact cap %d", n, limit)
	}
}

func runKeygateConformance(t *testing.T, name string, newRepo func(t *testing.T) keygateRepo) {
	t.Helper()
	t.Run(name+"/FirstAdmitIsFresh", func(t *testing.T) { conformKeygateFirstAdmitIsFresh(t, newRepo(t)) })
	t.Run(name+"/KnownPairIsFree", func(t *testing.T) { conformKeygateKnownPairIsFree(t, newRepo(t)) })
	t.Run(name+"/CapRefuses", func(t *testing.T) { conformKeygateCapRefuses(t, newRepo(t)) })
	t.Run(name+"/CapIsPerSubnet", func(t *testing.T) { conformKeygateCapIsPerSubnet(t, newRepo(t)) })
	t.Run(name+"/SnapshotReportsWindow", func(t *testing.T) { conformKeygateSnapshotReportsWindow(t, newRepo(t)) })
	t.Run(name+"/SnapshotOfEmptySubnet", func(t *testing.T) { conformKeygateSnapshotOfEmptySubnet(t, newRepo(t)) })
	t.Run(name+"/WindowExpiresRows", func(t *testing.T) { conformKeygateWindowExpiresRows(t, newRepo(t)) })
	t.Run(name+"/SubnetsForIdentity", func(t *testing.T) { conformKeygateSubnetsForIdentity(t, newRepo(t)) })
	t.Run(name+"/SubnetsForIdentityExpires", func(t *testing.T) {
		conformKeygateSubnetsForIdentityExpires(t, newRepo(t))
	})
	t.Run(name+"/ReadmitDoesNotRefresh", func(t *testing.T) { conformKeygateReadmitDoesNotRefresh(t, newRepo(t)) })
	t.Run(name+"/ForgetsOutOfWindow", func(t *testing.T) { conformKeygateForgetsOutOfWindow(t, newRepo(t)) })
	t.Run(name+"/ConcurrentCapIsExact", func(t *testing.T) { conformKeygateConcurrentCapIsExact(t, newRepo(t)) })
}
