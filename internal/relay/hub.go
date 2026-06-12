package relay

import "sync"

// Hub is the in-memory registry for ONE room's live connections. It owns
// the set of connected clients, the register / unregister path, and the
// broadcast fan-out. A hub is created lazily on the first connection to a
// room (by the Registry) and torn down when its last connection leaves,
// so no idle empty hub lingers.
//
// The hub is the testable core of the relay: it talks only to the Conn
// interface and a per-room connection cap, so register / broadcast /
// drop-a-laggard / teardown are all exercised with a fake Conn, no real
// socket.
//
// Concurrency: every method takes the hub's mutex, so concurrent register
// / broadcast / unregister from many goroutines is serialized and
// race-free. The broadcast is wait-free with respect to any INDIVIDUAL
// client: it calls Conn.Send, which enqueues on a bounded buffer and
// returns immediately (a slow client is dropped, never waited on), so one
// laggard cannot stall the fan-out to the rest of the room while the lock
// is held.
type Hub struct {
	key     RoomKey
	maxConn int

	mu    sync.Mutex
	conns map[uint64]Conn
	// onEmpty is invoked, WITHOUT the hub lock held, the first time the
	// hub transitions to zero connections. The Registry sets it to remove
	// the hub from the registry map so an empty hub does not linger. It is
	// called at most once per "became empty" transition.
	onEmpty func()
	// onDrop is invoked, WITHOUT the hub lock held, once per connection the
	// broadcast DROPS as a laggard - the one teardown path that removes a
	// connection from the hub WITHOUT going through the registry's release
	// (which is what does the per-app counter decrement). The Registry sets
	// it to decApp for the dropped connection, so a laggard-dropped slot is
	// reclaimed exactly like a cleanly-released one; the connection's own
	// later release then finds the id already gone and is a no-op (it must
	// NOT decApp a second time). Without this, a laggard drop removes the id
	// from the hub map but never decrements the per-app counter, leaking a
	// slot per dropped connection (caught by the multi-client churn test).
	onDrop func(id uint64)
}

// newHub builds an empty hub for key with the given per-room connection
// cap. onEmpty (may be nil in unit tests) fires when the last connection
// leaves; onDrop (may be nil) fires once per laggard the broadcast drops so
// the registry can reclaim its per-app slot.
func newHub(key RoomKey, maxConn int, onEmpty func(), onDrop func(id uint64)) *Hub {
	return &Hub{
		key:     key,
		maxConn: maxConn,
		conns:   make(map[uint64]Conn),
		onEmpty: onEmpty,
		onDrop:  onDrop,
	}
}

// register adds c to the hub. It returns false WITHOUT adding when the
// hub is already at its per-room connection cap, so the caller refuses the
// upgrade (HTTP 429). The add is done under the lock so a concurrent
// broadcast either sees c (and delivers to it) or does not (and the caller
// will deliver the snapshot that already reflects that write) - the
// no-gap / no-dup ordering the snapshot-then-stream join relies on.
func (h *Hub) register(c Conn) (ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxConn > 0 && len(h.conns) >= h.maxConn {
		return false
	}
	h.conns[c.ID()] = c
	return true
}

// unregister removes the connection with id from the hub. If that was the
// last connection, onEmpty is invoked (outside the lock) so the Registry
// can drop the now-empty hub. unregister does NOT call Conn.Close - the
// connection's own lifecycle closes the socket; the hub only forgets it.
// It is idempotent: unregistering an id that is not present is a no-op
// and does NOT fire onEmpty (the hub was already empty / never held it).
func (h *Hub) unregister(id uint64) {
	h.mu.Lock()
	if _, present := h.conns[id]; !present {
		h.mu.Unlock()
		return
	}
	delete(h.conns, id)
	empty := len(h.conns) == 0
	onEmpty := h.onEmpty
	h.mu.Unlock()

	if empty && onEmpty != nil {
		onEmpty()
	}
}

// broadcast fans f out to every connection in the hub EXCEPT the one with
// id == from (a message is relayed to all OTHER clients in the room, never
// echoed to the sender). A from of 0 is never a real connection id (ids
// start at 1), so broadcast(0, f) delivers to everyone - used for a
// server-originated frame (the live mirror of a durable PUT) that has no
// originating socket to exclude.
//
// Backpressure: for each recipient it calls Conn.Send, which enqueues on
// the recipient's bounded buffer and returns immediately. A recipient
// whose buffer is FULL (Send returns false) is a laggard that cannot keep
// up; broadcast collects it and drops it AFTER the fan-out loop - it
// closes the laggard's connection and unregisters it - so the slow client
// is ejected without ever blocking the broadcast to the rest of the room.
// The broadcast is therefore O(connections) and wait-free per client.
func (h *Hub) broadcast(from uint64, f Frame) {
	h.mu.Lock()
	cleanup := h.broadcastLocked(from, f)
	h.mu.Unlock()
	cleanup.run()
}

// broadcastDrops carries the post-broadcast cleanup that must run OUTSIDE
// the hub lock: reclaiming each dropped laggard's per-app slot, closing the
// dropped connections, and (if the drops emptied the hub) firing onEmpty.
// broadcastLocked builds it under the lock; the caller runs it after
// releasing the lock, so a slow Close or a registry-lock acquisition never
// stalls the room while the hub lock is held.
type broadcastDrops struct {
	laggards   []Conn
	laggardIDs []uint64
	onDrop     func(id uint64)
	onEmpty    func()
	empty      bool
}

func (d broadcastDrops) run() {
	// Reclaim each dropped laggard's per-app slot. This is the one teardown
	// path that removes a connection WITHOUT routing through the registry's
	// release, so the registry must be told to decApp here or the slot leaks.
	// onDrop runs BEFORE Close so the slot is accounted before the
	// connection's own (now no-op) release races in.
	if d.onDrop != nil {
		for _, id := range d.laggardIDs {
			d.onDrop(id)
		}
	}
	// Close the dropped laggards: closing triggers their own lifecycle
	// teardown (which calls release -> unregister, both no-ops now since we
	// already removed them and onDrop did the decApp).
	for _, c := range d.laggards {
		c.Close()
	}
	if len(d.laggards) > 0 && d.empty && d.onEmpty != nil {
		// Dropping the last connection(s) emptied the hub; tear it down.
		d.onEmpty()
	}
}

// broadcastLocked is the fan-out body; the caller MUST already hold h.mu. It
// snapshots the recipient set, Sends to each (Send never blocks, so holding
// the lock across the Sends is safe and keeps register / unregister
// serialized against this fan-out), removes laggards from the map, and
// returns the cleanup the caller runs after releasing the lock. It exists as
// a separate method so the commit-and-mirror path (registry.commitAndMirror)
// can fan out the live mirror under the SAME hub-lock acquisition that the
// durable commit runs in, making the two atomic against a join's
// snapshot-read + register.
func (h *Hub) broadcastLocked(from uint64, f Frame) broadcastDrops {
	var laggards []Conn
	var laggardIDs []uint64
	for id, c := range h.conns {
		if id == from {
			continue
		}
		if !c.Send(f) {
			laggards = append(laggards, c)
			laggardIDs = append(laggardIDs, id)
			delete(h.conns, id)
		}
	}
	return broadcastDrops{
		laggards:   laggards,
		laggardIDs: laggardIDs,
		onDrop:     h.onDrop,
		onEmpty:    h.onEmpty,
		empty:      len(h.conns) == 0,
	}
}

// len reports the current connection count. Used by tests and the
// registry's per-app accounting.
func (h *Hub) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// closeAll closes every connection in the hub (server shutdown). Each
// Close triggers the connection's own teardown; the hub is left empty.
// onEmpty is NOT fired here - shutdown drops the whole registry at once.
func (h *Hub) closeAll() {
	h.mu.Lock()
	conns := make([]Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = make(map[uint64]Conn)
	h.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}
