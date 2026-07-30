package relay

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
)

// Snapshotter is the relay's ONLY dependency on the durable tier: it reads the
// current room KV so a joining client is caught up before it joins the live
// stream. internal/service.Rooms satisfies it via Scan.
//
// The relay never writes durable state. Every durable mutation is the app's
// HTTP KV PUT/DELETE, which the server mirrors to the room's hub, so the relay
// inherits the durable tier's caps and retention and never opens a second
// persistence path.
type Snapshotter interface {
	Scan(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error)
}

// Heartbeat timing: the server pings each connection on PingInterval and
// reaps it if the pong misses PingTimeout. PingInterval must stay UNDER the
// proxy idle defaults (traefik / nginx 60-120 s) so the heartbeat also keeps a
// legitimately-quiet connection alive through the proxy.
const (
	PingInterval = 20 * time.Second
	PingTimeout  = 10 * time.Second
)

// Relay is the real-time room relay service: it owns the Registry, reads the
// late-join snapshot via the Snapshotter, and runs each connection's reader /
// writer / heartbeat lifecycle. The HTTP upgrade handler authenticates the
// room and hands the accepted websocket to Serve.
type Relay struct {
	reg  *Registry
	snap Snapshotter

	// peers is the multi-pod outbound port, nil on a single-pod deploy (the
	// degenerate zero-peer case). Wired by SetPeerPublisher at the
	// composition root.
	peers PeerPublisher

	// pingInterval / pingTimeout are fields rather than the consts directly
	// so tests can shorten them; NewRelay sets the defaults.
	pingInterval time.Duration
	pingTimeout  time.Duration
}

// NewRelay builds a relay over snap with the given limits.
func NewRelay(snap Snapshotter, limits Limits) *Relay {
	return &Relay{
		reg:          NewRegistry(limits),
		snap:         snap,
		pingInterval: PingInterval,
		pingTimeout:  PingTimeout,
	}
}

// Registry exposes the registry so the HTTP layer can mirror a durable PUT to
// a live hub and shutdown can close every connection.
func (rl *Relay) Registry() *Registry { return rl.reg }

// AnnounceDrain broadcasts the {"type":"reconnect"} drain hint once to every
// live connection (SPEC "Drain hint: reconnect-before-shutdown"). Must be
// called on the termination signal BEFORE the HTTP server stops accepting and
// before CloseAll, with serving continuing through the drain grace window, so
// a client acting on the hint can reconnect onto a surviving pod while its old
// socket still works. The hint is an optimization, never load-bearing: clients
// that ignore it heal through the normal reconnect + snapshot + splice path.
func (rl *Relay) AnnounceDrain() { rl.reg.announceDrain() }

// SetHeartbeat overrides the ping interval + timeout so tests can drive the
// reap path quickly.
func (rl *Relay) SetHeartbeat(interval, timeout time.Duration) {
	rl.pingInterval = interval
	rl.pingTimeout = timeout
}

// Admit reserves a connection slot for the room under the per-room / per-app /
// total-rooms caps and returns the hub + a fresh connection id, or an
// admission error (ErrRoomFull / ErrAppFull / ErrTooManyRooms) the HTTP layer
// maps to a status. Must be called BEFORE completing the websocket handshake,
// so an over-limit upgrade is refused with a normal HTTP status and no socket
// is ever accepted for it.
func (rl *Relay) Admit(key RoomKey) (*Hub, uint64, error) {
	return rl.reg.admit(key)
}

// Release frees a slot reserved by Admit that was never handed to Serve: the
// websocket Accept failed AFTER admission, so Serve's deferred teardown never
// ran. Without it a failed Accept leaks a connection count and an empty hub.
func (rl *Relay) Release(key RoomKey, id uint64) {
	rl.reg.release(key, id)
}

// CommitAndMirror runs a durable write's KV commit, broadcasts its LOCAL live
// mirror, then publishes the mirror to the peer pods (best-effort, never on
// the commit path). A failed commit mirrors nothing, publishes nothing, and
// returns the error.
//
// commit returns the mirror frame to fan out, built AFTER the write because it
// carries the per-room sequence the commit assigned: the seq is durable room
// state only the commit knows. NOTHING here runs under the room's hub lock -
// the commit is a storage round trip, and holding the hub lock across storage
// I/O would let a slow commit stall the room's fan-out and joins. The mirror
// broadcasts server-originated (from == 0), so every local connection receives
// it.
//
// Correctness, locally AND across pods, rides the sequence: subscribers order
// by seq, de-duplicate by the discard rule (seq <= their snapshot's S), and
// detect loss by the hole a dense seq leaves (SPEC "Multi-pod relay"). The
// local broadcast is the zero-hop case of the same contract.
func (rl *Relay) CommitAndMirror(key RoomKey, commit func() (Frame, error)) error {
	mirror, err := rl.reg.commitAndMirror(key, commit)
	if err != nil {
		return err
	}
	rl.publishToPeers(key, mirror)
	return nil
}

// wsConn wraps a coder/websocket connection in the Conn interface the hub
// broadcasts to. Each connection owns a bounded send buffer and a single
// writer goroutine draining it to the socket, which is what makes the
// broadcast wait-free per client.
type wsConn struct {
	id   uint64
	ws   *websocket.Conn
	send chan Frame

	// writeTimeout bounds each socket write. Set from the relay's CONFIGURED
	// ping timeout, not the package constant, so SetHeartbeat moves the write
	// reap window in lockstep with the ping reap window: a write that does
	// not complete in time is a dead socket, reaped on the same clock as a
	// missed pong.
	writeTimeout time.Duration

	closeOnce sync.Once
	// closed is closed exactly once to stop the writer goroutine and signal
	// the lifecycle to tear down. The reader/heartbeat select on it.
	closed chan struct{}
}

func newWSConn(id uint64, ws *websocket.Conn, buffer int, writeTimeout time.Duration) *wsConn {
	if buffer < 1 {
		buffer = 1
	}
	if writeTimeout <= 0 {
		writeTimeout = PingTimeout
	}
	return &wsConn{
		id:           id,
		ws:           ws,
		send:         make(chan Frame, buffer),
		writeTimeout: writeTimeout,
		closed:       make(chan struct{}),
	}
}

func (c *wsConn) ID() uint64 { return c.id }

// Send enqueues f on the bounded buffer without blocking. A full buffer (the
// laggard case) or an already-closed connection returns false, the
// backpressure signal the hub acts on by dropping this connection.
func (c *wsConn) Send(f Frame) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.send <- f:
		return true
	default:
		return false
	}
}

// Close stops the writer, signals the lifecycle, and aborts the socket.
// Idempotent.
func (c *wsConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		// CloseNow aborts without the closing handshake: a graceful teardown
		// has already sent a clean close before reaching here, and a laggard
		// or reap just needs the socket gone.
		_ = c.ws.CloseNow()
	})
}

// Serve runs a single accepted connection's full lifecycle: the late-join
// snapshot, the live stream, and the heartbeat, tearing everything down on any
// exit. The caller has already Admitted the connection and accepted the
// websocket. Serve blocks until the connection ends, so the handler's request
// goroutine owns the connection for its lifetime - correct, since the
// websocket was hijacked out from under the http.Server's timeouts.
//
// Late-join is gap-free and dup-free only under the register-FIRST sequence
// (SPEC "Persistence and late-join"):
//
//  1. Register the connection into the hub FIRST, so every mirror frame
//     broadcast from that instant on is queued for it (the frames wait in
//     the send buffer behind the reserved first-frame slot).
//  2. Read the room KV snapshot OUTSIDE the hub lock and write it as the
//     FIRST frame on the wire, stamped with the exact seq S it reflects.
//  3. Start the writer (draining any buffered live frames), reader, and
//     heartbeat; live frames flow.
//
// A frame broadcast BEFORE the register came from a commit that completed
// before the snapshot read started, so its effect is in the snapshot
// (seq <= S): no gap. A frame delivered after the register whose effect the
// snapshot also caught is discarded by the client's seq <= S rule: no dup.
// Correctness rides the sequence, so it holds identically for frames arriving
// from peer pods.
func (rl *Relay) Serve(ctx context.Context, key RoomKey, id uint64, ws *websocket.Conn) {
	limits := rl.reg.limits
	ws.SetReadLimit(limits.MaxMessageBytes)

	c := newWSConn(id, ws, limits.SendBuffer, rl.pingTimeout)

	// Teardown on every exit path (clean close, reap, laggard drop, snapshot
	// failure, shutdown): stop the writer, unregister from the hub, release
	// the per-app slot, exactly once.
	defer func() {
		c.Close()
		rl.reg.release(key, id)
	}()

	// 1+2. joinWithSnapshot swaps the admit's reservation for c under the hub
	// lock (c is the broadcast target from that instant), then reads the
	// durable KV snapshot with no lock held. A read error closes the
	// connection: a client that cannot be caught up is better off
	// reconnecting, which re-syncs from the KV.
	snap, err := rl.reg.joinWithSnapshot(key, c, func() (Frame, error) { return rl.buildSnapshot(key) })
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "join failed")
		return
	}

	// The reserved first-frame slot: write the snapshot DIRECTLY to the socket
	// before the writer goroutine starts draining the send buffer, so the
	// snapshot is first ON THE WIRE even though live frames may already be
	// buffered behind it. A room bursting faster than the send buffer during
	// the read drops this joiner as a laggard; it reconnects, correctness is
	// unaffected.
	if !c.writeFrame(ctx, snap) {
		return
	}

	// 3. writeLoop drains the send buffer (including live frames queued during
	// the join), readLoop pumps inbound frames into the hub broadcast under
	// the size + rate limits, heartbeat pings and reaps.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop(ctx) }()
	go func() { defer wg.Done(); rl.readLoop(ctx, key, c) }()
	rl.heartbeat(ctx, c)
	c.Close()
	wg.Wait()
}

func (rl *Relay) buildSnapshot(key RoomKey) (Frame, error) {
	kv, err := rl.snap.Scan(key.App, key.ID)
	if err != nil {
		return Frame{}, err
	}
	return encodeSnapshot(kv), nil
}

// writeFrame writes one frame under the bounded write timeout and reports
// whether it succeeded, closing the connection on error: a write that does not
// complete in time is a dead socket, reaped the same as a missed pong.
func (c *wsConn) writeFrame(ctx context.Context, f Frame) bool {
	typ := websocket.MessageText
	if f.Binary {
		typ = websocket.MessageBinary
	}
	wctx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	err := c.ws.Write(wctx, typ, f.Data)
	cancel()
	if err != nil {
		c.Close()
		return false
	}
	return true
}

// writeLoop drains the send buffer to the socket until the connection closes
// or ctx is done.
func (c *wsConn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case f := <-c.send:
			if !c.writeFrame(ctx, f) {
				return
			}
		}
	}
}

// readLoop pumps inbound frames into the room's broadcast under the per-frame
// size limit (SetReadLimit closes the socket on an over-cap frame) and the
// per-connection inbound rate limit. A read error (clean close, size
// violation, dead socket) ends the loop and closes the connection.
//
// The reader running is also what lets coder/websocket process incoming pongs
// (so the heartbeat's Ping resolves) and answer client-initiated pings; both
// happen inside Read.
func (rl *Relay) readLoop(ctx context.Context, key RoomKey, c *wsConn) {
	hub := rl.reg.hub(key)
	limit := newRateLimiter(rl.reg.limits.MaxMsgsPerSec)
	for {
		select {
		case <-c.closed:
			return
		case <-ctx.Done():
			return
		default:
		}
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			c.Close()
			return
		}
		// A client over its ceiling is dropped so it cannot saturate the
		// room's fan-out: one inbound frame is multiplied by the room's
		// connection count outbound.
		if !limit.allow() {
			_ = c.ws.Close(websocket.StatusPolicyViolation, "rate limit exceeded")
			c.Close()
			return
		}
		if hub == nil {
			hub = rl.reg.hub(key)
			if hub == nil {
				// Room emptied and torn down: nothing to fan out to, and this
				// connection is on its way down too.
				c.Close()
				return
			}
		}
		f := Frame{Binary: typ == websocket.MessageBinary, Data: data}
		hub.broadcast(c.id, f)
		// Cross-pod fan-out of the ephemeral frame: no seq, no contract, a
		// lost or reordered ephemeral frame needs no machinery. The
		// per-connection rate + size limits above ran BEFORE this point, so
		// peer input is bounded at the origin. No-op with zero peers.
		rl.publishToPeers(key, f)
	}
}

// heartbeat pings every pingInterval and reaps the connection if the pong does
// not return within pingTimeout. ws.Ping blocks until readLoop reads the pong
// or the deadline fires. Returning is what ends Serve's blocking call.
func (rl *Relay) heartbeat(ctx context.Context, c *wsConn) {
	t := time.NewTicker(rl.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, rl.pingTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				// Missed pong (or ctx done): the peer is dead.
				c.Close()
				return
			}
		}
	}
}
