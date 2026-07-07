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
	// production path is unchanged. See the transient-hub race test.
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
// stalls a concurrent join to (or durable commit on) a DIFFERENT room
// behind that room's hub mutex. It therefore does the per-app + total-rooms
// accounting and the lazy hub create (the fast, map-only work the global
// lock must serialize) UNDER r.mu, RELEASES r.mu, and only then takes the
// target hub's lock for the per-room cap check + register. admit never holds
// r.mu and hub.mu at once, so it cannot stall on a concurrent
// commitAndMirror that is holding the same hub's lock across a slow KV
// commit; and since the two never overlap their lock holds in admit, there
// is no lock-order cycle to deadlock on.
//
// The hub admit is about to register into is kept alive across the r.mu gap by
// the PENDING-ADMIT guard, not by emptiness. admit increments r.pending[key]
// under r.mu (alongside perApp++ and the id reservation) BEFORE it releases
// r.mu, and decrements it under r.mu AFTER hub.register returns (success OR
// rollback). Every hub-removal path - removeHub (the onEmpty callback),
// commitAndMirror's transient cleanup, and admit's own rollback - deletes a hub
// only when it is empty AND has zero pending admits. So a hub an admit has
// reserved a slot for but not yet registered into is never torn out from under
// the register, even by a concurrent commitAndMirror that CREATED a transient
// hub for the same empty room and would otherwise remove it as "still empty."
// This closes the transient-hub slot-leak race while keeping admit decoupled
// from r.mu across hub.register: admit still holds NO global lock while it takes
// the hub lock to register, so a join to one room never stalls on another
// room's hub contention.
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
		// tear down a hub we created transiently (removeHub re-checks emptiness
		// AND pending == 0 under r.mu, so a real connection that joined it in
		// the meantime - or another in-flight admit - keeps it). decApp +
		// removeHub each take r.mu themselves.
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
//     a concurrent commit-and-mirror broadcast on this room is serialized
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
// this call made it, so the commit-and-mirror path can tear down a hub it
// created transiently for a room with no live connections. Caller holds r.mu.
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

// commitAndMirror runs a durable write's KV COMMIT and its live MIRROR
// broadcast as ONE critical section under the room's hub lock, so a
// concurrent join's snapshot-read + register cannot interleave between them.
// This is the no-dup atomicity the spec requires (see SPEC.md "Persistence
// and late-join"): a durable write ends up in EXACTLY ONE of {a joiner's
// snapshot, a joiner's live stream}, never both (dup) and never neither
// (gap).
//
//   - commit performs the durable KV write (it is the service-layer Put /
//     Delete) and returns the mirror frame to fan out, built AFTER the
//     write so it carries the per-room sequence the commit assigned. It
//     runs UNDER the hub lock, so the room's live broadcasts wait
//     on it for its duration. This is the latency characteristic design (a)
//     accepts: the durable path is low-frequency, so the stall is acceptable;
//     high-frequency live motion rides ephemeral raw relay frames that never
//     take this path. The lock is the per-room hub mutex (not a global lock),
//     so one room's durable write never stalls another room.
//   - the returned mirror is the live frame fanned out after a successful
//     commit. It is broadcast under the SAME hub-lock acquisition
//     (broadcastLocked), so a join serialized on this hub either snapshots
//     after the commit (key in snapshot, and it registered after the mirror
//     so it is NOT also live) or before it (key not yet in snapshot, but it
//     registered before the mirror so it DOES receive it live) - exactly one.
//
// The successful mirror frame is returned to the caller (Relay), which
// publishes it to the peer pods OUTSIDE this lock - cross-pod ordering
// rides the frame's seq, not this lock, so holding the lock across the
// publish would buy nothing and cost the room's local broadcasts.
//
// If commit fails, the error is returned and nothing is mirrored. A hub is
// created transiently for a room with no live connections (so the commit
// still serializes against a join that is admitting concurrently, closing the
// gap that a "skip the mirror when no hub" check would open); it is removed
// again if it is still empty afterward, so an empty hub never lingers.
func (r *Registry) commitAndMirror(key RoomKey, commit func() (Frame, error)) (Frame, error) {
	r.mu.Lock()
	hub, created := r.getOrCreateHubLocked(key)
	// Take the hub lock BEFORE releasing the registry lock (lock order
	// r.mu -> hub.mu, the same order admit uses), so no concurrent join can
	// slip its snapshot-read + register between our commit and our mirror.
	hub.mu.Lock()
	r.mu.Unlock()

	mirror, err := commit()
	if err != nil {
		hub.mu.Unlock()
		// removeHub is a no-op unless the hub is still empty, so a real
		// connection that joined the transient hub during the commit keeps it.
		if created {
			r.removeHub(key)
		}
		return Frame{}, err
	}
	drops := hub.broadcastLocked(0, mirror)
	hub.mu.Unlock()
	drops.run()

	// Tear down a hub THIS commit created transiently for a room with no live
	// connections, IF it is still empty (removeHub re-checks emptiness, so a
	// connection that joined during the commit keeps the hub). A pre-existing
	// hub is owned by its connections' own teardown.
	if created {
		r.removeHub(key)
	}
	return mirror, nil
}

// removeHub deletes key's hub from the registry IF it is empty AND has no
// in-flight admit (the pending-admit guard). It is the onEmpty callback the hub
// fires when its last connection leaves, and the transient-cleanup path
// commitAndMirror / admit-rollback use. Both conditions are re-checked under
// the registry lock so neither a connection that joined between the hub's "I am
// empty" signal and this call, NOR an admit that has reserved a slot but not
// yet registered its reservation into the hub, is torn out from under. The
// pending check is what closes the transient-hub slot-leak race: a hub an admit
// is about to register into is never removed.
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
