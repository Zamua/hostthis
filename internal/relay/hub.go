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
}

// newHub builds an empty hub for key with the given per-room connection
// cap. onEmpty (may be nil in unit tests) fires when the last connection
// leaves.
func newHub(key RoomKey, maxConn int, onEmpty func()) *Hub {
	return &Hub{
		key:     key,
		maxConn: maxConn,
		conns:   make(map[uint64]Conn),
		onEmpty: onEmpty,
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
	// Snapshot the recipient set under the lock, then Send outside the map
	// iteration's mutation window. Send itself never blocks, so holding the
	// lock across the Send calls is safe and keeps register / unregister
	// serialized against this fan-out.
	var laggards []Conn
	for id, c := range h.conns {
		if id == from {
			continue
		}
		if !c.Send(f) {
			laggards = append(laggards, c)
			delete(h.conns, id)
		}
	}
	empty := len(h.conns) == 0
	onEmpty := h.onEmpty
	h.mu.Unlock()

	// Close the dropped laggards outside the lock: closing triggers their
	// own lifecycle teardown (which calls unregister, a no-op now since we
	// already removed them here). Done outside the lock so a slow Close
	// cannot stall the room.
	for _, c := range laggards {
		c.Close()
	}
	if len(laggards) > 0 && empty && onEmpty != nil {
		// Dropping the last connection(s) emptied the hub; tear it down.
		onEmpty()
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
