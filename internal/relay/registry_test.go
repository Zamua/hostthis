package relay

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// bindFake admits a key and joins a fake connection (with an empty
// snapshot) so the hub holds a real (non-reservation) Conn the broadcast
// can deliver to. Returns the id for later release.
func bindFake(t *testing.T, r *Registry, key RoomKey) (*fakeConn, uint64) {
	t.Helper()
	_, id, err := r.admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	c := newFakeConn(id)
	if _, err := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte(`{"type":"snapshot","state":{}}`)}, nil },
	); err != nil {
		t.Fatalf("join: %v", err)
	}
	// (The returned snapshot is the caller's first wire frame in production;
	// the fake-conn tests assert broadcast delivery only, so it is dropped.)
	return c, id
}

func TestRegistry_LazyCreateAndTeardown(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	if r.Rooms() != 0 {
		t.Fatalf("fresh registry has %d rooms, want 0", r.Rooms())
	}
	_, id := bindFake(t, r, key)
	if r.Rooms() != 1 {
		t.Fatalf("after first admit, rooms = %d, want 1", r.Rooms())
	}
	r.release(key, id)
	if r.Rooms() != 0 {
		t.Fatalf("after last release, rooms = %d, want 0 (empty hub should be torn down)", r.Rooms())
	}
	if r.AppConns(key.App) != 0 {
		t.Fatalf("per-app count = %d after release, want 0", r.AppConns(key.App))
	}
}

func TestRegistry_PerRoomCapRefuses(t *testing.T) {
	lim := NewLimits()
	lim.MaxConnsPerRoom = 2
	r := NewRegistry(lim)
	key := testKey()
	bindFake(t, r, key)
	bindFake(t, r, key)
	if _, _, err := r.admit(key); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("third admit err = %v, want ErrRoomFull", err)
	}
}

func TestRegistry_PerAppCapRefuses(t *testing.T) {
	lim := NewLimits()
	lim.MaxConnsPerApp = 2
	lim.MaxConnsPerRoom = 0 // isolate the app cap
	r := NewRegistry(lim)
	app := domain.Slug("appz2345")
	k1 := RoomKey{App: app, ID: domain.NewRoomID()}
	k2 := RoomKey{App: app, ID: domain.NewRoomID()}
	bindFake(t, r, k1)
	bindFake(t, r, k2) // 2 conns across two rooms of the same app
	k3 := RoomKey{App: app, ID: domain.NewRoomID()}
	if _, _, err := r.admit(k3); !errors.Is(err, ErrAppFull) {
		t.Fatalf("third admit (same app) err = %v, want ErrAppFull", err)
	}
	// A different app is unaffected.
	other := RoomKey{App: domain.Slug("otherapp"), ID: domain.NewRoomID()}
	if _, _, err := r.admit(other); err != nil {
		t.Fatalf("admit under a different app err = %v, want nil", err)
	}
}

func TestRegistry_TotalRoomsCapRefusesNewHubButAllowsJoin(t *testing.T) {
	lim := NewLimits()
	lim.MaxRooms = 1
	lim.MaxConnsPerRoom = 0
	lim.MaxConnsPerApp = 0
	r := NewRegistry(lim)
	k1 := testKey()
	bindFake(t, r, k1)
	// A second DISTINCT room would create a new hub past the cap: 503.
	k2 := testKey()
	if _, _, err := r.admit(k2); !errors.Is(err, ErrTooManyRooms) {
		t.Fatalf("admit a 2nd room err = %v, want ErrTooManyRooms", err)
	}
	// A JOIN to the already-live room still succeeds (no new hub).
	if _, _, err := r.admit(k1); err != nil {
		t.Fatalf("join to live room err = %v, want nil (joins not capped by MaxRooms)", err)
	}
}

// TestRegistry_HubLookupBroadcastReachesBoundConn pins the live-mirror fan-out
// path: a durable write's mirror reaches a room's bound connection via
// r.hub(key) + broadcast (the fan-out commitAndMirror performs after a commit).
func TestRegistry_HubLookupBroadcastReachesBoundConn(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	c, _ := bindFake(t, r, key)
	if h := r.hub(key); h != nil {
		h.broadcast(0, Frame{Data: []byte("mirror")})
	} else {
		t.Fatal("hub(key) returned nil for a live room")
	}
	if got := c.recv(); len(got) != 1 || string(got[0].Data) != "mirror" {
		t.Fatalf("bound conn got %v, want one 'mirror' frame", got)
	}
}

func TestRegistry_CloseAllRefusesNewAdmits(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	bindFake(t, r, key)
	r.CloseAll()
	if _, _, err := r.admit(testKey()); err == nil {
		t.Fatal("admit after CloseAll succeeded, want refusal")
	}
}

func TestRegistry_ReleaseIsIdempotent(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	c, id := bindFake(t, r, key)
	_ = c
	r.release(key, id)
	// A second release must not over-decrement the per-app counter into a
	// wedged state (the per-app map guards against going negative).
	r.release(key, id)
	if r.AppConns(key.App) != 0 {
		t.Fatalf("per-app count = %d after double release, want 0", r.AppConns(key.App))
	}
}

// TestRegistry_LaggardDropReclaimsAppSlotWithSurvivor pins the no-leak fix:
// when broadcast DROPS a laggard while a survivor keeps the hub alive, the
// laggard's per-app slot must be reclaimed (via the hub's onDrop -> decApp),
// AND the laggard's own later release must NOT decrement a second time. The
// bug this guards: release keyed its decApp off "is the id still in the hub,"
// which a laggard drop has already cleared, so the per-app counter leaked one
// slot per dropped connection. The multi-client churn integration test
// surfaced it; this is its deterministic unit pin.
// TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit pins that the
// admission path is fully decoupled from an in-flight durable commit: while
// a slow commit on room A is parked, an admit to A itself AND an admit to a
// different room B must both complete promptly. Under the spec's locking
// (no storage I/O under any hub lock) the commit holds NO lock at all, so
// neither admit has anything to wait on. The same-room fan-out half of this
// property is pinned by TestCommitAndMirror_SlowCommitDoesNotBlockRoom.
//
// WEAKEN DEMO: make commitAndMirror take A's hub lock before running
// commit() (the retired one-critical-section shape). RED: admit(A) blocks
// on A's hub lock behind the parked commit and misses the deadline.
func TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit(t *testing.T) {
	r := NewRegistry(NewLimits())
	keyA := testKey()
	keyB := testKey()

	// Bind a connection to room A so its hub exists.
	bindFake(t, r, keyA)

	// Start a commitAndMirror on A whose commit blocks until we release it.
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	var commitWG sync.WaitGroup
	commitWG.Go(func() {
		_, _ = r.commitAndMirror(keyA, func() (Frame, error) {
			close(commitEntered)
			<-releaseCommit
			return Frame{Data: []byte("mirror")}, nil
		})
	})
	<-commitEntered // the slow commit on A is now parked in flight.

	// Both admits - to the SAME room and to a DIFFERENT room - must complete
	// promptly while the commit is parked.
	for _, tc := range []struct {
		name string
		key  RoomKey
	}{{"same room", keyA}, {"different room", keyB}} {
		done := make(chan error, 1)
		go func() {
			_, _, err := r.admit(tc.key)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("admit(%s) returned an error: %v", tc.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("admit(%s) did not complete while a slow commit on room A was in flight: the admission path is coupled to the durable commit", tc.name)
		}
	}

	close(releaseCommit)
	commitWG.Wait()
}

// runLastLeaveVsAdmitRound drives ONE deterministic occurrence of the
// last-leave-vs-admit race and asserts no slot leaks and the join lands in
// the live mapped hub:
//
//  1. A connection is bound to the room (the hub exists, holding one conn).
//  2. A second admit reserves its per-app slot + the pending-admit guard
//     under r.mu, releases r.mu, and parks in the afterAdmitReserve seam -
//     reserved but not yet registered.
//  3. From inside the seam, the FIRST connection releases: the hub empties,
//     onEmpty fires removeHub. On pre-guard code removeHub sees len()==0 and
//     deletes the hub the admit is about to register into; with the
//     pending-admit guard, pending>0 keeps it.
//  4. The seam returns and the admit registers its reservation, then the
//     join completes.
//
// On the buggy (guard-less) code the join lands in an orphaned (unmapped)
// hub: r.hub(key) is nil, live mirrors never reach the connection, and its
// release finds no hub so the per-app slot leaks until process restart.
func runLastLeaveVsAdmitRound(t *testing.T, round int) {
	t.Helper()
	r := NewRegistry(NewLimits())
	key := testKey()

	// Step 1: the room has one live connection.
	_, firstID := bindFake(t, r, key)

	// Step 3, armed to fire from inside step 2's reserved-but-not-registered
	// window: the last connection leaves, so onEmpty -> removeHub runs while
	// the admit is parked between its r.mu reservation and its register.
	r.afterAdmitReserve = func(k RoomKey) {
		r.release(key, firstID)
	}

	// Step 2 + 4: admit reserves, the seam empties the room, then the
	// register runs.
	_, id, admitErr := r.admit(key)
	if admitErr != nil {
		t.Fatalf("round %d: admit: %v", round, admitErr)
	}
	r.afterAdmitReserve = nil

	c := newFakeConn(id)
	_, joinErr := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte("snap")}, nil },
	)

	// If the join failed (errHubGone on the buggy code: the hub was torn out),
	// the relay's deferred teardown still calls release - which must reclaim the
	// slot. On the buggy code release finds r.hubs[key] nil and skips decApp, so
	// AppConns stays at 1: the leak.
	if joinErr != nil {
		r.release(key, id)
		if got := r.AppConns(key.App); got != 0 {
			t.Fatalf("round %d: join failed (%v) but AppConns = %d after release, want 0 (leaked per-app slot via the last-leave-vs-admit race)", round, joinErr, got)
		}
		t.Fatalf("round %d: join returned %v - the hub was orphaned by the last leave's removeHub", round, joinErr)
	}

	// The join succeeded. The connection must be in the LIVE mapped hub: a
	// broadcast through r.hub(key) must reach it. If the hub was orphaned,
	// r.hub(key) is nil and the frame never arrives.
	if h := r.hub(key); h != nil {
		h.broadcast(0, Frame{Data: []byte("live")})
	} else {
		t.Fatalf("round %d: r.hub(key) is nil for a joined connection - the hub was orphaned by the last leave's removeHub", round)
	}
	gotLive := false
	for _, f := range c.recv() {
		if string(f.Data) == "live" {
			gotLive = true
		}
	}
	if !gotLive {
		t.Fatalf("round %d: joined connection did not receive the broadcast - it is registered into an orphaned (unmapped) hub", round)
	}

	// Release the connection. The per-app slot must return to 0 - if the hub
	// was orphaned, release finds r.hubs[key] nil and skips decApp, leaking.
	r.release(key, id)
	if got := r.AppConns(key.App); got != 0 {
		t.Fatalf("round %d: AppConns = %d after release, want 0 (leaked per-app slot via the last-leave-vs-admit race)", round, got)
	}
	if got := r.Rooms(); got != 0 {
		t.Fatalf("round %d: rooms = %d after every release, want 0 (hub leaked)", round, got)
	}
}

// TestRegistry_LastLeaveDuringAdmitDoesNotOrphanJoin closes the
// last-leave-vs-admit slot-leak race: an admit reserves its per-app slot
// under r.mu but has not yet registered its reservation into the hub (it
// released r.mu before taking hub.mu), while the room's LAST connection
// leaves - firing onEmpty -> removeHub on the very hub the admit is about
// to register into. Without the pending-admit guard the hub is deleted in
// that window and the join registers into an orphaned (unmapped) hub: live
// mirrors never reach it (r.hub(key) is nil) and its release leaks the
// per-app slot (the "hub gone -> no decApp" branch), silently eroding
// MaxConnsPerApp until the process restarts.
//
// The fix is the pending-admit guard: admit increments r.pending[key] under
// r.mu before releasing it and decrements it after register returns;
// removeHub deletes a hub only when it is empty AND has zero pending
// admits, so a hub a join is about to register into is never torn out.
//
// WEAKEN DEMO: drop the `r.pending[key] == 0` condition from removeHub.
// RED: "r.hub(key) is nil for a joined connection - the hub was orphaned by
// the last leave's removeHub". Restoring the guard is green. Run under
// -race; many rounds, each a fresh room, to also catch any data race the
// guard's bookkeeping might introduce.
func TestRegistry_LastLeaveDuringAdmitDoesNotOrphanJoin(t *testing.T) {
	const rounds = 500
	for i := range rounds {
		runLastLeaveVsAdmitRound(t, i)
	}
}

// TestRegistry_CommitOnRoomWithNoSubscribersLeavesNoHub pins that a durable
// commit on a room with NO live connections neither creates nor leaks a
// hub: there are no local subscribers to mirror to, so the local fan-out is
// skipped and the mirror is simply returned (for the peer publish). A
// joiner racing such a commit is caught up by its snapshot's exact S, not
// by a hub-side artifact.
func TestRegistry_CommitOnRoomWithNoSubscribersLeavesNoHub(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	mirror, err := r.commitAndMirror(key, func() (Frame, error) {
		return Frame{Data: []byte("mirror")}, nil
	})
	if err != nil {
		t.Fatalf("commitAndMirror: %v", err)
	}
	if string(mirror.Data) != "mirror" {
		t.Fatalf("mirror = %q, want the commit's frame", mirror.Data)
	}
	if got := r.Rooms(); got != 0 {
		t.Fatalf("rooms = %d after a no-subscriber commit, want 0 (hub leaked)", got)
	}
}

func TestRegistry_LaggardDropReclaimsAppSlotWithSurvivor(t *testing.T) {
	lim := NewLimits()
	lim.SendBuffer = 1 // not used by fakeConn, but keep the intent explicit
	r := NewRegistry(lim)
	key := testKey()

	lag, lagID := bindFake(t, r, key)
	_, survID := bindFake(t, r, key) // survivor keeps the hub from being torn down
	if r.AppConns(key.App) != 2 {
		t.Fatalf("after two joins AppConns = %d, want 2", r.AppConns(key.App))
	}

	// Make lag a laggard and broadcast: lag is dropped (removed from the hub
	// + closed) and onDrop reclaims its per-app slot. The survivor remains, so
	// the hub is NOT removed - exactly the case the buggy release mishandled.
	lag.setFull(true)
	r.hub(key).broadcast(0, Frame{Data: []byte("x")})
	if r.AppConns(key.App) != 1 {
		t.Fatalf("after laggard drop AppConns = %d, want 1 (survivor only)", r.AppConns(key.App))
	}

	// The dropped connection's own teardown now calls release - it must be a
	// no-op for accounting (onDrop already did the decApp).
	r.release(key, lagID)
	if r.AppConns(key.App) != 1 {
		t.Fatalf("after the dropped conn's release AppConns = %d, want 1 (no double-decrement)", r.AppConns(key.App))
	}

	// The survivor leaving returns the room to zero, no leak.
	r.release(key, survID)
	if r.AppConns(key.App) != 0 {
		t.Fatalf("after survivor release AppConns = %d, want 0", r.AppConns(key.App))
	}
	if r.Rooms() != 0 {
		t.Fatalf("rooms = %d after all leave, want 0 (empty hub leaked)", r.Rooms())
	}
}
