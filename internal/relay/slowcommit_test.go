package relay

import (
	"strings"
	"testing"
	"time"
)

// waitForFrameContaining polls c until a received frame contains sub, or fails
// after d with why. Polling rather than blocking on the conn means a deadlocked
// path under test reports a failure instead of hanging the suite.
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

// TestCommitAndMirror_SlowCommitDoesNotBlockRoom pins the locking contract from
// SPEC "Persistence and late-join": the durable commit does NOT run under the
// room's hub lock, so a parked commit (standing in for a slow or hung
// object-store CAS) must not stall the room's ephemeral fan-out, the
// peer-receive path, or a concurrent join. Once released, the commit's mirror
// still fans out, preserving commit-then-broadcast order.
//
// Fails if commitAndMirror takes the hub lock before running commit(): every
// leg then times out behind the in-flight durable commit.
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
	// goroutine so a broadcast blocking on the commit's lock times the poll
	// out instead of deadlocking the test.
	go func() {
		if h := r.hub(key); h != nil {
			h.broadcast(0, Frame{Data: []byte("ephemeral-during-commit")})
		}
	}()
	waitForFrameContaining(t, c, "ephemeral-during-commit", 2*time.Second,
		"ephemeral broadcast stalled behind an in-flight durable commit")

	// (b) The peer-receive path is the same broadcast, entered where the gRPC
	// receiver enters it.
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

	// Released: the mirror still fans out, after the write.
	close(releaseCommit)
	<-commitDone
	waitForFrameContaining(t, c, `"key":"k"`, 2*time.Second,
		"the released commit's mirror frame never fanned out")
}
