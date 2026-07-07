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
	pending map[RoomKey]int     // in-flight admits per room (the pending-admit guard)
	nextID  uint64              // monotonic per-connection id source
	closing bool                // set by CloseAll; refuses new acquires

	// afterAdmitReserve is a test-only seam fired by admit AFTER it has
	// reserved the per-app slot + the pending-admit guard and released r.mu,
	// but BEFORE it takes the hub lock to register the reservation. It is nil
	// in production (the field is set only by tests that need to drive the
	// admit-reserved-but-not-registered window deterministically), so the
	// production path is unchanged. See the last-leave-vs-admit race test.
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
// Per-room isolation in the admission path: admit must NOT hold the global
// registry lock across any hub-lock work, so a join to ANY room never
// stalls a concurrent join to a DIFFERENT room behind that room's hub
// mutex. It therefore does the per-app + total-rooms accounting and the
// lazy hub create (the fast, map-only work the global lock must serialize)
// UNDER r.mu, RELEASES r.mu, and only then takes the target hub's lock for
// the per-room cap check + register. admit never holds r.mu and hub.mu at
// once, so there is no lock-order cycle to deadlock on. (The hub lock
// itself is a pure membership mutex - no storage I/O ever runs under it,
// see commitAndMirror - so the register can never stall behind a slow
// durable commit either.)
//
// The hub admit is about to register into is kept alive across the r.mu gap by
// the PENDING-ADMIT guard, not by emptiness. admit increments r.pending[key]
// under r.mu (alongside perApp++ and the id reservation) BEFORE it releases
// r.mu, and decrements it under r.mu AFTER hub.register returns (success OR
// rollback). Every hub-removal path - removeHub (the onEmpty callback fired
// when a room's LAST connection leaves) and admit's own rollback - deletes a
// hub only when it is empty AND has zero pending admits. So a hub an admit
// has reserved a slot for but not yet registered into is never torn out from
// under the register (the last-leave-vs-admit race: the departing last
// connection's onEmpty would otherwise remove the hub the admit is about to
// register into, orphaning the join and leaking its per-app slot). This
// keeps admit decoupled from r.mu across hub.register: admit still holds NO
// global lock while it takes the hub lock to register, so a join to one room
// never stalls on another room's hub contention.
//
// The order of checks: the total-rooms cap (an upgrade creating a NEW hub)
// and the per-app aggregate are checked under r.mu; the per-room cap is
// enforced inside hub.register under hub.mu. When a room is at its
// per-room cap AND its app is at the per-app cap at once, the per-app
// refusal wins (it is checked first, under r.mu) - a precedence the prior
// global-lock version reversed, but no contract pins which 429 a
// doubly-capped upgrade returns, and decoupling the hubs is worth it.
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

	// Lazily create the hub on the first connection to a room. The onEmpty
	// callback drops it from the registry when its last connection leaves;
	// the onDrop callback reclaims a laggard-dropped connection's per-app
	// slot (the broadcast-drop path does not route through release, so the
	// decApp must happen here or the slot leaks).
	hub, created := r.getOrCreateHubLocked(key)

	// Reserve the per-app slot optimistically AND register this admit as
	// in-flight (the pending-admit guard), both while still under r.mu, so the
	// cap check above and these increments are atomic against a concurrent
	// admit. The pending count keeps the hub from being torn out between here
	// and the register below (a concurrent commitAndMirror on this empty room
	// would otherwise remove the transient hub it created as "still empty").
	// If the per-room register below fails, the rollback path (decApp +
	// pending-- + transient-hub teardown) undoes it.
	id = r.nextConnID()
	r.perApp[key.App]++
	r.pending[key]++
	r.mu.Unlock()

	// Test-only seam: lets a test drive the admit-reserved-but-not-registered
	// window deterministically. nil in production.
	if r.afterAdmitReserve != nil {
		r.afterAdmitReserve(key)
	}

	// Per-room cap check + register, under hub.mu ALONE (r.mu already
	// released). The per-room cap is enforced HERE, so it is strict no matter
	// how concurrent same-hub admits interleave.
	ok := hub.register(newReservation(id))

	// Drop the pending-admit guard now that the register has run (whether it
	// succeeded or hit the per-room cap), under r.mu.
	r.clearPending(key)

	if !ok {
		// At the per-room cap. Roll back the optimistic per-app reservation and
		// tear down a hub we created for this admit alone (removeHub re-checks
		// emptiness AND pending == 0 under r.mu, so a real connection that
		// joined it in the meantime - or another in-flight admit - keeps it).
		// decApp + removeHub each take r.mu themselves.
		r.decApp(key.App)
		if created {
			r.removeHub(key)
		}
		return nil, 0, ErrRoomFull
	}
	return hub, id, nil
}

// clearPending drops one in-flight admit for key (the pending-admit guard),
// pruning the map entry at zero so it does not grow unbounded. admit calls it
// after hub.register returns. Lock order: it takes r.mu itself, so admit must
// NOT hold r.mu (or hub.mu) when calling it.
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

// reservation is a placeholder Conn that admit registers into the hub to
// hold the slot atomically with the cap check. joinWithSnapshot later swaps
// it for the real connection under the hub lock. A broadcast that hits the
// reservation (before the real conn is bound) is safely discarded: a frame
// broadcast before the real conn is registered came from a commit that
// completed before the join's snapshot read starts, so its effect is IN
// the snapshot (seq <= S) the joiner then receives - the missed frame is
// never a gap. The id is reserved from the instant of admission, so the
// caps hold against concurrent admits.
type reservation struct{ id uint64 }

func newReservation(id uint64) reservation { return reservation{id: id} }

func (reservation) Send(Frame) bool { return true }
func (reservation) Close()          {}
func (r reservation) ID() uint64    { return r.id }

// joinWithSnapshot completes a late-join in the spec's register-FIRST
// order (see SPEC.md "Persistence and late-join"): it swaps the admit's
// reservation for the real connection c UNDER the hub lock (a pure
// membership mutex - from that instant every broadcast is queued for c),
// then reads the room's durable snapshot OUTSIDE the lock and returns it.
// The caller sends the returned snapshot as the connection's FIRST wire
// frame (the reserved first-frame slot), ahead of any live frames that
// buffered on c during the read.
//
// Correctness rides the per-room sequence, not the lock:
//
//   - No gap. A mirror frame broadcast BEFORE the register came from a
//     commit that completed before the snapshot read started, so its
//     effect is IN the snapshot (seq <= S). A commit whose broadcast runs
//     after the register finds c in the set and is delivered live.
//   - No dup. A frame CAN be both delivered to c and reflected in the
//     snapshot (it broadcast inside the join window); the client's discard
//     rule (drop seq <= S) applies it exactly once. No server lock is
//     needed for this - which is the point: the same rule de-duplicates
//     frames arriving from PEER pods, which no pod-local lock could
//     serialize against.
//
// The hub lock is held only for the map swap; the snapshot read (a
// storage Scan - an object-store round trip on the shale backend) runs
// with NO lock held, so a slow read stalls only this join, never the
// room's fan-out.
//
// On a readSnapshot error the error is returned and no frame is sent; c
// stays registered until the caller's deferred release unregisters it
// (the caller closes the connection: a client that cannot be caught up
// reconnects).
func (r *Registry) joinWithSnapshot(key RoomKey, c Conn, readSnapshot func() (Frame, error)) (Frame, error) {
	r.mu.Lock()
	hub := r.hubs[key]
	r.mu.Unlock()
	if hub == nil {
		// Hub gone (the reservation's room was torn down between admit and
		// here - only possible if release already ran, which it has not).
		// Treat as a failed join.
		return Frame{}, errHubGone
	}

	hub.mu.Lock()
	// The reservation must still hold this id's slot; if not, the connection
	// was already released (e.g. shutdown) and we must not register.
	if _, ok := hub.conns[c.ID()]; !ok {
		hub.mu.Unlock()
		return Frame{}, errHubGone
	}
	// Register FIRST: c is the broadcast target from this instant, so no
	// later mirror can be missed. Frames delivered before the snapshot is
	// sent wait in c's send buffer behind the reserved first-frame slot.
	hub.conns[c.ID()] = c
	hub.mu.Unlock()

	// Snapshot read OUTSIDE the lock. Every commit that completed before
	// this read began is reflected in it (and counted by its exact S), which
	// is what makes the register-first order gapless.
	return readSnapshot()
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
		// Hub already gone. The only ways a hub disappears are (a) this
		// connection's own unregister emptied it (then its decApp already ran
		// below), (b) a laggard drop emptied it (then onDrop already did this
		// connection's decApp), or (c) shutdown discarded the registry. In
		// every case the per-app slot for this id was already reclaimed by the
		// path that removed the hub, so release must NOT decApp again here - a
		// second decrement would under-count a sibling connection of the same
		// app. A hub vanishing is therefore a no-op for accounting.
		return
	}
	// Decrement the app counter ONLY if the hub still holds this id - i.e.
	// this release is the one performing the unregister. If the id is already
	// gone (a double release, or a laggard drop that ran onDrop), the decApp
	// was already done by whoever removed it, so this is a no-op.
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

// getOrCreateHubLocked returns key's hub, creating an empty one (with the
// standard onEmpty / onDrop wiring) if none exists. created reports whether
// this call made it, so admit's per-room-cap rollback can tear down a hub
// it created for an admit that was then refused. Caller holds r.mu.
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

// commitAndMirror runs a durable write's KV COMMIT and then its live
// MIRROR broadcast: commit FIRST, with NO lock held, broadcast after a
// successful commit. The spec forbids storage I/O under the hub lock (see
// SPEC.md "Persistence and late-join": the hub lock guards the connection
// set only, and a room's live broadcasts never stall behind a durable
// write's object-storage round trip) - a shale commit is an object-store
// CAS taking 110ms-1.5s, and a hung storage call must wedge only ITS
// writer, never the room's ephemeral fan-out, peer deliveries, or joins.
//
//   - commit performs the durable KV write (the service-layer Put /
//     Delete) and returns the mirror frame to fan out, built AFTER the
//     write so it carries the per-room sequence the commit assigned.
//   - the mirror is then broadcast to the room's live hub, if one exists.
//     A room with NO live hub skips the local fan-out entirely: there are
//     no local subscribers to mirror to, and no transient hub is created -
//     a joiner racing this commit reads a snapshot whose exact S already
//     reflects it (the storage read is the serialization point, not a
//     lock), so nothing is missed.
//
// No-gap / no-dup against a concurrent join rides the SEQUENCE, not a
// critical section: the joiner registers first, snapshots second, and
// discards any frame with seq <= S (see joinWithSnapshot). A frame this
// broadcast delivers to a joiner whose snapshot also reflects the write is
// a wire-level duplicate the client's discard rule drops - the same rule
// that de-duplicates the frame when it arrives from a PEER pod, which no
// pod-local lock could order anyway.
//
// The successful mirror frame is returned to the caller (Relay), which
// publishes it to the peer pods - also off every lock, best-effort, never
// on the commit path. If commit fails, the error is returned and nothing
// is mirrored anywhere.
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

// removeHub deletes key's hub from the registry IF it is empty AND has no
// in-flight admit (the pending-admit guard). It is the onEmpty callback the
// hub fires when its last connection leaves, and the rollback path admit
// uses when the per-room cap refuses a register. Both conditions are
// re-checked under the registry lock so neither a connection that joined
// between the hub's "I am empty" signal and this call, NOR an admit that has
// reserved a slot but not yet registered its reservation into the hub, is
// torn out from under. The pending check is what closes the
// last-leave-vs-admit slot-leak race: a hub an admit is about to register
// into is never removed.
func (r *Registry) removeHub(key RoomKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hubs[key]; ok && h.len() == 0 && r.pending[key] == 0 {
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

// announceDrain broadcasts the reconnect drain hint to every connection of
// every hub, server-originated (from == 0). It closes NOTHING and sets no
// flag: the relay keeps serving through the operator's drain grace window
// so hint-acting clients re-home make-before-break; CloseAll runs after
// (see cmd/hostthisd's shutdown sequence and SPEC "Drain hint").
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
