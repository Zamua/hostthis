package http

// Multi-client relay harness: drives the real upgrade + hub +
// snapshot-then-stream paths over real WebSocket connections through an
// httptest.Server. The single-handler checks (one or two clients, one axis
// each) live in ws_test.go; this file holds the N-client, no-leak, cap and
// concurrency scenarios plus the shared wsClient.
//
// Each scenario guarding a load-bearing guarantee names the exact production
// edit that breaks it and the symptom the scenario then reports.

import (
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
)

// parseEnvelope decodes a server control envelope (snapshot / put / delete).
// ok is false for anything else: a relayed peer frame is opaque bytes with no
// "type" field.
func parseEnvelope(data []byte) (durableFrame, bool) {
	var f durableFrame
	if err := json.Unmarshal(data, &f); err != nil || f.Type == "" {
		return durableFrame{}, false
	}
	return f, true
}

func domainSlug(s string) domain.Slug { return domain.Slug(s) }

// wsClient is a test-side relay client: a real WebSocket connection plus a
// reader goroutine demuxing inbound frames into a channel, so a scenario can
// assert both "received X" and "received NOTHING".
type wsClient struct {
	t    *testing.T
	name string
	conn *websocket.Conn

	// frames holds raw inbound frames; the reader goroutine is its only writer.
	frames chan []byte
	// readErr is set once when the reader goroutine exits: the observable
	// signal that this connection was closed / reaped / dropped.
	readErr   chan error
	closeOnce sync.Once
}

// newWSClient dials (slug, id) through ts and starts the reader pump.
func newWSClient(t *testing.T, ctx context.Context, ts *httptest.Server, name, slug, id string) *wsClient {
	t.Helper()
	conn := dial(t, ctx, ts, slug, id)
	c := &wsClient{
		t:       t,
		name:    name,
		conn:    conn,
		frames:  make(chan []byte, 4096),
		readErr: make(chan error, 1),
	}
	go c.readPump(ctx)
	return c
}

// readPump drains the socket into frames until the connection errors.
// coder/websocket auto-pongs server pings from inside Read, so a client with a
// running readPump is a HEALTHY client that answers the heartbeat.
func (c *wsClient) readPump(ctx context.Context) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			select {
			case c.readErr <- err:
			default:
			}
			return
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		select {
		case c.frames <- cp:
		case <-ctx.Done():
			return
		}
	}
}

// send writes a text frame. A write error fails the test: every caller
// expects a live client.
func (c *wsClient) send(ctx context.Context, payload string) {
	c.t.Helper()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		c.t.Fatalf("%s: write %q: %v", c.name, payload, err)
	}
}

// expectSnapshotFrame consumes the first frame and asserts it is the snapshot
// envelope (every fresh join's first frame), returning its raw bytes.
func (c *wsClient) expectSnapshotFrame(ctx context.Context) []byte {
	c.t.Helper()
	data := c.nextFrame(ctx, 3*time.Second)
	if data == nil {
		c.t.Fatalf("%s: no snapshot frame arrived", c.name)
	}
	if !hasType(data, relay.TypeSnapshot) {
		c.t.Fatalf("%s: first frame %q is not a snapshot", c.name, data)
	}
	return data
}

// nextFrame returns the next inbound frame within d, or nil on timeout.
func (c *wsClient) nextFrame(ctx context.Context, d time.Duration) []byte {
	c.t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return nil
	}
}

// drainFrames collects frames until a quiet window of d elapses (the window
// restarts on each frame). Timing out is not a failure: the absence of frames
// is itself part of what the caller asserts.
func (c *wsClient) drainFrames(ctx context.Context, d time.Duration) [][]byte {
	c.t.Helper()
	var out [][]byte
	for {
		f := c.nextFrame(ctx, d)
		if f == nil {
			return out
		}
		out = append(out, f)
	}
}

// expectFrame asserts a frame matching pred arrives within d; fails otherwise.
func (c *wsClient) expectFrame(ctx context.Context, d time.Duration, what string, pred func([]byte) bool) []byte {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		f := c.nextFrame(ctx, time.Until(deadline))
		if f == nil {
			break
		}
		if pred(f) {
			return f
		}
		// Skip non-matching frames (e.g. a snapshot) until the deadline.
	}
	c.t.Fatalf("%s: expected %s within %s, none arrived", c.name, what, d)
	return nil
}

// expectSilence asserts NO frame arrives within d: the isolation / no-echo
// assertion.
func (c *wsClient) expectSilence(ctx context.Context, d time.Duration, why string) {
	c.t.Helper()
	if f := c.nextFrame(ctx, d); f != nil {
		c.t.Fatalf("%s: expected silence (%s) but received %q", c.name, why, f)
	}
}

// expectClosed asserts the reader errored within d: the observable signal
// that the server closed / reaped / dropped this connection.
func (c *wsClient) expectClosed(ctx context.Context, d time.Duration, why string) {
	c.t.Helper()
	select {
	case <-c.readErr:
		return
	case <-time.After(d):
		c.t.Fatalf("%s: expected the connection to be closed (%s), it stayed open", c.name, why)
	case <-ctx.Done():
		c.t.Fatalf("%s: context cancelled before close (%s)", c.name, why)
	}
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() { _ = c.conn.CloseNow() })
}

// hasType reports whether data is a server envelope with the given "type".
// A relayed peer frame is opaque app bytes and never has this shape.
func hasType(data []byte, typ string) bool {
	needle := fmt.Sprintf(`"type":%q`, typ)
	return containsSub(data, needle) || containsSub(data, `"type": `+fmt.Sprintf("%q", typ))
}

func containsSub(haystack []byte, needle string) bool {
	n := []byte(needle)
	if len(n) == 0 || len(n) > len(haystack) {
		return len(n) == 0
	}
	for i := 0; i+len(n) <= len(haystack); i++ {
		if string(haystack[i:i+len(n)]) == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// BROADCAST: N clients in one room. Every client receives every OTHER
// client's messages; no client receives its own echo.
//
// Fails if broadcast's `if id == from { continue }` guard is dropped, which
// echoes a sender its own frame.
// ---------------------------------------------------------------------------

func TestRelayHarness_BroadcastNClientsFanOutNoEcho(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const n = 5
	clients := make([]*wsClient, n)
	for i := range clients {
		clients[i] = newWSClient(t, ctx, ts, fmt.Sprintf("c%d", i), slug, id)
		defer clients[i].close()
	}
	for _, c := range clients {
		c.expectSnapshotFrame(ctx)
	}

	for i, c := range clients {
		c.send(ctx, fmt.Sprintf("from-c%d", i))
	}

	// Each client's inbound peer set must equal {from-cJ : J != i}.
	for i, c := range clients {
		want := make(map[string]bool, n-1)
		for j := range clients {
			if j != i {
				want[fmt.Sprintf("from-c%d", j)] = true
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(want) > 0 && time.Now().Before(deadline) {
			f := c.nextFrame(ctx, time.Until(deadline))
			if f == nil {
				break
			}
			s := string(f)
			if s == fmt.Sprintf("from-c%d", i) {
				t.Fatalf("c%d received its OWN frame %q (echo leak)", i, s)
			}
			delete(want, s)
		}
		if len(want) != 0 {
			t.Fatalf("c%d did not receive all peer frames; missing %v", i, keysOf(want))
		}
		c.expectSilence(ctx, 200*time.Millisecond, "no echo / no dup after all peers received")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// ISOLATION: clients in room A never receive room B's traffic, and clients
// under app X never receive app Y's traffic. The room key is
// (app-slug, room-uuid), so isolation is structural, not a filter.
//
// Fails if the hub is keyed on the room uuid alone: two apps sharing a uuid
// then collide into one hub and frames leak across apps. The
// same-uuid-different-app pair below is what makes the app slug load-bearing.
// ---------------------------------------------------------------------------

func TestRelayHarness_IsolationAcrossRoomsAndApps(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const appA, appB = "appz2345", "bcde2345"
	roomA1 := mkRoom(t, rooms, appA)
	roomA2 := mkRoom(t, rooms, appA) // same app, different room
	roomB1 := mkRoom(t, rooms, appB) // different app, different uuid

	// A room under appB SHARING appA's roomA1 uuid: a different hub, because
	// the key carries the app slug. A slug-less key would collide them.
	mkRoomWithID(t, rooms, appB, roomA1)

	a1 := newWSClient(t, ctx, ts, "a1", appA, roomA1)
	defer a1.close()
	a1b := newWSClient(t, ctx, ts, "a1b", appA, roomA1) // a real peer in A1
	defer a1b.close()
	a2 := newWSClient(t, ctx, ts, "a2", appA, roomA2)
	defer a2.close()
	b1 := newWSClient(t, ctx, ts, "b1", appB, roomB1)
	defer b1.close()
	// Must NOT see appA's traffic despite the shared uuid.
	bShared := newWSClient(t, ctx, ts, "bShared", appB, roomA1)
	defer bShared.close()
	for _, c := range []*wsClient{a1, a1b, a2, b1, bShared} {
		c.expectSnapshotFrame(ctx)
	}

	a1.send(ctx, "secret-of-room-A1")
	a1b.expectFrame(ctx, 3*time.Second, "the in-room peer frame", func(f []byte) bool {
		return string(f) == "secret-of-room-A1"
	})
	a2.expectSilence(ctx, 400*time.Millisecond, "different room of the same app must not see A1's traffic")
	b1.expectSilence(ctx, 400*time.Millisecond, "different app must not see A1's traffic")
	bShared.expectSilence(ctx, 400*time.Millisecond, "different app sharing the uuid must not see A1's traffic")
}

// mkRoomWithID creates a room under slug with an EXPLICIT id, so two apps can
// deliberately share a room-uuid.
func mkRoomWithID(t *testing.T, rooms *service.Rooms, slug, id string) {
	t.Helper()
	now := time.Now().UTC()
	room := domain.Room{
		AppSlug:   domain.Slug(slug),
		ID:        domain.RoomID(id),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := rooms.Repo.CreateRoom(room, "203.0.113.0/24", rooms.PerAppByteCap, now); err != nil {
		t.Fatalf("create room %s/%s: %v", slug, id, err)
	}
}

// ---------------------------------------------------------------------------
// LATE-JOIN: a client that connects after durable state exists receives the
// current room state in its snapshot, consistent with the peers already in
// the room, then live updates with no gap and no dup.
// ---------------------------------------------------------------------------

func TestRelayHarness_LateJoinConsistentWithRoomThenLive(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed two durable cells BEFORE anyone joins.
	httpPut(t, ts, slug, id, "cell/1", []byte(`"busy"`))
	httpPut(t, ts, slug, id, "cell/2", []byte(`"free"`))

	early := newWSClient(t, ctx, ts, "early", slug, id)
	defer early.close()
	earlySnap := early.expectSnapshotFrame(ctx)
	for _, want := range []string{`"cell/1":"busy"`, `"cell/2":"free"`} {
		if !containsSub(earlySnap, want) {
			t.Fatalf("early snapshot missing %s: %q", want, earlySnap)
		}
	}

	// A third cell lands while only `early` is connected.
	httpPut(t, ts, slug, id, "cell/3", []byte(`"taken"`))
	early.expectFrame(ctx, 3*time.Second, "live mirror of cell/3", func(f []byte) bool {
		return hasType(f, relay.TypePut) && containsSub(f, `"key":"cell/3"`)
	})

	// The late joiner's snapshot must reflect ALL THREE cells (the pre-join
	// cell/3 too), so it is consistent with `early`. No gap.
	late := newWSClient(t, ctx, ts, "late", slug, id)
	defer late.close()
	lateSnap := late.expectSnapshotFrame(ctx)
	for _, want := range []string{`"cell/1":"busy"`, `"cell/2":"free"`, `"cell/3":"taken"`} {
		if !containsSub(lateSnap, want) {
			t.Fatalf("late snapshot missing %s (late joiner not caught up): %q", want, lateSnap)
		}
	}

	// A post-join PUT reaches BOTH clients as exactly one live mirror frame.
	httpPut(t, ts, slug, id, "cell/4", []byte(`"new"`))
	for _, c := range []*wsClient{early, late} {
		c.expectFrame(ctx, 3*time.Second, "live mirror of cell/4", func(f []byte) bool {
			return hasType(f, relay.TypePut) && containsSub(f, `"key":"cell/4"`)
		})
		// No dup of that single PUT.
		c.expectSilence(ctx, 200*time.Millisecond, "no dup of the single cell/4 PUT")
	}
}

// ---------------------------------------------------------------------------
// NO-DUP under a concurrent PUT || join race: every change is APPLIED exactly
// once, asserted where the bug lives - a durable HTTP PUT racing a fresh join.
//
// With a register-first join the no-dup rule is the CLIENT's seq discard (drop
// frames with seq <= the snapshot's S), not a server lock: a frame may legally
// arrive whose effect the snapshot already reflects. So occurrences are counted
// the way a compliant client applies them: once if the snapshot reflects the
// mutation, plus once per live frame surviving the discard. Zero is a gap; two
// is a dup, meaning a broken S stamp or a broken discard rule. hostthis is
// payload-opaque and assumes no idempotency, so an applied dup is a real
// defect. Many rounds, because the interleave is timing-dependent.
// ---------------------------------------------------------------------------

func TestRelayHarness_ConcurrentPutAndJoinNoDupNoGap(t *testing.T) {
	runConcurrentMutateAndJoinNoDupNoGap(t, "put",
		// mutate: PUT this round's key.
		func(ts *httptest.Server, slug, id, key string, round int) {
			val := fmt.Sprintf("%q", fmt.Sprintf("v%d", round))
			httpPut(t, ts, slug, id, key, []byte(val))
		},
		// occurrences: once if the snapshot carries key+value, plus once per
		// live TypePut frame for the key surviving the discard rule.
		func(snap []byte, snapSeq uint64, live [][]byte, key string, round int) int {
			val := fmt.Sprintf("%q", fmt.Sprintf("v%d", round))
			count := 0
			if containsSub(snap, fmt.Sprintf("%q:%s", key, val)) {
				count++
			}
			for _, f := range live {
				env, ok := parseEnvelope(f)
				if !ok || env.Type != relay.TypePut || env.Key != key {
					continue
				}
				if env.Seq <= snapSeq {
					continue // the client discard rule: already reflected in the snapshot
				}
				count++
			}
			return count
		},
		nil, // no per-round seed for the PUT variant
	)
}

// TestRelayHarness_ConcurrentDeleteAndJoinNoDupNoGap is the DELETE twin of the
// PUT no-dup gate. Per round it seeds a key, races a DELETE of it against a
// fresh join, and asserts the joiner APPLIES the deletion exactly once: the
// deletion is reflected in the snapshot when the key is ABSENT, and applied
// from the stream when a TypeDelete frame survives the seq > S discard. Zero is
// a gap (the delete was lost); two is a dup (a broken S stamp).
//
// Fails on the same two mutations as the PUT gate: dropping the local mirror
// broadcast, or understating the snapshot's S.
func TestRelayHarness_ConcurrentDeleteAndJoinNoDupNoGap(t *testing.T) {
	runConcurrentMutateAndJoinNoDupNoGap(t, "delete",
		// mutate: DELETE this round's (pre-seeded) key.
		func(ts *httptest.Server, slug, id, key string, round int) {
			httpDelete(t, ts, slug, id, key)
		},
		// occurrences: once if the key is ABSENT from the snapshot, plus once
		// per live TypeDelete frame surviving the discard rule.
		func(snap []byte, snapSeq uint64, live [][]byte, key string, round int) int {
			count := 0
			if !containsSub(snap, fmt.Sprintf(`%q:`, key)) {
				count++ // key absent from the snapshot: the delete is reflected there
			}
			for _, f := range live {
				env, ok := parseEnvelope(f)
				if !ok || env.Type != relay.TypeDelete || env.Key != key {
					continue
				}
				if env.Seq <= snapSeq {
					continue // discard rule: the snapshot already reflects it
				}
				count++
			}
			return count
		},
		// seed: write the key BEFORE the race so there is something to delete,
		// and so it appears in a snapshot read taken before the commit.
		func(ts *httptest.Server, slug, id, key string, round int) {
			httpPut(t, ts, slug, id, key, fmt.Appendf(nil, "%q", fmt.Sprintf("seed%d", round)))
		},
	)
}

// runConcurrentMutateAndJoinNoDupNoGap is the shared body of the PUT and DELETE
// no-dup gates. Per round it optionally seeds the key, races `mutate` (a
// durable HTTP verb: commit, then mirror, with no lock held across the commit)
// against a fresh join, and asserts `occurrences` is EXACTLY ONE.
func runConcurrentMutateAndJoinNoDupNoGap(
	t *testing.T,
	kind string,
	mutate func(ts *httptest.Server, slug, id, key string, round int),
	occurrences func(snap []byte, snapSeq uint64, live [][]byte, key string, round int) int,
	seed func(ts *httptest.Server, slug, id, key string, round int),
) {
	t.Helper()
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// An established client keeps the hub alive across rounds, which is the
	// configuration the dup needs: the mirror has a hub to fan out to and the
	// joiner has one to register into.
	keepalive := newWSClient(t, ctx, ts, "keepalive", slug, id)
	defer keepalive.close()
	keepalive.expectSnapshotFrame(ctx)

	const rounds = 40
	for round := range rounds {
		key := fmt.Sprintf("cell/%d", round)

		// Seed BEFORE the race when the variant needs a starting value.
		if seed != nil {
			seed(ts, slug, id, key, round)
		}

		// Race the durable mutation of this round's key against a fresh join.
		var wg sync.WaitGroup
		wg.Add(2)

		joinerCh := make(chan *wsClient, 1)
		go func() {
			defer wg.Done()
			mutate(ts, slug, id, key, round)
		}()
		go func() {
			defer wg.Done()
			joinerCh <- newWSClient(t, ctx, ts, fmt.Sprintf("joiner-%s-%d", kind, round), slug, id)
		}()
		wg.Wait()
		joiner := <-joinerCh

		// The snapshot may or may not reflect this round's mutation, depending
		// on whether the commit landed before its read. Its seq S is the
		// discard fence the occurrence count applies.
		snap := joiner.expectSnapshotFrame(ctx)
		snapEnv, ok := parseEnvelope(snap)
		if !ok || snapEnv.Type != relay.TypeSnapshot {
			t.Fatalf("round %d (%s): first frame is not a snapshot envelope: %q", round, kind, snap)
		}
		live := joiner.drainFrames(ctx, 250*time.Millisecond)

		count := occurrences(snap, snapEnv.Seq, live, key, round)
		switch {
		case count == 0:
			t.Fatalf("round %d (%s): key %q appeared 0x (GAP) across snapshot+stream - the change was neither snapshotted nor mirrored live", round, kind, key)
		case count > 1:
			t.Fatalf("round %d (%s): key %q appeared %dx (DUP) - applied from the stream despite already being reflected in the snapshot (broken S stamp or discard fence)", round, kind, key, count)
		}

		joiner.close()
	}
}

// ---------------------------------------------------------------------------
// DISCONNECT / RECONNECT: a client drops mid-stream and reconnects; it
// re-syncs from the KV snapshot, rejoins the live stream, and the hub reaps
// the dead connection so the count returns to exactly the survivors.
//
// Fails if Serve's teardown drops its `reg.release(key, id)`: a dropped
// connection then leaks its hub slot and the room never returns to 1 conn.
// ---------------------------------------------------------------------------

func TestRelayHarness_DisconnectReconnectResyncsAndReapsNoLeak(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A survivor stays connected so the hub is never torn down underneath the
	// assertion, which is that the DROPPED client's slot alone is reaped.
	survivor := newWSClient(t, ctx, ts, "survivor", slug, id)
	defer survivor.close()
	survivor.expectSnapshotFrame(ctx)

	dropper := newWSClient(t, ctx, ts, "dropper", slug, id)
	dropper.expectSnapshotFrame(ctx)
	waitForRoomConns(t, rl, slug, 2, 3*time.Second)

	// A hard close, like a yanked link: the hub must reap the slot.
	dropper.close()
	waitForRoomConns(t, rl, slug, 1, 5*time.Second)

	// While the client is away, durable state changes.
	httpPut(t, ts, slug, id, "k", []byte(`"v"`))

	// A fresh join re-syncs the FULL current state from the KV snapshot: there
	// is no incremental "catch me up from N" to get wrong.
	again := newWSClient(t, ctx, ts, "again", slug, id)
	defer again.close()
	againSnap := again.expectSnapshotFrame(ctx)
	if !containsSub(againSnap, `"k":"v"`) {
		t.Fatalf("reconnect snapshot missing the away-time write: %q", againSnap)
	}
	waitForRoomConns(t, rl, slug, 2, 3*time.Second)

	// The reconnected client rejoins the LIVE stream.
	httpPut(t, ts, slug, id, "k2", []byte(`"v2"`))
	again.expectFrame(ctx, 3*time.Second, "live mirror after reconnect", func(f []byte) bool {
		return hasType(f, relay.TypePut) && containsSub(f, `"key":"k2"`)
	})

	// When BOTH clients leave the hub is torn down: no dangling empty hub.
	survivor.close()
	again.close()
	waitForRooms(t, rl, 0, 5*time.Second)
}

// waitForRoomConns polls until the app's live connection count equals want,
// failing after d. Teardown is asynchronous, so the reap can only be asserted
// by converging, never by a single read.
func waitForRoomConns(t *testing.T, rl *relay.Relay, slug string, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := rl.Registry().AppConns(domainSlug(slug)); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("app %q live conns = %d after %s, want %d (leak or missed join?)", slug, rl.Registry().AppConns(domainSlug(slug)), d, want)
}

func waitForRooms(t *testing.T, rl *relay.Relay, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if rl.Registry().Rooms() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("registry rooms = %d after %s, want %d (empty hub leaked?)", rl.Registry().Rooms(), d, want)
}

// ---------------------------------------------------------------------------
// HEARTBEAT (positive): a HEALTHY client that keeps reading (and so auto-pongs)
// stays connected across many heartbeat intervals. The negative half (a
// vanished client reaped within the timeout) is ws_test.go's
// TestRelay_HeartbeatReapsDeadConnection; without this half, a too-aggressive
// reap would pass by reaping everyone.
// ---------------------------------------------------------------------------

func TestRelayHarness_HealthyClientSurvivesIdleAcrossHeartbeats(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	// The load-bearing knob is the ping INTERVAL being short relative to the
	// idle window, so the window spans MANY heartbeats. The pong TIMEOUT is
	// only the reap budget and stays generous: the pong round-trip is gated by
	// goroutine scheduling, so a tight budget reaps a healthy client under
	// load. Tightening it would not strengthen this test either, since the reap
	// is pinned by the negative half in ws_test.go.
	const hbInterval = 50 * time.Millisecond
	rl.SetHeartbeat(hbInterval, 2*time.Second)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The client sends NOTHING: liveness rides on the readPump's auto-pong.
	c := newWSClient(t, ctx, ts, "healthy", slug, id)
	defer c.close()
	c.expectSnapshotFrame(ctx)

	// idleWindow / hbInterval is ~20, so survival is asserted across many
	// heartbeats, not one.
	const idleWindow = 1 * time.Second
	deadline := time.Now().Add(idleWindow)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.readErr:
			t.Fatalf("healthy idle client was reaped: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	if got := rl.Registry().AppConns(domainSlug(slug)); got != 1 {
		t.Fatalf("healthy client conns = %d after idle window, want 1 (it was wrongly reaped)", got)
	}
	// And it still receives live traffic.
	peer := newWSClient(t, ctx, ts, "peer", slug, id)
	defer peer.close()
	peer.expectSnapshotFrame(ctx)
	peer.send(ctx, "still-alive")
	c.expectFrame(ctx, 2*time.Second, "live frame after the idle window", func(f []byte) bool {
		return string(f) == "still-alive"
	})
}

// ---------------------------------------------------------------------------
// BACKPRESSURE: a slow / stuck client must not stall the broadcast. Over a
// real socket the kernel's TCP buffers absorb a non-reading client's backlog
// unreliably, so the drop-the-laggard half is proven deterministically in the
// relay package (TestWSConn_SendBufferBoundsAndDropSignal,
// TestHub_SlowClientDroppedWithoutBlockingRoom); this scenario pins the
// OBSERVABLE property: every fast client keeps flowing past a stuck peer.
//
// Fails if wsConn.Send's non-blocking buffered send becomes a blocking one: a
// stuck client then head-of-line-blocks the room instead of being dropped.
// ---------------------------------------------------------------------------

func TestRelayHarness_StuckClientDoesNotStallMultipleFastPeers(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxMsgsPerSec = 0         // isolate from the rate limit
	lim.MaxMessageBytes = 1 << 20 // allow the larger frames this test sends
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const fastN = 3
	fast := make([]*wsClient, fastN)
	for i := range fast {
		fast[i] = newWSClient(t, ctx, ts, fmt.Sprintf("fast%d", i), slug, id)
		defer fast[i].close()
		fast[i].expectSnapshotFrame(ctx)
	}

	// Dialled raw, not as a wsClient, whose readPump would drain it.
	stuck := dial(t, ctx, ts, slug, id)
	defer stuck.CloseNow() //nolint:errcheck
	if _, _, err := stuck.Read(ctx); err != nil {
		t.Fatalf("stuck client snapshot read: %v", err)
	}
	// stuck never reads again.

	sender := newWSClient(t, ctx, ts, "sender", slug, id)
	defer sender.close()
	sender.expectSnapshotFrame(ctx)

	var got [fastN]int32
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range fast {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-fast[idx].frames:
					atomic.AddInt32(&got[idx], 1)
				case <-ctx.Done():
					return
				}
			}
		}(i)
	}

	// Despite the stuck peer, EVERY fast client must keep accumulating: the
	// broadcast is wait-free per client.
	payload := make([]byte, 64<<10)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := sender.conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for i := range fast {
			if atomic.LoadInt32(&got[i]) < 30 {
				all = false
			}
		}
		if all {
			close(stop)
			wg.Wait()
			return // every fast client kept flowing despite the stuck peer.
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	for i := range fast {
		if atomic.LoadInt32(&got[i]) < 30 {
			t.Fatalf("fast%d received only %d frames with a stuck peer present: room stalled on the laggard", i, atomic.LoadInt32(&got[i]))
		}
	}
}

// ---------------------------------------------------------------------------
// LIMITS: the PER-APP and TOTAL-ROOMS caps enforced at the upgrade over the
// real HTTP path (the single-axis per-room / frame-size / rate-limit checks
// are in ws_test.go), plus the property that a rejection leaves the
// already-connected clients working.
//
// Fails if admit's per-app cap check is removed: an over-cap upgrade then gets
// a 101 instead of a 429.
// ---------------------------------------------------------------------------

func TestRelayHarness_PerAppConnectionCapRefusesAndRoomStaysHealthy(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxConnsPerApp = 2
	lim.MaxConnsPerRoom = 0 // isolate the per-app cap
	lim.MaxRooms = 0
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	r1 := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Two connections in the SAME room of the same app fill the per-app cap.
	c1 := newWSClient(t, ctx, ts, "c1", slug, r1)
	defer c1.close()
	c2 := newWSClient(t, ctx, ts, "c2", slug, r1)
	defer c2.close()
	c1.expectSnapshotFrame(ctx)
	c2.expectSnapshotFrame(ctx)

	// A third connection under the same app is refused 429 BEFORE the 101.
	assertDialStatus(t, ctx, ts, slug, r1, nethttp.StatusTooManyRequests, "per-app cap")

	// The refusal must not collateral-damage the hub.
	c1.send(ctx, "after-refusal")
	c2.expectFrame(ctx, 2*time.Second, "in-room relay survives the per-app refusal", func(f []byte) bool {
		return string(f) == "after-refusal"
	})
	// The durable path is unaffected by the connection cap.
	httpPut(t, ts, slug, r1, "k", []byte(`"v"`))
	for _, c := range []*wsClient{c1, c2} {
		c.expectFrame(ctx, 2*time.Second, "live mirror survives the per-app refusal", func(f []byte) bool {
			return hasType(f, relay.TypePut) && containsSub(f, `"key":"k"`)
		})
	}
}

func TestRelayHarness_TotalRoomsCapRefusesNewHubAllowsJoin(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxRooms = 1
	lim.MaxConnsPerRoom = 0
	lim.MaxConnsPerApp = 0
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	r1 := mkRoom(t, rooms, slug)
	r2 := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First room creates the one allowed hub.
	c1 := newWSClient(t, ctx, ts, "c1", slug, r1)
	defer c1.close()
	c1.expectSnapshotFrame(ctx)

	// A second DISTINCT room would create a new hub past the cap: 503.
	assertDialStatus(t, ctx, ts, slug, r2, nethttp.StatusServiceUnavailable, "total-rooms cap")

	// A JOIN to the already-live room still succeeds (no new hub).
	c1b := newWSClient(t, ctx, ts, "c1b", slug, r1)
	defer c1b.close()
	c1b.expectSnapshotFrame(ctx)
	c1.send(ctx, "in-room")
	c1b.expectFrame(ctx, 2*time.Second, "in-room relay survives the cap", func(f []byte) bool {
		return string(f) == "in-room"
	})
}

// assertDialStatus asserts the upgrade is refused with want BEFORE any 101:
// the cap is enforced at the HTTP layer, not after a socket opens.
func assertDialStatus(t *testing.T, ctx context.Context, ts *httptest.Server, slug, id string, want int, why string) {
	t.Helper()
	_, resp, err := websocket.Dial(ctx, wsURL(ts, slug, id), &websocket.DialOptions{Host: slug + "." + wsTestApex})
	if err == nil {
		t.Fatalf("dial past the %s succeeded, want refusal", why)
	}
	if resp == nil || resp.StatusCode != want {
		t.Fatalf("%s refusal status = %v, want %d", why, resp, want)
	}
}

// ---------------------------------------------------------------------------
// CONCURRENCY / RACE: many clients connecting, sending, and disconnecting
// CONCURRENTLY through the FULL socket stack, so the goroutine choreography
// (reader, writer, heartbeat, teardown) races too, not just the hub maps the
// relay package unit-tests. Under -race this catches a torn hub map or per-app
// counter; the terminal assertion is the no-leak invariant, the registry
// returning to zero rooms and zero app conns.
// ---------------------------------------------------------------------------

func TestRelayHarness_ConcurrentConnectSendDisconnectRace(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// A handful of rooms across two apps so the per-app counter and the hub
	// map are both contended.
	type roomRef struct{ slug, id string }
	rooms2 := []roomRef{}
	for _, slug := range []string{"appz2345", "bcde2345"} {
		for range 3 {
			rooms2 = append(rooms2, roomRef{slug: slug, id: mkRoom(t, rooms, slug)})
		}
	}

	const workers = 40
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rr := rooms2[w%len(rooms2)]
			conn := dial(t, ctx, ts, rr.slug, rr.id)
			// Drain the snapshot.
			rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
			_, _, _ = conn.Read(rctx)
			rcancel()
			for i := range 5 {
				if err := conn.Write(ctx, websocket.MessageText, fmt.Appendf(nil, "w%d-%d", w, i)); err != nil {
					break
				}
			}
			// Drain whatever peer frames arrived, then disconnect.
			drain, dcancel := context.WithTimeout(ctx, 100*time.Millisecond)
			for {
				if _, _, err := conn.Read(drain); err != nil {
					break
				}
			}
			dcancel()
			_ = conn.CloseNow()
		}(w)
	}
	wg.Wait()

	// Both axes are polled rather than read once: the hub drop (Rooms) and the
	// per-app decrement (AppConns) land in separate asynchronous steps, so a
	// single read of one right after the other settles sees an intermediate.
	for _, slug := range []string{"appz2345", "bcde2345"} {
		waitForAppConns(t, rl, slug, 0, 10*time.Second)
	}
	waitForRooms(t, rl, 0, 10*time.Second)
}

// ---------------------------------------------------------------------------
// DRAIN HINT: on shutdown begin, the relay broadcasts {"type":"reconnect"} to
// every live connection BEFORE any close. This drives the exact sequence main
// runs (AnnounceDrain, a serve-through grace, then CloseAll) over real sockets.
// The hint carries no seq: it is not a room mutation.
//
// Fails if the AnnounceDrain broadcast is deleted or reordered after CloseAll:
// clients are then hard-cut with no re-home window.
// ---------------------------------------------------------------------------

func TestRelayHarness_DrainHintBeforeClose(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c1 := newWSClient(t, ctx, ts, "c1", slug, id)
	defer c1.close()
	c2 := newWSClient(t, ctx, ts, "c2", slug, id)
	defer c2.close()
	c1.expectSnapshotFrame(ctx)
	c2.expectSnapshotFrame(ctx)

	// The hint fires while the relay is still serving.
	rl.AnnounceDrain()
	for _, c := range []*wsClient{c1, c2} {
		c.expectFrame(ctx, 3*time.Second, "the reconnect drain hint", func(f []byte) bool {
			return hasType(f, relay.TypeReconnect)
		})
	}

	// A fresh join still succeeds after the hint: the make-before-break window
	// a hint-acting client re-homes in.
	c3 := newWSClient(t, ctx, ts, "c3", slug, id)
	defer c3.close()
	c3.expectSnapshotFrame(ctx)

	// The final close, as main does after the grace window: every connection
	// observes it AFTER having received the hint.
	rl.Registry().CloseAll()
	c1.expectClosed(ctx, 5*time.Second, "shutdown close after the drain hint")
	c2.expectClosed(ctx, 5*time.Second, "shutdown close after the drain hint")
}

// waitForAppConns is waitForRoomConns with the failure message of the terminal
// no-leak convergence check.
func waitForAppConns(t *testing.T, rl *relay.Relay, slug string, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := rl.Registry().AppConns(domainSlug(slug)); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("app %q live conns = %d after %s, want %d (leaked conns after the churn storm?)", slug, rl.Registry().AppConns(domainSlug(slug)), d, want)
}
