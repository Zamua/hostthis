package relay

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
)

// Snapshotter is the relay's ONLY dependency on the durable tier: it reads
// the current room KV so a joining client (including a reloaded page) is
// caught up before it joins the live stream. It is the read half of the
// existing service-layer room surface, narrowed to the one verb the relay
// needs. internal/service.Rooms satisfies it via its Scan method.
//
// The relay does NOT write durable state itself. A durable mutation is the
// app's PUT/DELETE through the existing HTTP KV verb, which the server
// mirrors to the room's hub (the live fan-out of a committed change) - so
// the relay reuses the durable tier's caps and retention without
// re-implementing them, and never opens a second persistence path.
type Snapshotter interface {
	Scan(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error)
}

// Heartbeat timing. The server pings each connection on PingInterval and
// expects the pong within PingTimeout; a connection that misses it is
// reaped. PingInterval is chosen UNDER the proxy idle defaults (traefik /
// nginx 60-120 s) so the heartbeat also keeps a legitimately-quiet
// connection alive through the proxy.
const (
	PingInterval = 20 * time.Second
	PingTimeout  = 10 * time.Second
)

// Relay is the real-time room relay service: it owns the Registry, reads
// the late-join snapshot via the Snapshotter, and runs each connection's
// reader / writer / heartbeat lifecycle. The HTTP upgrade handler, after
// authenticating the room, hands a freshly-accepted websocket connection
// to Serve and the relay does the rest.
type Relay struct {
	reg  *Registry
	snap Snapshotter

	// peers is the multi-pod outbound port (nil on a single-pod deploy -
	// the zero-peer degenerate case, where the relay is exactly the
	// single-pod relay). Wired by SetPeerPublisher at the composition root.
	peers PeerPublisher

	// pingInterval / pingTimeout are fields (not the consts directly) so
	// tests can shorten them; NewRelay sets the defaults.
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

// Registry exposes the registry so the HTTP layer can mirror a durable PUT
// to a live hub and so server shutdown can close all connections.
func (rl *Relay) Registry() *Registry { return rl.reg }

// SetHeartbeat overrides the ping interval + timeout. Used by tests to
// drive the reap path quickly; production uses the NewRelay defaults.
func (rl *Relay) SetHeartbeat(interval, timeout time.Duration) {
	rl.pingInterval = interval
	rl.pingTimeout = timeout
}

// Admit reserves a connection slot for the room under the per-room /
// per-app / total-rooms caps and returns the hub + a fresh connection id,
// or an admission error (ErrRoomFull / ErrAppFull / ErrTooManyRooms) the
// HTTP layer maps to a status. The handler calls Admit BEFORE completing
// the websocket handshake, so an over-limit upgrade is refused with a
// normal HTTP status and no socket is ever accepted for it.
func (rl *Relay) Admit(key RoomKey) (*Hub, uint64, error) {
	return rl.reg.admit(key)
}

// Release frees a slot reserved by Admit that was never handed to Serve -
// the case where the websocket Accept failed AFTER admission, so the
// connection's deferred teardown in Serve never ran. It unregisters the
// reserved id from the hub and decrements the per-app counter, the same
// teardown Serve does, so a failed Accept does not leak a connection
// count or an empty hub.
func (rl *Relay) Release(key RoomKey, id uint64) {
	rl.reg.release(key, id)
}

// CommitAndMirror runs a durable write's KV commit, then broadcasts its
// LOCAL live mirror, then publishes the mirror to the peer pods
// (best-effort, never on the commit path). The HTTP KV PUT/DELETE handlers
// call it instead of doing the commit and the mirror as two separate steps.
//
// commit performs the durable KV write (the service-layer Put / Delete)
// and returns the mirror frame to fan out - built AFTER the write so it
// carries the per-room sequence the commit assigned (the frame cannot be
// built up front: the seq is durable room state only the commit knows).
// NOTHING runs under the room's hub lock here - the commit is a storage
// round trip and the spec forbids holding the hub lock across storage I/O
// (a slow commit must never stall the room's fan-out or joins; see
// registry.commitAndMirror). The mirror is broadcast server-originated
// (from == 0, so EVERY local connection receives it) after a successful
// commit. A failed commit mirrors nothing, publishes nothing, and returns
// the error.
//
// Correctness - locally AND across pods - rides the sequence: subscribers
// order by seq, de-duplicate by the discard rule (seq <= their snapshot's
// S), and detect loss by the hole a dense seq leaves (see SPEC "Multi-pod
// relay"). Local and remote delivery are the same contract; the local
// broadcast is merely the zero-hop case.
func (rl *Relay) CommitAndMirror(key RoomKey, commit func() (Frame, error)) error {
	mirror, err := rl.reg.commitAndMirror(key, commit)
	if err != nil {
		return err
	}
	rl.publishToPeers(key, mirror)
	return nil
}

// wsConn is the real connection adapter: it wraps a coder/websocket
// connection in the Conn interface the hub broadcasts to. Each connection
// owns a bounded buffered channel (the send buffer) and a single writer
// goroutine that drains it to the socket; Send is a non-blocking channel
// send that returns false when the buffer is full (the backpressure
// signal the hub acts on by dropping this connection).
type wsConn struct {
	id   uint64
	ws   *websocket.Conn
	send chan Frame

	// writeTimeout bounds each socket write. It is set from the relay's
	// CONFIGURED ping timeout (rl.pingTimeout), not the package constant, so
	// SetHeartbeat shortens the write reap window in lockstep with the ping
	// reap window: a write that does not complete within the timeout is a
	// dead socket, reaped on the same clock as a missed pong.
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

// Send enqueues f on the bounded buffer without blocking. A full buffer
// (the laggard case) returns false so the hub drops this connection; an
// already-closed connection also returns false. This is what makes the
// broadcast wait-free per client.
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

// Close stops the connection: it closes the done channel (stopping the
// writer + signaling the lifecycle) and aborts the socket. Idempotent.
func (c *wsConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		// CloseNow aborts without the closing handshake; the lifecycle's
		// normal path sends a clean close before calling here on a graceful
		// teardown, and a laggard / reap just needs the socket gone.
		_ = c.ws.CloseNow()
	})
}

// Serve runs a single accepted connection's full lifecycle: the late-join
// snapshot, the live stream, and the heartbeat, tearing everything down
// cleanly on any exit. The caller (the HTTP upgrade handler) has already
// Admitted the connection (so key/id reserve a hub slot) and accepted the
// websocket. Serve blocks until the connection ends, then returns; the
// handler's request goroutine is the connection's owner for its lifetime,
// which is correct since the websocket was hijacked out from under the
// http.Server's timeouts.
//
// The ordering that makes late-join correct (no gap, no dup) is the
// spec's register-FIRST sequence (SPEC "Persistence and late-join"):
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
// (seq <= S) - no gap. A frame delivered after the register whose effect
// the snapshot also caught is discarded by the client's seq <= S rule -
// no dup. Correctness rides the sequence, so it holds identically for
// frames arriving from peer pods.
func (rl *Relay) Serve(ctx context.Context, key RoomKey, id uint64, ws *websocket.Conn) {
	limits := rl.reg.limits
	ws.SetReadLimit(limits.MaxMessageBytes)

	c := newWSConn(id, ws, limits.SendBuffer, rl.pingTimeout)

	// Teardown: unregister from the hub + release the per-app slot exactly
	// once, and stop the writer. Done on every exit path (clean close,
	// reap, laggard drop, snapshot failure, shutdown).
	defer func() {
		c.Close()
		rl.reg.release(key, id)
	}()

	// 1+2. Late-join, register-first: joinWithSnapshot swaps the admit's
	// reservation for c under the hub lock (c is the broadcast target from
	// that instant), then reads the durable KV snapshot with no lock held
	// and returns it. A read error closes the connection: a client that
	// cannot be caught up is better off reconnecting (which re-syncs from
	// the KV).
	snap, err := rl.reg.joinWithSnapshot(key, c, func() (Frame, error) { return rl.buildSnapshot(key) })
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "join failed")
		return
	}

	// The reserved first-frame slot: write the snapshot DIRECTLY to the
	// socket before the writer goroutine starts draining the send buffer,
	// so the snapshot is the first frame ON THE WIRE even though live
	// frames may already be buffered behind it. (A room bursting faster
	// than the send buffer during the read can drop this joiner as a
	// laggard - it reconnects; correctness is unaffected.)
	if !c.writeFrame(ctx, snap) {
		return
	}

	// 3. Run the writer, reader, and heartbeat. writeLoop drains the send
	// buffer (any live frames queued during the join) to the socket;
	// readLoop pumps inbound frames into the hub broadcast and enforces the
	// per-frame size + rate limits; heartbeat pings and reaps.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop(ctx) }()
	go func() { defer wg.Done(); rl.readLoop(ctx, key, c) }()
	rl.heartbeat(ctx, c)
	c.Close()
	wg.Wait()
}

// buildSnapshot reads the room KV and encodes it as the late-join snapshot
// control frame.
func (rl *Relay) buildSnapshot(key RoomKey) (Frame, error) {
	kv, err := rl.snap.Scan(key.App, key.ID)
	if err != nil {
		return Frame{}, err
	}
	return encodeSnapshot(kv), nil
}

// writeFrame writes one frame to the socket under the bounded write
// timeout, closing the connection on error (a write that does not complete
// within the timeout is a dead socket, reaped the same as a missed pong).
// Reports whether the write succeeded. Used by writeLoop for every drained
// frame and by Serve for the snapshot's reserved first-frame write.
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

// writeLoop drains the send buffer to the socket. It exits when the
// connection is closed or ctx is done. A write error closes the
// connection (which the lifecycle's teardown finishes).
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

// readLoop pumps inbound frames into the room's broadcast and enforces the
// per-frame size limit (via SetReadLimit, which closes the socket on an
// over-cap frame) and the per-connection inbound rate limit. A read error
// (clean close, size violation, dead socket) ends the loop and closes the
// connection. The reader running is also what lets coder/websocket process
// incoming pong frames (so the heartbeat's Ping resolves) and respond to
// client-initiated pings with pongs - both handled inside Read.
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
		// Per-connection inbound rate limit: a client over its ceiling is
		// dropped so it cannot saturate the room's fan-out (one inbound
		// frame is multiplied by the room's connection count outbound).
		if !limit.allow() {
			_ = c.ws.Close(websocket.StatusPolicyViolation, "rate limit exceeded")
			c.Close()
			return
		}
		if hub == nil {
			hub = rl.reg.hub(key)
			if hub == nil {
				// Hub gone (room emptied + torn down). Nothing to fan out to;
				// this connection is on its way down too.
				c.Close()
				return
			}
		}
		f := Frame{Binary: typ == websocket.MessageBinary, Data: data}
		hub.broadcast(c.id, f)
		// Cross-pod fan-out of the ephemeral peer frame (free on the same
		// path the durable mirror uses; no seq, no contract - a lost or
		// reordered ephemeral frame needs no machinery). The per-connection
		// rate + size limits above ran BEFORE this point, so peer input is
		// bounded at the origin. No-op with no publisher (zero peers).
		rl.publishToPeers(key, f)
	}
}

// heartbeat pings the connection every pingInterval and reaps it if the
// pong does not return within pingTimeout. ws.Ping blocks until the pong
// is read (by readLoop) or the context deadline fires; a deadline error
// means the peer is unresponsive and the connection is closed. heartbeat
// returns (ending Serve's blocking call) when the connection or ctx is
// done.
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
				// Missed pong (or ctx done): the peer is dead. Reap it.
				c.Close()
				return
			}
		}
	}
}
