package relay

import (
	"strings"
	"sync"
	"testing"
)

// TestJoinWithSnapshot_NoGapUnderConcurrentBroadcast pins the spec's core
// late-join guarantee at the unit level, deterministically: a durable write
// racing a join is NEVER lost (no gap). The join registers the connection
// FIRST, then reads the snapshot (SPEC "Persistence and late-join"), so:
//
//   - a broadcast that ran BEFORE the register was missed by the
//     connection, but its commit completed before the snapshot read
//     started, so the write is IN the snapshot;
//   - a broadcast that ran AFTER the register is delivered live.
//
// The write CAN legally appear in both (a broadcast inside the join
// window whose commit the snapshot read also caught) - that wire-level
// duplicate is handled by the client's seq-discard rule, not by a server
// lock, so this test asserts only the no-gap half: the written key appears
// in the snapshot, the live stream, or both - never neither.
//
// The test drives the race many times. The snapshot reflects whatever the
// shared store held at read time; the writer mutates that store THEN
// broadcasts (mirroring the HTTP PUT: commit-then-mirror).
func TestJoinWithSnapshot_NoGapUnderConcurrentBroadcast(t *testing.T) {
	const iterations = 200
	for i := range iterations {
		r := NewRegistry(NewLimits())
		key := testKey()

		// A shared "durable store": the snapshot reads it; the writer mutates
		// it before broadcasting, exactly like commit-then-mirror.
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

		// Admit + the connection we will join.
		_, id, err := r.admit(key)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		c := newFakeConn(id)

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine A: the durable write. Commit to the store, THEN broadcast
		// the live mirror (the order commit-and-mirror uses).
		go func() {
			defer wg.Done()
			storeMu.Lock()
			written = true
			storeMu.Unlock()
			if h := r.hub(key); h != nil {
				h.broadcast(0, Frame{Data: []byte(`{"type":"put","key":"k","value":"v"}`)})
			}
		}()

		// Goroutine B: the join (register first, snapshot read off the lock).
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

		// Observe what the joiner has: the returned snapshot (its first wire
		// frame) plus any live frames delivered to the connection. The written
		// key must appear in at least one of them (no gap).
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

// TestJoinWithSnapshot_RegistersThenReturnsSnapshot confirms the join's two
// halves: the connection is IN the broadcast set when joinWithSnapshot
// returns (a subsequent broadcast reaches it), and the snapshot frame is
// RETURNED to the caller - who writes it as the first frame on the wire
// (the reserved first-frame slot; the wire ordering itself is pinned by
// the ws-level harness, where every join's first frame is asserted to be
// the snapshot).
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
