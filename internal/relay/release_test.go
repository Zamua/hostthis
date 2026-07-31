package relay

import "testing"

// TestRegistry_LaggardDropDuringReleaseDecrementsAppSlotOnce pins that a
// connection's per-app slot is reclaimed EXACTLY once when a broadcast drops it
// as a laggard while its own release is already in flight. Removal and the
// decision to decrement must be one atomic step: the drop deletes the id under
// the hub lock and reclaims its slot via onDrop, so a release that decided to
// decrement from an earlier presence check decrements a second time, stealing a
// live sibling's per-app slot and letting MaxConnsPerApp drift upward.
//
// The beforeReleaseUnregister seam fires the drop in exactly the window a
// concurrent broadcast would occupy, so the interleave is deterministic rather
// than timing-dependent. Fails if release keys its decApp off a presence check
// taken before the unregister instead of off the unregister's own result.
func TestRegistry_LaggardDropDuringReleaseDecrementsAppSlotOnce(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()

	lag, lagID := bindFake(t, r, key)
	_, survID := bindFake(t, r, key) // survivor keeps the hub mapped
	if got := r.AppConns(key.App); got != 2 {
		t.Fatalf("after two joins AppConns = %d, want 2", got)
	}

	// Armed to fire from inside release, after it has observed the id and
	// before it unregisters: the broadcast finds lag full, drops it under the
	// hub lock and reclaims its per-app slot via onDrop.
	r.beforeReleaseUnregister = func(k RoomKey, id uint64) {
		r.beforeReleaseUnregister = nil
		lag.setFull(true)
		r.hub(k).broadcast(0, Frame{Data: []byte("x")})
	}

	r.release(key, lagID)

	if got := r.AppConns(key.App); got != 1 {
		t.Fatalf("AppConns = %d after a laggard drop landed inside its own release, want 1 (the survivor): the connection's per-app slot was decremented twice", got)
	}

	r.release(key, survID)
	if got := r.AppConns(key.App); got != 0 {
		t.Fatalf("AppConns = %d after every connection left, want 0", got)
	}
	if got := r.Rooms(); got != 0 {
		t.Fatalf("rooms = %d after every connection left, want 0 (empty hub leaked)", got)
	}
}
