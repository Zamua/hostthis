package relay

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// bindFake admits a key and joins a fake connection so the hub holds a real
// (non-reservation) Conn a broadcast can deliver to.
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
	// The returned snapshot is the caller's first wire frame in production;
	// these tests assert broadcast delivery only, so it is dropped.
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
	// The cap is per app, so a different app is unaffected.
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
	// A second DISTINCT room would create a new hub past the cap.
	k2 := testKey()
	if _, _, err := r.admit(k2); !errors.Is(err, ErrTooManyRooms) {
		t.Fatalf("admit a 2nd room err = %v, want ErrTooManyRooms", err)
	}
	// MaxRooms caps hubs, not joins: an already-live room still admits.
	if _, _, err := r.admit(k1); err != nil {
		t.Fatalf("join to live room err = %v, want nil (joins not capped by MaxRooms)", err)
	}
}

// TestRegistry_HubLookupBroadcastReachesBoundConn pins the live-mirror fan-out
// path: a durable write's mirror reaches a room's bound connection via
// r.hub(key) + broadcast, the fan-out commitAndMirror performs after a commit.
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
	// A second release must not over-decrement the per-app counter, which
	// would wedge the app below its cap.
	r.release(key, id)
	if r.AppConns(key.App) != 0 {
		t.Fatalf("per-app count = %d after double release, want 0", r.AppConns(key.App))
	}
}

// TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit pins that the
// admission path is decoupled from an in-flight durable commit: while a slow
// commit on room A is parked, an admit to A and an admit to a different room B
// must both complete promptly. The spec forbids storage I/O under any hub lock,
// so the commit holds no lock and neither admit has anything to wait on. The
// same-room fan-out half is pinned by
// TestCommitAndMirror_SlowCommitDoesNotBlockRoom.
//
// Fails if commitAndMirror takes A's hub lock before running commit(): admit(A)
// then blocks behind the parked commit and misses the deadline.
func TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit(t *testing.T) {
	r := NewRegistry(NewLimits())
	keyA := testKey()
	keyB := testKey()

	// Bind a connection to room A so its hub exists.
	bindFake(t, r, keyA)

	// A commitAndMirror on A whose commit blocks until released below.
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
// last-leave-vs-admit race and asserts no slot leaks and the join lands in the
// live mapped hub:
//
//  1. A connection is bound to the room, so the hub exists holding one conn.
//  2. A second admit reserves its per-app slot and the pending-admit guard
//     under r.mu, releases r.mu, and parks in the afterAdmitReserve seam:
//     reserved but not yet registered.
//  3. From inside the seam the FIRST connection releases, emptying the hub so
//     onEmpty fires removeHub against the very hub the admit will register
//     into. Only pending>0 keeps that hub alive.
//  4. The seam returns, the admit registers, then the join completes.
//
// Without the guard the join lands in an orphaned (unmapped) hub: r.hub(key) is
// nil, live mirrors never reach the connection, and its release finds no hub so
// the per-app slot leaks until process restart.
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

	// Steps 2 and 4: admit reserves, the seam empties the room, register runs.
	_, id, admitErr := r.admit(key)
	if admitErr != nil {
		t.Fatalf("round %d: admit: %v", round, admitErr)
	}
	r.afterAdmitReserve = nil

	c := newFakeConn(id)
	_, joinErr := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte("snap")}, nil },
	)

	// A failed join (errHubGone: the hub was torn out) still runs the relay's
	// deferred release, which must reclaim the slot. A release that finds
	// r.hubs[key] nil and skips decApp leaves AppConns at 1: the leak.
	if joinErr != nil {
		r.release(key, id)
		if got := r.AppConns(key.App); got != 0 {
			t.Fatalf("round %d: join failed (%v) but AppConns = %d after release, want 0 (leaked per-app slot via the last-leave-vs-admit race)", round, joinErr, got)
		}
		t.Fatalf("round %d: join returned %v - the hub was orphaned by the last leave's removeHub", round, joinErr)
	}

	// The connection must be in the LIVE mapped hub: an orphaned hub makes
	// r.hub(key) nil and the frame never arrives.
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

	// The per-app slot must return to 0: an orphaned hub makes release skip
	// decApp and leak it.
	r.release(key, id)
	if got := r.AppConns(key.App); got != 0 {
		t.Fatalf("round %d: AppConns = %d after release, want 0 (leaked per-app slot via the last-leave-vs-admit race)", round, got)
	}
	if got := r.Rooms(); got != 0 {
		t.Fatalf("round %d: rooms = %d after every release, want 0 (hub leaked)", round, got)
	}
}

// TestRegistry_LastLeaveDuringAdmitDoesNotOrphanJoin pins the pending-admit
// guard: admit increments r.pending[key] under r.mu before releasing it and
// decrements it after register returns, and removeHub deletes a hub only when
// it is empty AND has zero pending admits.
//
// Fails if removeHub drops its `r.pending[key] == 0` condition. The room's last
// connection then leaves inside the window where an admit has reserved but not
// registered, tearing out the hub the join is about to enter: live mirrors
// never reach it (r.hub(key) is nil) and its release leaks the per-app slot,
// eroding MaxConnsPerApp until the process restarts.
//
// Many rounds, each a fresh room; run under -race to also catch a data race in
// the guard's own bookkeeping.
func TestRegistry_LastLeaveDuringAdmitDoesNotOrphanJoin(t *testing.T) {
	const rounds = 500
	for i := range rounds {
		runLastLeaveVsAdmitRound(t, i)
	}
}

// TestRegistry_CommitOnRoomWithNoSubscribersLeavesNoHub pins that a durable
// commit on a room with NO live connections neither creates nor leaks a hub:
// with no local subscribers the fan-out is skipped and the mirror is returned
// for the peer publish. A joiner racing such a commit is caught up by its
// snapshot's exact S, not by a hub-side artifact.
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

// TestRegistry_LaggardDropReclaimsAppSlotWithSurvivor pins the accounting when
// a broadcast DROPS a laggard while a survivor keeps the hub alive: the
// laggard's per-app slot is reclaimed by the hub's onDrop -> decApp, and the
// laggard's own later release must NOT decrement a second time. A release that
// keys its decApp off "is the id still in the hub" leaks one per-app slot per
// dropped connection, since the drop already cleared it.
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

	// The broadcast drops lag (removed from the hub and closed) and onDrop
	// reclaims its per-app slot. The survivor keeps the hub mapped.
	lag.setFull(true)
	r.hub(key).broadcast(0, Frame{Data: []byte("x")})
	if r.AppConns(key.App) != 1 {
		t.Fatalf("after laggard drop AppConns = %d, want 1 (survivor only)", r.AppConns(key.App))
	}

	// The dropped connection's own teardown calls release, which must be a
	// no-op for accounting: onDrop already did the decApp.
	r.release(key, lagID)
	if r.AppConns(key.App) != 1 {
		t.Fatalf("after the dropped conn's release AppConns = %d, want 1 (no double-decrement)", r.AppConns(key.App))
	}

	r.release(key, survID)
	if r.AppConns(key.App) != 0 {
		t.Fatalf("after survivor release AppConns = %d, want 0", r.AppConns(key.App))
	}
	if r.Rooms() != 0 {
		t.Fatalf("rooms = %d after all leave, want 0 (empty hub leaked)", r.Rooms())
	}
}
