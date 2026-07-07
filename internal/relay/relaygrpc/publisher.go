package relaygrpc

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/Zamua/hostthis/internal/relay"
)

// Publisher defaults. QueueDepth bounds each peer's outbound queue in
// frames (a frame is at most the relay's 32 KiB cap, so the worst-case
// buffered memory per peer is small); SendTimeout bounds one Publish RPC
// so a black-holing peer cannot wedge its sender.
const (
	DefaultQueueDepth  = 256
	DefaultSendTimeout = 5 * time.Second
)

// PublisherConfig tunes the dialing publisher. Zero values take the
// defaults above; Logf (optional) receives sparse peer up/down lines.
type PublisherConfig struct {
	QueueDepth  int
	SendTimeout time.Duration
	Logf        func(format string, args ...any)
}

// Publisher is the production relay.PeerPublisher: it fans every frame
// out to the CURRENT peer set (read from the relay.Peers port per
// publish, so membership churn is followed with no subscription
// machinery) over one bounded queue + one sender goroutine + one
// long-lived client connection per peer.
//
// The delivery contract (SPEC "Delivery semantics: best-effort per peer,
// isolated per peer, and never on the commit path"):
//
//   - Publish NEVER blocks and NEVER fails the caller: it enqueues on
//     each peer's bounded queue and returns. A FULL queue drops the frame
//     being published (drop-newest: O(1), allocation-free; drop-oldest
//     would be equally contract-legal since either way the gap is
//     detectable). A dropped durable frame is healed by the affected
//     subscribers' splice re-snapshot; a dropped ephemeral frame is
//     harmless by definition.
//   - Peers are isolated: a slow, full, or unreachable peer costs the
//     writer nothing and other peers' queues nothing.
//   - Senders follow membership: a new peer address gets a sender on the
//     next publish; a departed peer's sender is stopped and its
//     connection closed.
type Publisher struct {
	peers       relay.Peers
	queueDepth  int
	sendTimeout time.Duration
	logf        func(format string, args ...any)

	// baseCtx parents every RPC so Close aborts in-flight sends promptly.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu        sync.Mutex
	senders   map[string]*peerSender
	lastAddrs []string // sorted snapshot for the no-change fast path
	closed    bool
	wg        sync.WaitGroup

	// drops counts frames dropped on a full queue (all peers combined).
	// Observability + tests; never consulted for control flow.
	drops atomic.Uint64
}

// outbound is one queued frame for one peer.
type outbound struct {
	key relay.RoomKey
	f   relay.Frame
}

// peerSender is one peer's bounded queue + stop signal; its goroutine owns
// the client connection.
type peerSender struct {
	addr string
	ch   chan outbound
	stop chan struct{}
}

// NewPublisher builds a publisher over the peer-discovery port. Senders
// are created lazily on the first publish that sees each peer address.
func NewPublisher(peers relay.Peers, cfg PublisherConfig) *Publisher {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Publisher{
		peers:       peers,
		queueDepth:  cfg.QueueDepth,
		sendTimeout: cfg.SendTimeout,
		logf:        cfg.Logf,
		baseCtx:     ctx,
		baseCancel:  cancel,
		senders:     make(map[string]*peerSender),
	}
}

// Publish implements relay.PeerPublisher: reconcile the sender set with
// the current peer addresses, then enqueue the frame on every peer's
// queue without blocking. See the Publisher doc for the drop policy.
func (p *Publisher) Publish(key relay.RoomKey, f relay.Frame) {
	addrs := p.peers.Addresses()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.reconcileLocked(addrs)
	ob := outbound{key: key, f: f}
	for _, s := range p.senders {
		select {
		case s.ch <- ob:
		default:
			p.drops.Add(1) // full queue: drop THIS frame for THIS peer
		}
	}
	p.mu.Unlock()
}

// reconcileLocked aligns p.senders with addrs: start senders for new
// peers, stop senders for departed ones. Caller holds p.mu. The sorted
// snapshot makes the steady state (unchanged membership) a cheap
// slice-compare.
func (p *Publisher) reconcileLocked(addrs []string) {
	sorted := slices.Clone(addrs)
	slices.Sort(sorted)
	if slices.Equal(sorted, p.lastAddrs) {
		return
	}
	p.lastAddrs = sorted

	want := make(map[string]bool, len(sorted))
	for _, a := range sorted {
		if a == "" {
			continue
		}
		want[a] = true
		if _, ok := p.senders[a]; !ok {
			s := &peerSender{
				addr: a,
				ch:   make(chan outbound, p.queueDepth),
				stop: make(chan struct{}),
			}
			p.senders[a] = s
			p.wg.Add(1)
			go p.runSender(s)
		}
	}
	for a, s := range p.senders {
		if !want[a] {
			close(s.stop)
			delete(p.senders, a)
		}
	}
}

// runSender drains one peer's queue over a long-lived client connection.
// Errors are best-effort drops; up/down transitions are logged once each
// so a flapping peer does not spam the log.
func (p *Publisher) runSender(s *peerSender) {
	defer p.wg.Done()
	// grpc.NewClient constructs the connection lazily; the first RPC dials.
	// The peer service rides the cluster-internal listener shale forwarding
	// uses (pod-to-pod inside the deployment's network boundary, never the
	// public ingress), so plaintext credentials mirror shale's own
	// forwarding client.
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		// Malformed target (not a transient dial failure - those surface per
		// RPC). Drain and drop until the peer leaves the membership.
		p.logfSafe("relay peers: client for %s failed to construct: %v (frames to this peer drop)", s.addr, err)
		for {
			select {
			case <-s.stop:
				return
			case <-s.ch:
				p.drops.Add(1)
			}
		}
	}
	defer func() { _ = conn.Close() }()
	client := NewRoomRelayClient(conn)

	healthy := true
	for {
		select {
		case <-s.stop:
			return
		case ob := <-s.ch:
			ctx, cancel := context.WithTimeout(p.baseCtx, p.sendTimeout)
			_, err := client.Publish(ctx, &PublishRequest{
				AppSlug: ob.key.App.String(),
				RoomId:  ob.key.ID.String(),
				Binary:  ob.f.Binary,
				Data:    ob.f.Data,
			})
			cancel()
			if err != nil {
				p.drops.Add(1)
				if healthy {
					healthy = false
					p.logfSafe("relay peers: publish to %s failed: %v (best-effort; suppressing until it recovers)", s.addr, err)
				}
			} else if !healthy {
				healthy = true
				p.logfSafe("relay peers: peer %s recovered", s.addr)
			}
		}
	}
}

// Drops reports the total frames dropped so far (full queues + failed
// sends, all peers combined). Observability + tests only.
func (p *Publisher) Drops() uint64 { return p.drops.Load() }

// Close stops every sender goroutine, aborts in-flight RPCs, and closes
// the client connections. Safe to call once at shutdown; Publish after
// Close is a no-op.
func (p *Publisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for a, s := range p.senders {
		close(s.stop)
		delete(p.senders, a)
	}
	p.mu.Unlock()
	p.baseCancel() // abort any in-flight Publish RPC
	p.wg.Wait()
}

func (p *Publisher) logfSafe(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}
