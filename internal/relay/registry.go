package relay

import (
	"errors"
	"sync"

	"github.com/Zamua/hostthis/internal/domain"
)

// Limits is the relay's abuse posture (see SPEC.md "Limits and abuse
// posture"). Every in-memory structure is bounded; the room-UUID
// capability plus these caps are the whole posture, no new auth. A zero
// value on any field means "unlimited" for that axis (used in tests that
// isolate one limit); NewLimits sets the SPEC defaults.
type Limits struct {
	// MaxConnsPerRoom bounds one hub's client-set size and one room's
	// fan-out cost (a broadcast is O(connections)). Past it, a new upgrade
	// to that room is refused 429.
	MaxConnsPerRoom int
	// MaxConnsPerApp bounds the total live connections any one app's rooms
	// hold open in aggregate. Past it, a new upgrade under that app is
	// refused 429.
	MaxConnsPerApp int
	// MaxRooms bounds the hub registry itself: the count of distinct live
	// rooms cannot grow unbounded. Past it, an upgrade that would create a
	// NEW hub is refused 503; joins to already-live rooms still succeed.
	MaxRooms int
	// MaxMessageBytes bounds a single inbound frame. A frame over the cap
	// closes the connection (StatusMessageTooBig). Independent of the
	// per-room DURABLE byte cap - a relay frame is never persisted, so it
	// is bounded for memory, not storage.
	MaxMessageBytes int64
	// SendBuffer is the bounded per-client send-buffer depth (frames). A
	// client whose buffer is full when a broadcast tries to enqueue is
	// dropped (the backpressure mechanism). A small handful is correct: a
	// client that cannot keep up with a few buffered frames is not a viable
	// live participant and is better off reconnecting.
	SendBuffer int
	// MaxMsgsPerSec is the per-connection inbound send rate ceiling. A
	// client that exceeds it is dropped, so one hostile connection cannot
	// saturate a room's fan-out (every inbound frame is multiplied by the
	// room's connection count on the way out).
	MaxMsgsPerSec int
}

// SPEC defaults (see SPEC.md "Limits and abuse posture"). Each is a
// starting point, tunable as real usage informs it.
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

// Admission errors the upgrade handler maps to HTTP status codes. They
// are relay-layer sentinels so the HTTP handler does not reach into the
// registry's internals.
var (
	// ErrRoomFull: the per-room connection cap is hit. HTTP 429.
	ErrRoomFull = errors.New("relay: room connection cap reached")
	// ErrAppFull: the per-app aggregate connection cap is hit. HTTP 429.
	ErrAppFull = errors.New("relay: app connection cap reached")
	// ErrTooManyRooms: the service-wide live-room cap is hit and this
	// upgrade would create a NEW hub. HTTP 503. (Joins to already-live
	// rooms still succeed.)
	ErrTooManyRooms = errors.New("relay: too many active relay rooms")
	// errHubGone is an internal sentinel: the hub for a reserved connection
	// vanished before the late-join completed (a shutdown race). The
	// connection is closed without registering.
	errHubGone = errors.New("relay: hub gone before join completed")
)

// Registry maps a RoomKey to its live Hub and owns the per-app aggregate
// connection cap and the service-wide live-room cap. It is the only
// in-memory structure the upgrade handler interacts with directly, and
// every structure it holds is bounded.
//
// Concurrency: the registry mutex guards the hub map and the per-app
// counters. acquire (the admission + lazy-hub-create + register path) is
// done under it so the caps are checked atomically against the create /
// register, and an empty hub is removed under it so the room count is
// exact.
type Registry struct {
	limits Limits

	mu      sync.Mutex
	hubs    map[RoomKey]*Hub
	perApp  map[domain.Slug]int // live connection count per app
	nextID  uint64              // monotonic per-connection id source
	closing bool                // set by CloseAll; refuses new acquires
}

// NewRegistry builds an empty registry with the given limits.
func NewRegistry(limits Limits) *Registry {
	return &Registry{
		limits: limits,
		hubs:   make(map[RoomKey]*Hub),
		perApp: make(map[domain.Slug]int),
	}
}

// Limits returns the configured limits (the relay reads MaxMessageBytes /
// SendBuffer / MaxMsgsPerSec from here when wiring a connection).
func (r *Registry) Limits() Limits { return r.limits }

// nextConnID hands out a fresh, monotonically increasing connection id.
// Ids start at 1 so 0 is reserved for "no originating connection" in a
// server-originated broadcast (see Hub.broadcast). Caller holds r.mu.
func (r *Registry) nextConnID() uint64 {
	r.nextID++
	return r.nextID
}

// admit reserves a connection slot for key under all three caps and
// returns the hub to register into (creating it lazily) plus the fresh
// connection id, OR an admission error. The reservation increments the
// per-app counter; the caller MUST call release(key) exactly once when the
// connection ends so the counter is decremented (the relay does this in
// the connection's deferred teardown).
//
// The order of checks matters: per-room is checked against the existing
// hub's size; per-app against the running aggregate; the room cap only
// against the case where this upgrade would create a NEW hub. A join to an
// already-live room is never refused by the room cap.
func (r *Registry) admit(key RoomKey) (h *Hub, id uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, 0, ErrTooManyRooms
	}

	hub, exists := r.hubs[key]

	// Service-wide live-room cap: only an upgrade that would create a NEW
	// hub is refused; joins to already-live rooms still succeed.
	if !exists && r.limits.MaxRooms > 0 && len(r.hubs) >= r.limits.MaxRooms {
		return nil, 0, ErrTooManyRooms
	}

	// Per-room connection cap: checked against the existing hub size. A
	// not-yet-existing hub has zero connections, so a first connection
	// always passes this (assuming a positive cap).
	if exists && r.limits.MaxConnsPerRoom > 0 && hub.len() >= r.limits.MaxConnsPerRoom {
		return nil, 0, ErrRoomFull
	}

	// Per-app aggregate connection cap.
	if r.limits.MaxConnsPerApp > 0 && r.perApp[key.App] >= r.limits.MaxConnsPerApp {
		return nil, 0, ErrAppFull
	}

	// Lazily create the hub on the first connection to a room. The onEmpty
	// callback drops it from the registry when its last connection leaves.
	if !exists {
		hub = newHub(key, r.limits.MaxConnsPerRoom, func() { r.removeHub(key) })
		r.hubs[key] = hub
	}

	id = r.nextConnID()
	if !hub.register(newReservation(id)) {
		// Lost a race to the per-room cap between the size check and the
		// register (another acquire filled the last slot). Roll back a hub
		// we just created so we do not leak an empty hub.
		if !exists {
			delete(r.hubs, key)
		}
		return nil, 0, ErrRoomFull
	}
	r.perApp[key.App]++
	return hub, id, nil
}

// reservation is a placeholder Conn that admit registers into the hub to
// hold the slot atomically with the cap check. joinWithSnapshot later swaps
// it for the real connection under the hub lock. A broadcast that hits the
// reservation (before the real conn is bound) is safely discarded: the
// snapshot read in joinWithSnapshot happens under the same hub lock, AFTER
// any such broadcast, and that broadcast's write committed before its
// broadcast - so the discarded change is in the snapshot the joiner then
// receives. The id is reserved from the instant of admission, so the caps
// hold against concurrent admits.
type reservation struct{ id uint64 }

func newReservation(id uint64) reservation { return reservation{id: id} }

func (reservation) Send(Frame) bool { return true }
func (reservation) Close()          {}
func (r reservation) ID() uint64    { return r.id }

// joinWithSnapshot completes a late-join atomically: it reads the room's
// durable snapshot and registers the real connection c into the hub UNDER
// THE HUB'S LOCK, so no broadcast can interleave between the snapshot read
// and the register. This is the no-gap / no-dup guarantee the spec
// requires (see SPEC.md "Persistence and late-join"):
//
//   - snapshot is built (via the readSnapshot callback) and queued onto c
//     as the FIRST frame, then the reservation placeholder is swapped for
//     the real c (c enters the broadcast set) - all while h.mu is held, so
//     a concurrent MirrorDurable broadcast on this room is serialized
//     against it.
//   - A durable write whose broadcast already ran (before this lock was
//     acquired) committed to the KV before this snapshot read (which is
//     under the lock, after), so it is IN the snapshot - not missed (no
//     gap) and not also delivered live (c was not yet in the set when it
//     broadcast: no dup).
//   - A durable write whose broadcast runs AFTER this returns finds c in
//     the broadcast set and is delivered live - not missed (no gap), and it
//     post-dates the snapshot so it is not also in it (no dup).
//
// readSnapshot returns the frame to queue first plus an error; on error
// joinWithSnapshot returns it without registering c (the caller closes the
// connection: a client that cannot be caught up reconnects). enqueue is a
// non-blocking enqueue onto c's send buffer; the snapshot is the first
// frame so it always fits.
func (r *Registry) joinWithSnapshot(key RoomKey, c Conn, readSnapshot func() (Frame, error), enqueue func(Frame) bool) error {
	r.mu.Lock()
	hub := r.hubs[key]
	r.mu.Unlock()
	if hub == nil {
		// Hub gone (the reservation's room was torn down between admit and
		// here - only possible if release already ran, which it has not).
		// Treat as a failed join.
		return errHubGone
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()

	// The reservation must still hold this id's slot; if not, the connection
	// was already released (e.g. shutdown) and we must not register.
	if _, ok := hub.conns[c.ID()]; !ok {
		return errHubGone
	}

	snap, err := readSnapshot()
	if err != nil {
		return err
	}
	// Queue the snapshot as the first frame, THEN make c the broadcast
	// target. Both under h.mu: a broadcast cannot slip a live frame ahead of
	// the snapshot, and cannot be missed once c is in the set.
	enqueue(snap)
	hub.conns[c.ID()] = c
	return nil
}

// release ends a connection: it unregisters id from key's hub and
// decrements the per-app counter. Idempotent per (key, id): calling it
// twice for the same connection decrements the app counter once (guarded
// by whether the hub still held the id). The relay calls it exactly once
// in the connection's deferred teardown.
func (r *Registry) release(key RoomKey, id uint64) {
	r.mu.Lock()
	hub := r.hubs[key]
	r.mu.Unlock()
	if hub == nil {
		// Hub already gone (last-conn teardown raced us). Still decrement
		// the per-app counter for this connection.
		r.decApp(key.App)
		return
	}
	// Only decrement the app counter if the hub actually held this id (so a
	// double release does not over-decrement).
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

// hub returns the live hub for key, or nil if none. Used by the durable
// PUT/DELETE mirror path so a committed change is fanned out to the room's
// connected clients without the caller needing the registry internals.
func (r *Registry) hub(key RoomKey) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hubs[key]
}

// removeHub deletes key's hub from the registry IF it is empty. It is the
// onEmpty callback the hub fires when its last connection leaves. The
// emptiness is re-checked under the registry lock so a connection that
// joined between the hub's "I am empty" signal and this call is not torn
// out from under.
func (r *Registry) removeHub(key RoomKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hubs[key]; ok && h.len() == 0 {
		delete(r.hubs, key)
	}
}

// Rooms reports the number of live hubs (distinct active relay rooms).
// Used by tests and observability.
func (r *Registry) Rooms() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hubs)
}

// AppConns reports the live connection count for app. Used by tests.
func (r *Registry) AppConns(app domain.Slug) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perApp[app]
}

// CloseAll closes every connection in every hub (server shutdown). It sets
// the closing flag so no new acquire succeeds, then closes each hub's
// connections with a normal-closure status (driven by each connection's
// own Close) so clients reconnect on their backoff schedule rather than
// hammering instantly.
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
