package relay

import "sync"

// Hub is the in-memory registry for ONE room's live connections. The Registry
// creates a hub lazily on the first connection to a room and tears it down
// when the last connection leaves, so no idle empty hub lingers.
//
// Every method takes the hub's mutex, so concurrent register / broadcast /
// unregister is serialized. The broadcast is still wait-free per client: it
// calls Conn.Send, which enqueues on a bounded buffer and returns immediately,
// so one laggard cannot stall the fan-out to the rest of the room while the
// lock is held.
type Hub struct {
	key     RoomKey
	maxConn int

	mu    sync.Mutex
	conns map[uint64]Conn
	// onEmpty is invoked WITHOUT the hub lock held, at most once per
	// transition to zero connections. The Registry sets it to drop the hub
	// from the registry map.
	onEmpty func()
	// onDrop is invoked WITHOUT the hub lock held, once per connection the
	// broadcast drops as a laggard. That is the one teardown path removing a
	// connection WITHOUT going through the registry's release, which is what
	// decrements the per-app counter, so the Registry sets onDrop to decApp:
	// without it a laggard drop leaks a per-app slot. The connection's own
	// later release finds the id already gone and must not decApp again.
	onDrop func(id uint64)
}

// newHub builds an empty hub for key. onEmpty (nil-able) fires when the last
// connection leaves; onDrop (nil-able) fires once per laggard the broadcast
// drops so the registry can reclaim its per-app slot.
func newHub(key RoomKey, maxConn int, onEmpty func(), onDrop func(id uint64)) *Hub {
	return &Hub{
		key:     key,
		maxConn: maxConn,
		conns:   make(map[uint64]Conn),
		onEmpty: onEmpty,
		onDrop:  onDrop,
	}
}

// register adds c, returning false WITHOUT adding when the hub is at its
// per-room connection cap so the caller refuses the upgrade (HTTP 429). The
// add happens under the lock, so a concurrent broadcast either sees c and
// delivers to it or does not and the caller's snapshot already reflects that
// write: the no-gap / no-dup ordering the snapshot-then-stream join relies on.
// Enforcing the cap here rather than in the registry keeps it strict under
// concurrent admits while a register on one hub never stalls an admit to a
// different room.
func (h *Hub) register(c Conn) (ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxConn > 0 && len(h.conns) >= h.maxConn {
		return false
	}
	h.conns[c.ID()] = c
	return true
}

// unregister removes the connection with id and reports whether this call is
// the one that removed it, firing onEmpty outside the lock if that was the last
// one. It does NOT call Conn.Close: the connection's own lifecycle closes the
// socket, the hub only forgets it. Idempotent, and an unregister of an absent
// id reports false and does not fire onEmpty.
//
// The report is what lets the Registry reclaim the per-app slot exactly once.
// The removal and the answer to "did I remove it" come from the same critical
// section, so a laggard drop removing the same id concurrently cannot make two
// callers both believe they own the reclaim.
func (h *Hub) unregister(id uint64) (removed bool) {
	h.mu.Lock()
	if _, present := h.conns[id]; !present {
		h.mu.Unlock()
		return false
	}
	delete(h.conns, id)
	empty := len(h.conns) == 0
	onEmpty := h.onEmpty
	h.mu.Unlock()

	if empty && onEmpty != nil {
		onEmpty()
	}
	return true
}

// broadcast fans f out to every connection EXCEPT id == from, so a frame is
// never echoed to its sender. Connection ids start at 1, so from == 0 delivers
// to everyone: that is the server-originated frame (the live mirror of a
// durable PUT) with no originating socket to exclude.
//
// A recipient whose bounded buffer is full (Send returns false) is a laggard;
// it is collected and dropped AFTER the fan-out loop, so a slow client is
// ejected without ever blocking delivery to the rest of the room.
func (h *Hub) broadcast(from uint64, f Frame) {
	h.mu.Lock()
	cleanup := h.broadcastLocked(from, f)
	h.mu.Unlock()
	cleanup.run()
}

// broadcastDrops carries the post-broadcast cleanup that must run OUTSIDE the
// hub lock: reclaiming each dropped laggard's per-app slot, closing the
// dropped connections, and firing onEmpty if the drops emptied the hub. A slow
// Close or a registry-lock acquisition must never stall the room.
type broadcastDrops struct {
	laggards   []Conn
	laggardIDs []uint64
	onDrop     func(id uint64)
	onEmpty    func()
	empty      bool
}

func (d broadcastDrops) run() {
	// onDrop must run BEFORE Close so each laggard's per-app slot is accounted
	// before the connection's own (now no-op) release races in.
	if d.onDrop != nil {
		for _, id := range d.laggardIDs {
			d.onDrop(id)
		}
	}
	// Close triggers each laggard's own lifecycle teardown, whose release ->
	// unregister is a no-op now that the id is gone and onDrop did the decApp.
	for _, c := range d.laggards {
		c.Close()
	}
	if len(d.laggards) > 0 && d.empty && d.onEmpty != nil {
		d.onEmpty()
	}
}

// broadcastLocked is the fan-out body; the caller MUST already hold h.mu.
// Send never blocks, so holding the lock across the Sends is safe and keeps
// register / unregister serialized against the fan-out. It returns the laggard
// teardown for the caller to run after releasing the lock, since none of that
// (Close, the registry's decApp, a possible onEmpty) may stall the room. The
// hub lock is a pure membership mutex: no storage I/O runs under it, the
// durable commit happening before broadcast is called (registry.commitAndMirror).
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

func (h *Hub) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// closeAll closes every connection for server shutdown, leaving the hub empty.
// onEmpty is NOT fired: shutdown drops the whole registry at once.
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
