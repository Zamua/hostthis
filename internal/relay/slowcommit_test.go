package relay

import (
	"strings"
	"testing"
	"time"
)

// waitForFrameContaining polls c until a received frame contains sub, or
// fails after d with why. Polling (not blocking on the conn) keeps the
// assertion deterministic even when the path under test would deadlock:
// the poll times out and reports instead of hanging the suite.
func waitForFrameContaining(t *testing.T, c *fakeConn, sub string, d time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, f := range c.recv() {
			if strings.Contains(string(f.Data), sub) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no frame containing %q within %s: %s", sub, d, why)
}

// TestCommitAndMirror_SlowCommitDoesNotBlockRoom pins the locking contract
// the spec requires (SPEC "Persistence and late-join": "the durable commit
// does NOT run under the room's hub lock: the hub lock guards the
// connection set only, and a room's live broadcasts never stall behind a
// durable write's object-storage round trip"). A commit callback that
// PARKS (standing in for a slow object-store CAS - 110ms to 1.5s in
// production, unbounded when the store hangs) must not stall:
//
//   - the room's EPHEMERAL fan-out (a raw relay frame broadcast while the
//     commit is in flight must deliver promptly),
//   - the peer-receive path (DeliverFromPeer is the same broadcast), and
//   - a concurrent JOIN to the same room (admit + snapshot + register).
//
// After the parked commit is released, its mirror still fans out (the
// commit-then-broadcast order is preserved; ordering ACROSS writers rides
// the seq, not a lock).
//
// WEAKEN DEMO (the pre-fix shape): make commitAndMirror take the hub lock
// before running commit() (the old one-critical-section form). RED: every
// leg times out - "ephemeral broadcast stalled behind an in-flight durable
// commit". The off-lock commit is green.
func TestCommitAndMirror_SlowCommitDoesNotBlockRoom(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	c, _ := bindFake(t, r, key)

	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	commitDone := make(chan struct{})
	go func() {
		defer close(commitDone)
		_, _ = r.commitAndMirror(key, func() (Frame, error) {
			close(commitEntered)
			<-releaseCommit
			return Frame{Data: []byte(`{"type":"put","seq":1,"key":"k","value":"v"}`)}, nil
		})
	}()
	<-commitEntered // the durable commit is now parked in flight

	// (a) Ephemeral fan-out while the commit is parked. Broadcast from a
	// goroutine so a regression (broadcast blocking on a lock the commit
	// holds) times the poll out instead of deadlocking the test.
	go func() {
		if h := r.hub(key); h != nil {
			h.broadcast(0, Frame{Data: []byte("ephemeral-during-commit")})
		}
	}()
	waitForFrameContaining(t, c, "ephemeral-during-commit", 2*time.Second,
		"ephemeral broadcast stalled behind an in-flight durable commit")

	// (b) The peer-receive path is the same broadcast, via the public
	// DeliverFromPeer entry (what the gRPC receiver calls).
	rl := &Relay{reg: r}
	go rl.DeliverFromPeer(key, Frame{Data: []byte("peer-frame-during-commit")})
	waitForFrameContaining(t, c, "peer-frame-during-commit", 2*time.Second,
		"peer-delivered frame stalled behind an in-flight durable commit")

	// (c) A join to the SAME room completes while the commit is parked.
	joinDone := make(chan error, 1)
	go func() {
		_, id2, err := r.admit(key)
		if err != nil {
			joinDone <- err
			return
		}
		c2 := newFakeConn(id2)
		_, err = r.joinWithSnapshot(key, c2,
			func() (Frame, error) { return Frame{Data: []byte(`{"type":"snapshot","seq":0,"state":{}}`)}, nil },
		)
		joinDone <- err
	}()
	select {
	case err := <-joinDone:
		if err != nil {
			t.Fatalf("join during in-flight commit failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("join to the room stalled behind an in-flight durable commit")
	}

	// Release the commit: its mirror still fans out, after the write.
	close(releaseCommit)
	<-commitDone
	waitForFrameContaining(t, c, `"key":"k"`, 2*time.Second,
		"the released commit's mirror frame never fanned out")
}
