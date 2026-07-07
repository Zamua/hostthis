package relay

import (
	"testing"
)

// TestAnnounceDrain_HintReachesEveryConnectionAndKeepsServing pins the
// drain hint (SPEC "Drain hint: reconnect-before-shutdown"): AnnounceDrain
// broadcasts the {"type":"reconnect"} control envelope once to EVERY
// connection of EVERY hub, and the relay keeps serving afterwards - the
// hint is fired at drain start, before any close, so a client acting on it
// re-homes while its old socket still works and new joins still land.
func TestAnnounceDrain_HintReachesEveryConnectionAndKeepsServing(t *testing.T) {
	r := NewRegistry(NewLimits())
	k1, k2 := testKey(), testKey()
	c1a, _ := bindFake(t, r, k1)
	c1b, _ := bindFake(t, r, k1)
	c2, _ := bindFake(t, r, k2)

	rl := &Relay{reg: r}
	rl.AnnounceDrain()

	for name, c := range map[string]*fakeConn{"c1a": c1a, "c1b": c1b, "c2": c2} {
		got := c.recv()
		hints := 0
		for _, f := range got {
			if string(f.Data) == `{"type":"reconnect"}` {
				hints++
			}
		}
		if hints != 1 {
			t.Fatalf("%s received %d reconnect hints (frames=%v), want exactly 1", name, hints, framesToStrings(got))
		}
		if c.isClosed() {
			t.Fatalf("%s was closed by AnnounceDrain - the hint must precede the close, not perform it", name)
		}
	}

	// The relay keeps serving through the grace window: a new admit + join
	// still succeeds after the hint fired.
	_, id, err := r.admit(k1)
	if err != nil {
		t.Fatalf("admit after AnnounceDrain: %v (the relay must keep serving through the drain grace)", err)
	}
	c := newFakeConn(id)
	if _, err := r.joinWithSnapshot(k1, c,
		func() (Frame, error) { return Frame{Data: []byte(`{"type":"snapshot","seq":0,"state":{}}`)}, nil },
	); err != nil {
		t.Fatalf("join after AnnounceDrain: %v", err)
	}
}
