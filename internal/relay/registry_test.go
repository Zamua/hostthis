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
	if err := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte(`{"type":"snapshot","state":{}}`)}, nil },
		func(f Frame) bool { return c.Send(f) },
	); err != nil {
		t.Fatalf("join: %v", err)
	}
	// Drain the snapshot frame the join queued, so the test's broadcast
	// assertions see only the frames they send.
	_ = c.recv()
	c.mu.Lock()
	c.received = nil
	c.mu.Unlock()
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
// TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit pins the per-room
// isolation the admission path must deliver: a join to one room (blocked behind
// that room's hub lock because a slow durable commit on it is in flight) must
// NOT stall a concurrent join to a DIFFERENT room. The slow path is
// commitAndMirror on room A, which holds A's hub lock across the (here,
// blocking) commit; a second admit to A then blocks on A's hub lock; the test
// asserts an admit to room B completes promptly anyway.
//
// WEAKEN DEMO: hold r.mu across the whole admit body (defer r.mu.Unlock() + the
// hub.register call under it, the pre-W-P2 shape). RED: admit(B) blocks on r.mu
// behind admit(A), which is itself blocked on A's hub lock behind the slow
// commit, so admit(B) does not return within the deadline. The fix - releasing
// r.mu before the per-room register - is green: admit(B) returns immediately.
func TestRegistry_AdmitDoesNotCoupleAcrossRoomsDuringSlowCommit(t *testing.T) {
	r := NewRegistry(NewLimits())
	keyA := testKey()
	keyB := testKey()

	// Bind a connection to room A so its hub exists and a second admit to A
	// must contend on A's hub lock.
	bindFake(t, r, keyA)

	// Start a commitAndMirror on A whose commit blocks until we release it. It
	// holds A's hub lock for the whole (blocked) commit, the slow-durable-write
	// critical section.
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	var commitWG sync.WaitGroup
	commitWG.Add(1)
	go func() {
		defer commitWG.Done()
		_ = r.commitAndMirror(keyA, func() error {
			close(commitEntered)
			<-releaseCommit
			return nil
		}, Frame{Data: []byte("mirror")})
	}()
	<-commitEntered // A's hub lock is now held by the in-flight slow commit.

	// A second admit to room A will block on A's hub lock (held by the commit).
	var admitAWG sync.WaitGroup
	admitAWG.Add(1)
	go func() {
		defer admitAWG.Done()
		_, _, _ = r.admit(keyA)
	}()

	// Give admit(A) a moment to reach (and block on) A's hub lock. It must be
	// blocked there with r.mu NOT held, so admit(B) below is free.
	time.Sleep(20 * time.Millisecond)

	// The isolation assertion: an admit to a DIFFERENT room must complete
	// promptly, not stall behind A's slow commit + the blocked admit(A).
	doneB := make(chan error, 1)
	go func() {
		_, _, err := r.admit(keyB)
		doneB <- err
	}()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("admit(B) returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admit(B) did not complete while a slow commit on room A held A's hub lock: the admission path couples rooms via the global registry lock")
	}

	// Release the slow commit; both A-side goroutines now unblock.
	close(releaseCommit)
	commitWG.Wait()
	admitAWG.Wait()
}

// TestRegistry_DurableWriteOnEmptyRoomDoesNotLeakSlotAgainstJoin closes the
// transient-hub slot-leak race: a durable commit on a room with NO live
// connections creates a transient hub, commits + mirrors, then removes the hub
// if it is "still empty" - while a concurrent join to the SAME empty room has
// reserved its per-app slot (under r.mu) but has not yet registered into the
// hub (it released r.mu before taking hub.mu). If the transient cleanup deletes
// the hub in that window, the join registers into an orphaned (unmapped) hub:
//
//   - The per-app slot is leaked: when the connection releases, release looks
//     up r.hubs[key], finds nil, and takes the "hub gone -> no decApp" branch,
//     so the slot reserved in admit is never freed. It accumulates silently and
//     erodes MaxConnsPerApp until the process restarts.
//   - The register is orphaned: the connection sits in a hub that is no longer
//     in r.hubs, so a later commitAndMirror / mirror fan-out (which looks up
//     r.hubs[key]) never reaches it - it silently misses live frames.
//
// The fix is the pending-admit guard: admit increments a per-key in-flight
// count under r.mu before releasing it, and decrements it under r.mu after
// register returns; removeHub / commitAndMirror's transient cleanup / onEmpty
// only delete a hub when it is empty AND has zero pending admits, so a hub a
// join is about to register into is never torn out.
//
// RED on the pre-guard code: across enough rounds the transient cleanup wins
// the race, the slot leaks (AppConns stays > 0 after every connection released)
// and/or the joined connection is in an orphaned hub a subsequent broadcast
// cannot reach. GREEN with the guard: AppConns returns to 0 every round and the
// connection always lands in the live mapped hub and receives the broadcast.
// runDurableWriteVsJoinRound drives ONE deterministic occurrence of the
// transient-hub race for a fresh empty room and asserts no slot leaks and the
// join lands in the live mapped hub. It pins the exact interleaving the
// natural-timing race hits only rarely, in the faithful direction (the durable
// commit CREATES the transient hub, the join finds it second):
//
//  1. The durable commit creates the transient hub (created=true), takes its
//     hub lock, and parks inside its commit() callback.
//  2. The join's admit then finds that hub (created=false), reserves its
//     per-app slot + pending guard, releases r.mu, and parks in the
//     afterAdmitReserve seam - reserved but not yet registered.
//  3. The seam releases the commit, which finishes its broadcast and runs its
//     transient cleanup (removeHub) while the join is still parked. On the
//     pre-guard code that cleanup sees len()==0 and deletes the hub the join is
//     about to register into; with the pending-admit guard pending>0 keeps it.
//  4. The seam returns and the join registers, then completes its
//     joinWithSnapshot.
func runDurableWriteVsJoinRound(t *testing.T, round int) {
	t.Helper()
	r := NewRegistry(NewLimits())
	key := testKey()

	commitEntered := make(chan struct{}) // commit created the hub + is in commit()
	releaseCommit := make(chan struct{}) // let the parked commit finish
	commitDone := make(chan struct{})    // commit + its transient cleanup ran

	// Step 1: a durable commit on the EMPTY room. getOrCreateHubLocked creates a
	// transient hub (created=true); we park inside commit() so admit below finds
	// the hub already in the map.
	go func() {
		_ = r.commitAndMirror(key, func() error {
			close(commitEntered)
			<-releaseCommit
			return nil
		}, Frame{Data: []byte("durable")})
		close(commitDone)
	}()
	<-commitEntered // the transient hub now exists; the commit holds its hub lock

	// Step 3 (fired from inside admit's reserved-but-not-registered window):
	// release the parked commit and wait for its transient cleanup (removeHub)
	// to run. This is the worst-case interleaving for the leak.
	r.afterAdmitReserve = func(k RoomKey) {
		close(releaseCommit)
		<-commitDone
	}

	// Step 2 + 4: admit finds the existing hub, reserves, parks in the seam,
	// then registers.
	_, id, admitErr := r.admit(key)
	if admitErr != nil {
		t.Fatalf("round %d: admit: %v", round, admitErr)
	}

	c := newFakeConn(id)
	joinErr := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte("snap")}, nil },
		func(f Frame) bool { return c.Send(f) },
	)

	// If the join failed (errHubGone on the buggy code: the hub was torn out),
	// the relay's deferred teardown still calls release - which must reclaim the
	// slot. On the buggy code release finds r.hubs[key] nil and skips decApp, so
	// AppConns stays at 1: the leak.
	if joinErr != nil {
		r.release(key, id)
		if got := r.AppConns(key.App); got != 0 {
			t.Fatalf("round %d: join failed (%v) but AppConns = %d after release, want 0 (leaked per-app slot via the transient-hub race)", round, joinErr, got)
		}
		t.Fatalf("round %d: join returned %v - the hub was orphaned by the transient cleanup", round, joinErr)
	}

	// The join succeeded. The connection must be in the LIVE mapped hub: a
	// broadcast through r.hub(key) must reach it. If the hub was orphaned,
	// r.hub(key) is nil and the frame never arrives.
	c.mu.Lock()
	c.received = nil
	c.mu.Unlock()
	if h := r.hub(key); h != nil {
		h.broadcast(0, Frame{Data: []byte("live")})
	} else {
		t.Fatalf("round %d: r.hub(key) is nil for a joined connection - the hub was orphaned by the transient cleanup", round)
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
		t.Fatalf("round %d: AppConns = %d after release, want 0 (leaked per-app slot via the transient-hub race)", round, got)
	}
}

// TestRegistry_DurableWriteOnEmptyRoomDoesNotLeakSlotAgainstJoin closes the
// transient-hub slot-leak race: a durable commit on a room with NO live
// connections creates a transient hub, commits + mirrors, then removes the hub
// if it is "still empty" - while a concurrent join to the SAME empty room has
// reserved its per-app slot (under r.mu) but has not yet registered its
// reservation into the hub (it released r.mu before taking hub.mu). If the
// transient cleanup deletes the hub in that window, the join registers into an
// orphaned (unmapped) hub:
//
//   - The per-app slot is leaked: when the connection releases, release looks
//     up r.hubs[key], finds nil, and takes the "hub gone -> no decApp" branch,
//     so the slot reserved in admit is never freed. It accumulates silently and
//     erodes MaxConnsPerApp until the process restarts.
//   - The register is orphaned: the connection sits in a hub no longer in
//     r.hubs, so a later commitAndMirror / mirror fan-out (which looks up
//     r.hubs[key]) never reaches it - it silently misses live frames.
//
// The fix is the pending-admit guard: admit increments r.pending[key] under
// r.mu before releasing it and decrements it after register returns; removeHub
// (and so commitAndMirror's transient cleanup + the onEmpty callback) deletes a
// hub only when it is empty AND has zero pending admits, so a hub a join is
// about to register into is never torn out. RED on the pre-guard code (the join
// fails / the slot leaks / the broadcast is missed); GREEN with the guard. Run
// under -race; many rounds, each a fresh room, to also catch any data race the
// guard's bookkeeping might introduce.
func TestRegistry_DurableWriteOnEmptyRoomDoesNotLeakSlotAgainstJoin(t *testing.T) {
	const rounds = 500
	for i := 0; i < rounds; i++ {
		runDurableWriteVsJoinRound(t, i)
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
