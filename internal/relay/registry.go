package relay

import (
	"errors"
	"sync"

	"github.com/Zamua/hostthis/internal/domain"
)

// Limits is the relay's abuse posture (SPEC.md "Limits and abuse posture").
// Every in-memory structure is bounded; the room-UUID capability plus these caps
// are the whole posture, with no additional auth. A zero field means "unlimited"
// on that axis (used by tests isolating one limit); NewLimits sets the SPEC
// defaults.
type Limits struct {
	// MaxConnsPerRoom bounds one hub's client set and one room's fan-out cost
	// (a broadcast is O(connections)). Past it, a new upgrade to that room is
	// refused 429.
	MaxConnsPerRoom int
	// MaxConnsPerApp bounds the live connections one app's rooms hold open in
	// aggregate. Past it, a new upgrade under that app is refused 429.
	MaxConnsPerApp int
	// MaxRooms bounds the count of distinct live rooms. Past it, an upgrade
	// that would create a NEW hub is refused 503; joins to already-live rooms
	// still succeed.
	MaxRooms int
	// MaxMessageBytes bounds a single inbound frame; over it the connection
	// closes (StatusMessageTooBig). Distinct from the per-room DURABLE byte
	// cap: a relay frame is never persisted, so this bounds memory, not
	// storage.
	MaxMessageBytes int64
	// SendBuffer is the bounded per-client send-buffer depth in frames. A
	// client whose buffer is full when a broadcast tries to enqueue is dropped:
	// that is the backpressure mechanism. A small handful is correct, since a
	// client that cannot keep up with a few buffered frames is better off
	// reconnecting than holding a live slot.
	SendBuffer int
	// MaxMsgsPerSec is the per-connection inbound rate ceiling; a client past
	// it is dropped. Without it one hostile connection saturates a room's
	// fan-out, since every inbound frame is multiplied by the room's connection
	// count on the way out.
	MaxMsgsPerSec int
}

// SPEC defaults (SPEC.md "Limits and abuse posture").
const (
	DefaultMaxConnsPerRoom = 64
	DefaultMaxConnsPerApp  = 1024
	DefaultMaxRooms        = 4096
	DefaultMaxMessageBytes = 32 << 10 // 32 KiB
	DefaultSendBuffer      = 16
	DefaultMaxMsgsPerSec   = 120
)

// NewLimits returns the SPEC defaults.
func NewLimits() Limits {
	return Limits{
		MaxConnsPerRoom: DefaultMaxConnsPerRoom,
		MaxConnsPerApp:  DefaultMaxConnsPerApp,
		MaxRooms:        DefaultMaxRooms,
		MaxMessageBytes: DefaultMaxMessageBytes,
		SendBuffer:      DefaultSendBuffer,
		MaxMsgsPerSec:   DefaultMaxMsgsPerSec,
	}
}

// Admission errors the upgrade handler maps to HTTP status codes. Relay-layer
// sentinels, so the HTTP handler never reaches into the registry's internals.
var (
	// ErrRoomFull: the per-room connection cap is hit. HTTP 429.
	ErrRoomFull = errors.New("relay: room connection cap reached")
	// ErrAppFull: the per-app aggregate connection cap is hit. HTTP 429.
	ErrAppFull = errors.New("relay: app connection cap reached")
	// ErrTooManyRooms: the live-room cap is hit and this upgrade would create
	// a NEW hub. HTTP 503; joins to already-live rooms still succeed.
	ErrTooManyRooms = errors.New("relay: too many active relay rooms")
	// errHubGone is internal: the hub for a reserved connection vanished
	// before the late-join completed (a shutdown race), so the connection is
	// closed without registering.
	errHubGone = errors.New("relay: hub gone before join completed")
)

// Registry maps a RoomKey to its live Hub and owns the per-app aggregate
// connection cap and the service-wide live-room cap. It is the only in-memory
// structure the upgrade handler touches directly, and everything it holds is
// bounded.
//
// Concurrency: r.mu guards the hub map and the per-app counters. Admission's
// map-only accounting runs under it so the caps are checked atomically against
// the lazy create, and an empty hub is removed under it so the room count stays
// exact.
type Registry struct {
	limits Limits

	mu      sync.Mutex
	hubs    map[RoomKey]*Hub
	perApp  map[domain.Slug]int // live connection count per app
	pending map[RoomKey]int     // in-flight admits per room (the pending-admit guard)
	nextID  uint64              // monotonic per-connection id source
	closing bool                // set by CloseAll; refuses new acquires

	// afterAdmitReserve is a test-only seam fired by admit AFTER it reserves
	// the per-app slot plus the pending-admit guard and releases r.mu, but
	// BEFORE it takes the hub lock to register the reservation. Nil in
	// production; set only by tests driving that window deterministically (see
	// the last-leave-vs-admit race test).
	afterAdmitReserve func(key RoomKey)
}

// NewRegistry builds an empty registry with the given limits.
func NewRegistry(limits Limits) *Registry {
	return &Registry{
		limits:  limits,
		hubs:    make(map[RoomKey]*Hub),
		perApp:  make(map[domain.Slug]int),
		pending: make(map[RoomKey]int),
	}
}

// Limits returns the configured limits. The relay reads MaxMessageBytes,
// SendBuffer and MaxMsgsPerSec from here when wiring a connection.
func (r *Registry) Limits() Limits { return r.limits }

// nextConnID hands out a fresh, monotonically increasing connection id. Ids
// start at 1 so 0 stays reserved for "no originating connection" in a
// server-originated broadcast (see Hub.broadcast). Caller holds r.mu.
func (r *Registry) nextConnID() uint64 {
	r.nextID++
	return r.nextID
}

// admit reserves a connection slot for key under all three caps and returns the
// hub to register into (creating it lazily) plus a fresh connection id. The
// reservation increments the per-app counter, so the caller MUST call
// release(key) exactly once when the connection ends.
//
// # Per-room isolation
//
// admit must NOT hold the registry lock across any hub-lock work, or a join to
// one room stalls a concurrent join to a different room. So the map-only
// accounting and lazy hub create run under r.mu, r.mu is RELEASED, and only then
// does it take the target hub's lock to check the per-room cap and register. It
// never holds r.mu and hub.mu at once, so there is no lock-order cycle, and the
// hub lock covers pure membership with no storage I/O, so a register cannot
// stall behind a durable commit.
//
// # Keeping the hub alive across the r.mu gap
//
// By a pending-admit guard, not by emptiness: r.pending[key] is incremented
// under r.mu before releasing it and decremented after hub.register returns,
// success or rollback. Every hub-removal path deletes a hub only when it is
// empty AND has zero pending admits. Without that, a departing last connection's
// onEmpty could remove the hub an admit is about to register into, orphaning the
// join and leaking its per-app slot.
//
// # Order of checks
//
// The total-rooms cap and the per-app aggregate are checked under r.mu, the
// per-room cap inside hub.register under hub.mu. A room at its per-room cap
// whose app is also at the per-app cap gets the per-app refusal, since that is
// checked first; no contract pins which 429 a doubly-capped upgrade returns.
func (r *Registry) admit(key RoomKey) (h *Hub, id uint64, err error) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return nil, 0, ErrTooManyRooms
	}

	_, exists := r.hubs[key]

	// Service-wide live-room cap: only an upgrade that would create a NEW
	// hub is refused; joins to already-live rooms still succeed.
	if !exists && r.limits.MaxRooms > 0 && len(r.hubs) >= r.limits.MaxRooms {
		r.mu.Unlock()
		return nil, 0, ErrTooManyRooms
	}

	// Per-app aggregate connection cap (map-only, so it stays under r.mu).
	if r.limits.MaxConnsPerApp > 0 && r.perApp[key.App] >= r.limits.MaxConnsPerApp {
		r.mu.Unlock()
		return nil, 0, ErrAppFull
	}

	// The onEmpty callback drops the hub when its last connection leaves; the
	// onDrop callback reclaims a laggard-dropped connection's per-app slot,
	// which must happen there because the broadcast-drop path does not route
	// through release and the slot would otherwise leak.
	hub, created := r.getOrCreateHubLocked(key)

	// Both increments happen under the same r.mu hold as the cap check above,
	// so they are atomic against a concurrent admit. The pending count keeps
	// the hub from being torn out between here and the register below, where a
	// departing LAST connection's onEmpty would otherwise removeHub the very
	// hub this admit is about to register into.
	id = r.nextConnID()
	r.perApp[key.App]++
	r.pending[key]++
	r.mu.Unlock()

	if r.afterAdmitReserve != nil {
		r.afterAdmitReserve(key)
	}

	// Per-room cap check plus register, under hub.mu ALONE. Enforcing the cap
	// HERE makes it strict no matter how concurrent same-hub admits interleave.
	ok := hub.register(newReservation(id))

	// The register has run (succeeded or hit the cap), so the guard drops.
	r.clearPending(key)

	if !ok {
		// At the per-room cap: roll back the optimistic per-app reservation and
		// tear down a hub created for this admit alone. removeHub re-checks
		// emptiness AND pending == 0 under r.mu, so a real connection that
		// joined in the meantime, or another in-flight admit, keeps the hub.
		// decApp and removeHub each take r.mu themselves.
		r.decApp(key.App)
		if created {
			r.removeHub(key)
		}
		return nil, 0, ErrRoomFull
	}
	return hub, id, nil
}

// clearPending drops one in-flight admit for key, pruning the map entry at zero
// so it cannot grow unbounded. Lock order: it takes r.mu itself, so the caller
// must hold neither r.mu nor hub.mu.
func (r *Registry) clearPending(key RoomKey) {
	r.mu.Lock()
	if r.pending[key] > 0 {
		r.pending[key]--
	}
	if r.pending[key] == 0 {
		delete(r.pending, key)
	}
	r.mu.Unlock()
}

// reservation is a placeholder Conn that admit registers into the hub, holding
// the slot atomically with the cap check from the instant of admission so the
// caps hold against concurrent admits. joinWithSnapshot later swaps it for the
// real connection under the hub lock. A broadcast that hits the reservation is
// safely discarded: it came from a commit that completed before the join's
// snapshot read started, so its effect is IN the snapshot (seq <= S) the joiner
// receives, and the discarded frame is never a gap.
type reservation struct{ id uint64 }

func newReservation(id uint64) reservation { return reservation{id: id} }

func (reservation) Send(Frame) bool { return true }
func (reservation) Close()          {}
func (r reservation) ID() uint64    { return r.id }

// joinWithSnapshot completes a late-join in register-FIRST order: it swaps the
// admit's reservation for the real connection under the hub lock, so from that
// instant every broadcast queues for c, then reads the durable snapshot OUTSIDE
// the lock. The caller sends that snapshot as the connection's first frame,
// ahead of any live frames buffered during the read.
//
// Correctness rides the per-room sequence, not the lock:
//
//   - No gap. A frame broadcast before the register came from a commit that
//     completed before the snapshot read began, so its effect is IN the
//     snapshot. A commit broadcasting after the register finds c registered.
//   - No dup. A frame can be both delivered and reflected in the snapshot; the
//     client's discard rule (drop seq <= S) applies it exactly once. Needing no
//     server lock is the point: the same rule de-duplicates frames from PEER
//     pods, which no pod-local lock could serialize against.
//
// The hub lock covers only the map swap. The snapshot read is an object-store
// round trip on the shale backend, so holding a lock across it would stall the
// room's whole fan-out rather than just this join.
//
// On a read error nothing is sent and c stays registered until the caller's
// deferred release; the caller closes the connection and the client reconnects.
func (r *Registry) joinWithSnapshot(key RoomKey, c Conn, readSnapshot func() (Frame, error)) (Frame, error) {
	r.mu.Lock()
	hub := r.hubs[key]
	r.mu.Unlock()
	if hub == nil {
		// The reservation's room was torn down between admit and here. Treat
		// as a failed join.
		return Frame{}, errHubGone
	}

	hub.mu.Lock()
	// The reservation must still hold this id's slot. If it does not, the
	// connection was already released (shutdown), so registering is wrong.
	if _, ok := hub.conns[c.ID()]; !ok {
		hub.mu.Unlock()
		return Frame{}, errHubGone
	}
	// Register FIRST: c is the broadcast target from this instant, so no later
	// mirror can be missed. Frames delivered before the snapshot is sent wait
	// in c's send buffer behind the reserved first-frame slot.
	hub.conns[c.ID()] = c
	hub.mu.Unlock()

	// Outside the lock. Every commit that completed before this read began is
	// reflected in it and counted by its exact S, which is what makes the
	// register-first order gapless.
	return readSnapshot()
}

// release ends a connection: it unregisters id from key's hub and decrements the
// per-app counter. Idempotent per (key, id), guarded by whether the hub still
// holds the id, so a second call decrements nothing.
func (r *Registry) release(key RoomKey, id uint64) {
	r.mu.Lock()
	hub := r.hubs[key]
	r.mu.Unlock()
	if hub == nil {
		// A hub disappears only when this connection's own unregister emptied
		// it (its decApp already ran below), a laggard drop emptied it (onDrop
		// already did this connection's decApp), or shutdown discarded the
		// registry. In every case the per-app slot was already reclaimed by
		// the path that removed the hub, so decrementing again here would
		// under-count a sibling connection of the same app.
		return
	}
	// Decrement ONLY if the hub still holds this id, meaning this release is
	// the one performing the unregister. If the id is already gone (a double
	// release, or a laggard drop that ran onDrop), whoever removed it already
	// did the decApp.
	hub.mu.Lock()
	_, held := hub.conns[id]
	hub.mu.Unlock()
	if !held {
		return
	}
	hub.unregister(id)
	r.decApp(key.App)
}

func (r *Registry) decApp(app domain.Slug) {
	r.mu.Lock()
	if r.perApp[app] > 0 {
		r.perApp[app]--
	}
	if r.perApp[app] == 0 {
		delete(r.perApp, app)
	}
	r.mu.Unlock()
}

// hub returns the live hub for key, or nil. It lets the durable PUT/DELETE
// mirror path fan a committed change out to the room's clients without reaching
// into the registry internals.
func (r *Registry) hub(key RoomKey) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hubs[key]
}

// getOrCreateHubLocked returns key's hub, creating an empty one (with the
// standard onEmpty / onDrop wiring) if none exists. created reports whether this
// call made it, so admit's per-room-cap rollback can tear down a hub it created
// for an admit that was then refused. Caller holds r.mu.
func (r *Registry) getOrCreateHubLocked(key RoomKey) (hub *Hub, created bool) {
	if hub = r.hubs[key]; hub != nil {
		return hub, false
	}
	hub = newHub(key, r.limits.MaxConnsPerRoom,
		func() { r.removeHub(key) },
		func(uint64) { r.decApp(key.App) },
	)
	r.hubs[key] = hub
	return hub, true
}

// commitAndMirror runs a durable write's commit with no lock held, then
// broadcasts its mirror frame only if the commit succeeded.
//
// Storage I/O must never run under the hub lock: a shale commit is an
// object-store CAS, slow on a loaded backend and unbounded when the store hangs,
// so holding the lock across it would wedge the room's fan-out, peer deliveries
// and joins rather than just its own writer.
//
//   - commit does the durable write and returns the mirror frame, built AFTER
//     it so the frame carries the sequence the commit assigned.
//   - the mirror broadcasts to the live hub if one exists. A room with no hub
//     skips the fan-out and creates no transient one: a joiner racing this
//     commit reads a snapshot whose S already reflects it, since the storage
//     read is the serialization point.
//
// No-gap and no-dup against a concurrent join ride the sequence, not a critical
// section: the joiner registers first, snapshots second, and discards frames
// with seq <= S. That same rule de-duplicates frames arriving from peer pods.
//
// The mirror is returned to Relay, which publishes it to peers off every lock,
// best-effort. On commit failure nothing is mirrored anywhere.
func (r *Registry) commitAndMirror(key RoomKey, commit func() (Frame, error)) (Frame, error) {
	mirror, err := commit()
	if err != nil {
		return Frame{}, err
	}
	if hub := r.hub(key); hub != nil {
		hub.broadcast(0, mirror)
	}
	return mirror, nil
}

// removeHub deletes key's hub IF it is empty AND has no in-flight admit. It
// serves as the hub's onEmpty callback and as admit's rollback path when the
// per-room cap refuses a register. Both conditions are re-checked under r.mu, so
// neither a connection that joined between the hub's "I am empty" signal and
// this call, nor an admit that reserved a slot but has not yet registered its
// reservation, is torn out from under. The pending check is what closes the
// last-leave-vs-admit slot-leak race.
func (r *Registry) removeHub(key RoomKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hubs[key]; ok && h.len() == 0 && r.pending[key] == 0 {
		delete(r.hubs, key)
	}
}

// Rooms reports the number of live hubs (distinct active relay rooms).
func (r *Registry) Rooms() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hubs)
}

// AppConns reports the live connection count for app.
func (r *Registry) AppConns(app domain.Slug) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perApp[app]
}

// announceDrain broadcasts the reconnect drain hint to every connection of every
// hub, server-originated (from == 0). It closes NOTHING and sets no flag: the
// relay keeps serving through the operator's drain grace window so hint-acting
// clients re-home make-before-break, and CloseAll runs after (see cmd/hostthisd's
// shutdown sequence and SPEC "Drain hint").
func (r *Registry) announceDrain() {
	r.mu.Lock()
	hubs := make([]*Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		hubs = append(hubs, h)
	}
	r.mu.Unlock()
	hint := encodeReconnect()
	for _, h := range hubs {
		h.broadcast(0, hint)
	}
}

// CloseAll closes every connection in every hub at shutdown. It sets the closing
// flag so no new admit succeeds, then closes each hub's connections with a
// normal-closure status so clients reconnect on their backoff schedule rather
// than hammering instantly.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	r.closing = true
	hubs := make([]*Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		hubs = append(hubs, h)
	}
	r.mu.Unlock()
	for _, h := range hubs {
		h.closeAll()
	}
}
