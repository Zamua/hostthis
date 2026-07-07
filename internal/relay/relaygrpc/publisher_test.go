package relaygrpc

import (
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
)

// staticPeers is a fixed relay.Peers for tests; Set swaps the list to
// drive membership churn.
type staticPeers struct {
	mu    sync.Mutex
	addrs []string
}

func (s *staticPeers) Addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.addrs...)
}

func (s *staticPeers) Set(addrs ...string) {
	s.mu.Lock()
	s.addrs = addrs
	s.mu.Unlock()
}

func testRoomKey() relay.RoomKey {
	return relay.RoomKey{App: domain.Slug("appz2345"), ID: domain.NewRoomID()}
}

// startReceiver serves a Receiver on a loopback listener and returns its
// address plus the receiver. Cleanup stops the server.
func startReceiver(t *testing.T, maxFrameBytes int64) (string, *Receiver) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer()
	recv := NewReceiver(maxFrameBytes)
	recv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	return lis.Addr().String(), recv
}

// TestPublisher_NeverBlocksOnUnreachablePeer pins the delivery contract's
// core clause: Publish enqueues and returns - it never blocks and never
// fails the caller, even when the peer is unreachable and the queue
// overflows. Frames past the bounded queue are DROPPED (drop-newest),
// counted, and the caller is never delayed: the whole burst must complete
// in a bound far below a single dial timeout.
func TestPublisher_NeverBlocksOnUnreachablePeer(t *testing.T) {
	peers := &staticPeers{}
	// A TEST-NET address nothing answers on: every RPC hangs until its
	// deadline, so the sender goroutine drains at most one frame per
	// timeout while the caller keeps publishing.
	peers.Set("192.0.2.1:9")
	p := NewPublisher(peers, PublisherConfig{QueueDepth: 8, SendTimeout: 200 * time.Millisecond})
	defer p.Close()

	key := testRoomKey()
	const bursts = 5000
	start := time.Now()
	for range bursts {
		p.Publish(key, relay.Frame{Data: []byte("x")})
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("publishing %d frames to an unreachable peer took %s - Publish blocked the caller", bursts, elapsed)
	}
	if p.Drops() == 0 {
		t.Fatal("no drops recorded despite overflowing a depth-8 queue with an unreachable peer")
	}
}

// TestPublisher_DeliversOverRealGRPCAndFollowsMembership drives the full
// adapter loop over the REAL client/server transport: frames published
// reach the peer's Receiver (after Bind), a peer removed from the
// membership stops receiving (its sender is pruned), and Close shuts down
// cleanly.
func TestPublisher_DeliversOverRealGRPCAndFollowsMembership(t *testing.T) {
	addr, recv := startReceiver(t, relay.DefaultMaxMessageBytes)

	var mu sync.Mutex
	var got []string
	recv.Bind(func(key relay.RoomKey, f relay.Frame) {
		mu.Lock()
		got = append(got, string(f.Data))
		mu.Unlock()
	})

	peers := &staticPeers{}
	peers.Set(addr)
	p := NewPublisher(peers, PublisherConfig{})
	defer p.Close()

	key := testRoomKey()
	p.Publish(key, relay.Frame{Data: []byte("first")})
	waitFor(t, 5*time.Second, "the frame to arrive at the peer over real gRPC", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1 && got[0] == "first"
	})

	// Membership churn: the peer leaves; its sender is pruned and later
	// frames do not arrive.
	peers.Set()
	p.Publish(key, relay.Frame{Data: []byte("after-leave")}) // triggers the reconcile
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("peer received %d frames after leaving the membership, want 1 (the pre-leave frame only)", n)
	}
}

// TestReceiver_DropsBeforeBindAndEnforcesSizeCap pins the receive path's
// two guards: a frame arriving before the delivery hook is bound (the
// boot race) is dropped without error, and an over-cap frame is refused
// (the defense-in-depth size re-check).
func TestReceiver_DropsBeforeBindAndEnforcesSizeCap(t *testing.T) {
	addr, recv := startReceiver(t, 16)

	peers := &staticPeers{}
	peers.Set(addr)
	p := NewPublisher(peers, PublisherConfig{})
	defer p.Close()

	key := testRoomKey()

	// Before Bind: delivery must be a silent drop (no panic, no error
	// surfaced anywhere that could fail the origin).
	p.Publish(key, relay.Frame{Data: []byte("early")})
	time.Sleep(100 * time.Millisecond)

	var mu sync.Mutex
	var got []string
	recv.Bind(func(_ relay.RoomKey, f relay.Frame) {
		mu.Lock()
		got = append(got, string(f.Data))
		mu.Unlock()
	})

	// Over the 16-byte cap: refused at the receiver, never delivered.
	p.Publish(key, relay.Frame{Data: []byte("this frame is far past the sixteen byte cap")})
	// Under the cap: delivered.
	p.Publish(key, relay.Frame{Data: []byte("small")})

	waitFor(t, 5*time.Second, "the under-cap frame to arrive", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1 && got[0] == "small"
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("received %v: the pre-Bind and over-cap frames must both be dropped", got)
	}
}

// waitFor polls cond until true or fails after d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}
