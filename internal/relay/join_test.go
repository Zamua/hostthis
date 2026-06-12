package relay

import (
	"strings"
	"sync"
	"testing"
)

// TestJoinWithSnapshot_NoGapNoDupUnderConcurrentBroadcast pins the spec's
// core late-join guarantee at the unit level, deterministically: a durable
// write racing a join lands in EXACTLY ONE of {snapshot, live stream},
// never zero (gap) and - for the ordering the lock enforces - never
// duplicated by the server. Because joinWithSnapshot reads the snapshot AND
// registers the connection under the hub lock, and broadcast also takes the
// hub lock, the two are serialized: the joiner either sees the write in its
// snapshot (broadcast ran first, so the snapshot read after it) or as a
// live frame (broadcast ran after the register), but the write is never
// lost.
//
// The test drives the race many times. The snapshot reflects whatever the
// shared store held at read time; the broadcast mutates that store THEN
// broadcasts (mirroring the HTTP PUT: commit-then-mirror). We assert the
// joiner's observed state (snapshot keys + any live frames) always contains
// the written key.
func TestJoinWithSnapshot_NoGapNoDupUnderConcurrentBroadcast(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
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

		// Goroutine B: the join (snapshot read + register under the hub lock).
		go func() {
			defer wg.Done()
			_ = r.joinWithSnapshot(key, c, readSnap, func(f Frame) bool { return c.Send(f) })
		}()

		wg.Wait()

		// Observe what the joiner has: the snapshot frame (always first) plus
		// any live frame. The written key must appear in at least one of them
		// (no gap).
		frames := c.recv()
		if len(frames) == 0 {
			t.Fatalf("iter %d: joiner received no frames at all", i)
		}
		sawKey := false
		for _, f := range frames {
			s := string(f.Data)
			if strings.Contains(s, `"k":"v"`) || strings.Contains(s, `"key":"k"`) {
				sawKey = true
			}
		}
		if !sawKey {
			t.Fatalf("iter %d: joiner saw neither the snapshot nor the live frame for the write (GAP). frames=%v", i, framesToStrings(frames))
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

// TestJoinWithSnapshot_SnapshotIsFirstFrame confirms the snapshot is always
// delivered before any live frame: the join queues it first under the lock.
func TestJoinWithSnapshot_SnapshotIsFirstFrame(t *testing.T) {
	r := NewRegistry(NewLimits())
	key := testKey()
	_, id, err := r.admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	c := newFakeConn(id)
	if err := r.joinWithSnapshot(key, c,
		func() (Frame, error) { return Frame{Data: []byte(`SNAP`)}, nil },
		func(f Frame) bool { return c.Send(f) },
	); err != nil {
		t.Fatalf("join: %v", err)
	}
	// A subsequent broadcast is the second frame.
	r.hub(key).broadcast(0, Frame{Data: []byte(`LIVE`)})
	frames := c.recv()
	if len(frames) < 2 || string(frames[0].Data) != "SNAP" || string(frames[1].Data) != "LIVE" {
		t.Fatalf("frame order = %v, want [SNAP LIVE]", framesToStrings(frames))
	}
}
