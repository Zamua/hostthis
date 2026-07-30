package relay

import (
	"strings"
	"sync"
	"testing"
)

// TestJoinWithSnapshot_NoGapUnderConcurrentBroadcast pins the late-join no-gap
// guarantee: a durable write racing a join is never lost. Register-then-read
// (SPEC "Persistence and late-join") is what buys it - a broadcast before the
// register is missed live but its commit precedes the snapshot read, and a
// broadcast after the register is delivered live.
//
// A write may legally land in BOTH; the client's seq-discard rule handles that
// duplicate, so only the no-gap half is asserted here: never neither.
func TestJoinWithSnapshot_NoGapUnderConcurrentBroadcast(t *testing.T) {
	const iterations = 200
	for i := range iterations {
		r := NewRegistry(NewLimits())
		key := testKey()

		// A shared "durable store": the snapshot reads it; the writer mutates
		// it before broadcasting, like commit-then-mirror.
		var storeMu sync.Mutex
		written := false
		readSnap := func() (Frame, error) {
			storeMu.Lock()
			has := written
			storeMu.Unlock()
			if has {
				return Frame{Data: []byte(`{"type":"snapshot","state":{"k":"v"}}`)}, nil
			}
			return Frame{Data: []byte(`{"type":"snapshot","state":{}}`)}, nil
		}

		_, id, err := r.admit(key)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		c := newFakeConn(id)

		var wg sync.WaitGroup
		wg.Add(2)

		// The durable write: commit to the store, THEN broadcast the live
		// mirror (the order commit-and-mirror uses).
		go func() {
			defer wg.Done()
			storeMu.Lock()
			written = true
			storeMu.Unlock()
			if h := r.hub(key); h != nil {
				h.broadcast(0, Frame{Data: []byte(`{"type":"put","key":"k","value":"v"}`)})
			}
		}()

		// The join: register first, snapshot read off the lock.
		snapCh := make(chan Frame, 1)
		go func() {
			defer wg.Done()
			snap, jerr := r.joinWithSnapshot(key, c, readSnap)
			if jerr == nil {
				snapCh <- snap
			} else {
				close(snapCh)
			}
		}()

		wg.Wait()

		// What the joiner has: the returned snapshot (its first wire frame)
		// plus any live frames delivered to the connection.
		snap, ok := <-snapCh
		if !ok {
			t.Fatalf("iter %d: join failed", i)
		}
		observed := append([]Frame{snap}, c.recv()...)
		sawKey := false
		for _, f := range observed {
			s := string(f.Data)
			if strings.Contains(s, `"k":"v"`) || strings.Contains(s, `"key":"k"`) {
				sawKey = true
			}
		}
		if !sawKey {
			t.Fatalf("iter %d: joiner saw neither the snapshot nor the live frame for the write (GAP). frames=%v", i, framesToStrings(observed))
		}
	}
}

func framesToStrings(fs []Frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f.Data)
	}
	return out
}

// TestJoinWithSnapshot_RegistersThenReturnsSnapshot pins the join's two halves:
// the connection is in the broadcast set when joinWithSnapshot returns, and the
// snapshot frame is returned to the caller (who writes it as the first wire
// frame; the wire ordering itself is pinned by the ws-level harness).
func TestJoinWithSnapshot_RegistersThenReturnsSnapshot(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	_, id, err := r.admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	c := newFakeConn(id)
	snap, err := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte(`SNAP`)}, nil },
	)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if string(snap.Data) != "SNAP" {
		t.Fatalf("returned snapshot = %q, want SNAP", snap.Data)
	}
	// The connection is registered: a subsequent broadcast reaches it.
	r.hub(key).broadcast(0, Frame{Data: []byte(`LIVE`)})
	frames := c.recv()
	if len(frames) != 1 || string(frames[0].Data) != "LIVE" {
		t.Fatalf("frames after broadcast = %v, want [LIVE]", framesToStrings(frames))
	}
}
