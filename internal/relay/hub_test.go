package relay

import (
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// fakeConn is a test Conn that records the frames it received and can be
// made to fill its buffer (block) so the backpressure path is exercised
// without a real socket. It implements the Conn interface.
type fakeConn struct {
	id uint64

	mu       sync.Mutex
	received []Frame
	full     bool // when true, Send returns false (buffer full / laggard)
	closed   bool
}

func newFakeConn(id uint64) *fakeConn { return &fakeConn{id: id} }

func (c *fakeConn) ID() uint64 { return c.id }

func (c *fakeConn) Send(f Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.full || c.closed {
		return false
	}
	c.received = append(c.received, f)
	return true
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *fakeConn) setFull(v bool) {
	c.mu.Lock()
	c.full = v
	c.mu.Unlock()
}

func (c *fakeConn) recv() []Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Frame, len(c.received))
	copy(out, c.received)
	return out
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func testKey() RoomKey {
	return RoomKey{App: domain.Slug("appz2345"), ID: domain.NewRoomID()}
}

func TestHub_BroadcastReachesOthersNotSender(t *testing.T) {
	h := newHub(testKey(), 0, nil, nil)
	a, b, c := newFakeConn(1), newFakeConn(2), newFakeConn(3)
	for _, conn := range []*fakeConn{a, b, c} {
		if !h.register(conn) {
			t.Fatalf("register %d failed", conn.id)
		}
	}
	f := Frame{Data: []byte("hello")}
	h.broadcast(a.id, f)

	if got := a.recv(); len(got) != 0 {
		t.Fatalf("sender a received its own message: %v", got)
	}
	for _, conn := range []*fakeConn{b, c} {
		got := conn.recv()
		if len(got) != 1 || string(got[0].Data) != "hello" {
			t.Fatalf("peer %d got %v, want one 'hello'", conn.id, got)
		}
	}
}

func TestHub_ServerOriginatedBroadcastReachesEveryone(t *testing.T) {
	h := newHub(testKey(), 0, nil, nil)
	a, b := newFakeConn(1), newFakeConn(2)
	h.register(a)
	h.register(b)
	// from == 0 is the server-originated mirror: no socket to exclude.
	h.broadcast(0, Frame{Data: []byte("put")})
	for _, conn := range []*fakeConn{a, b} {
		if got := conn.recv(); len(got) != 1 {
			t.Fatalf("conn %d got %d frames, want 1", conn.id, len(got))
		}
	}
}

func TestHub_RegisterRespectsPerRoomCap(t *testing.T) {
	h := newHub(testKey(), 2, nil, nil)
	if !h.register(newFakeConn(1)) {
		t.Fatal("first register should pass")
	}
	if !h.register(newFakeConn(2)) {
		t.Fatal("second register should pass")
	}
	if h.register(newFakeConn(3)) {
		t.Fatal("third register should be refused (cap 2)")
	}
	if h.len() != 2 {
		t.Fatalf("hub len = %d, want 2", h.len())
	}
}

func TestHub_SlowClientDroppedWithoutBlockingRoom(t *testing.T) {
	// onDrop must fire exactly once, for the dropped laggard's id, so the
	// registry can reclaim its per-app slot (the leak the multi-client churn
	// test surfaced: a laggard drop that never decremented the per-app
	// counter). A non-laggard recipient never triggers onDrop.
	var dropped []uint64
	h := newHub(testKey(), 0, nil, func(id uint64) { dropped = append(dropped, id) })
	slow, fast := newFakeConn(1), newFakeConn(2)
	h.register(slow)
	h.register(fast)
	slow.setFull(true) // slow's buffer is full: Send will return false.

	h.broadcast(0, Frame{Data: []byte("x")})

	// The fast client still got the frame (the room was not stalled on the
	// laggard).
	if got := fast.recv(); len(got) != 1 {
		t.Fatalf("fast client got %d frames, want 1 (room stalled on laggard?)", len(got))
	}
	// The slow client was dropped: closed and removed from the hub.
	if !slow.isClosed() {
		t.Fatal("slow client was not closed")
	}
	if h.len() != 1 {
		t.Fatalf("hub len = %d after dropping laggard, want 1", h.len())
	}
	// onDrop fired once, for the slow client's id only (so the registry decApps
	// the dropped slot - and exactly once).
	if len(dropped) != 1 || dropped[0] != slow.id {
		t.Fatalf("onDrop fired with %v, want exactly [%d] (the dropped laggard)", dropped, slow.id)
	}
}

func TestHub_UnregisterFiresOnEmpty(t *testing.T) {
	var emptied int
	var mu sync.Mutex
	h := newHub(testKey(), 0, func() {
		mu.Lock()
		emptied++
		mu.Unlock()
	}, nil)
	a, b := newFakeConn(1), newFakeConn(2)
	h.register(a)
	h.register(b)

	h.unregister(a.id)
	mu.Lock()
	if emptied != 0 {
		t.Fatalf("onEmpty fired with one conn still present (emptied=%d)", emptied)
	}
	mu.Unlock()

	h.unregister(b.id)
	mu.Lock()
	if emptied != 1 {
		t.Fatalf("onEmpty fired %d times, want 1", emptied)
	}
	mu.Unlock()

	// Idempotent: unregistering an absent id does not re-fire onEmpty.
	h.unregister(a.id)
	mu.Lock()
	if emptied != 1 {
		t.Fatalf("onEmpty re-fired on idempotent unregister (emptied=%d)", emptied)
	}
	mu.Unlock()
}

func TestHub_DroppingLastLaggardFiresOnEmptyAndOnDrop(t *testing.T) {
	var emptied int
	var dropped []uint64
	h := newHub(testKey(), 0, func() { emptied++ }, func(id uint64) { dropped = append(dropped, id) })
	only := newFakeConn(1)
	h.register(only)
	only.setFull(true)

	// A server-originated broadcast to the one full client drops it, which
	// empties the hub and must fire BOTH onDrop (reclaim its per-app slot) and
	// onEmpty (tear the now-empty hub down).
	h.broadcast(0, Frame{Data: []byte("x")})
	if !only.isClosed() {
		t.Fatal("the only (laggard) client was not closed")
	}
	if h.len() != 0 {
		t.Fatalf("hub len = %d, want 0", h.len())
	}
	if emptied != 1 {
		t.Fatalf("onEmpty fired %d times, want 1", emptied)
	}
	if len(dropped) != 1 || dropped[0] != only.id {
		t.Fatalf("onDrop fired with %v, want exactly [%d]", dropped, only.id)
	}
}

func TestHub_ConcurrentRegisterBroadcastUnregisterRace(t *testing.T) {
	// Run under -race: hammer register / broadcast / unregister from many
	// goroutines to catch a data race in the hub's concurrent paths.
	h := newHub(testKey(), 0, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			c := newFakeConn(id)
			h.register(c)
			h.broadcast(id, Frame{Data: []byte("x")})
			h.unregister(id)
		}(uint64(i + 1))
	}
	wg.Wait()
	if h.len() != 0 {
		t.Fatalf("hub len = %d after all unregister, want 0", h.len())
	}
}
