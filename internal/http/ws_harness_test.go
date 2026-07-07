package http

// This file is the MULTI-CLIENT relay harness: the integration gate the
// spec names in "Real-time room relay (WebSocket) -> Testing: the
// multi-client harness is the gate." It drives the REAL upgrade + hub +
// snapshot-then-stream paths by dialing real WebSocket connections through
// an httptest.Server, so the finicky connection lifecycle is exercised end
// to end, not against a fake socket.
//
// The single-handler-focused checks (one or two clients, one axis each)
// live in ws_test.go. This file adds the N-client scenarios and the
// no-leak / cap / concurrency coverage the spec's gate calls for, plus a
// reusable client abstraction (wsClient) several scenarios share.
//
// Where a guarantee is load-bearing (isolation, no-leak on disconnect,
// backpressure-does-not-stall, the caps), each scenario carries a WEAKEN
// DEMO comment naming the EXACT production edit that breaks the guarantee
// and the symptom the scenario then reports. Each was run by applying that
// edit to the named line, observing the scenario go RED, and restoring -
// so the assertion is proven to actually fail-closed, not vacuously pass.
// The demos are described, not committed as broken code, so the tree stays
// green; the edit + symptom in each comment is enough to reproduce the red.

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

// parseEnvelope decodes a server control envelope (snapshot / put /
// delete). ok is false for anything else - an opaque relayed peer frame
// has no "type" field, so it never parses as an envelope.
func parseEnvelope(data []byte) (durableFrame, bool) {
	var f durableFrame
	if err := json.Unmarshal(data, &f); err != nil || f.Type == "" {
		return durableFrame{}, false
	}
	return f, true
}

// domainSlug is a tiny local converter so the harness's registry
// assertions read cleanly (the registry's AppConns takes a domain.Slug).
func domainSlug(s string) domain.Slug { return domain.Slug(s) }

// wsClient is a small test-side relay client: it owns a real WebSocket
// connection plus a background reader goroutine that demuxes inbound frames
// into a channel, so a scenario can assert "this client received X" and
// "this client received NOTHING" without each test re-implementing the
// read pump. It is the harness's stand-in for an app's browser client.
type wsClient struct {
	t    *testing.T
	name string
	conn *websocket.Conn

	// frames carries every inbound frame's raw bytes (snapshot, live mirror,
	// or relayed peer frame). The reader goroutine is the only writer.
	frames chan []byte
	// readErr is set once when the reader goroutine exits (clean close, reap,
	// drop). A scenario waits on it to assert "this connection was closed."
	readErr   chan error
	closeOnce sync.Once
}

// newWSClient dials (slug, id) through ts and starts the reader pump. It
// raises the client-side read limit well past the server's inbound cap so
// the harness accepts the larger server frames the backpressure scenario
// emits (a test-harness concern, separate from the server's
// MaxMessageBytes on INBOUND frames).
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

// readPump drains the socket into the frames channel until the connection
// errors (the reader is how coder/websocket also processes server pings ->
// auto-pong, so a client with a running readPump is a HEALTHY client that
// answers the heartbeat). On any read error it records readErr once and
// returns, so a scenario can detect a reap / drop / clean close.
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

// send writes a text frame from this client. A write error fails the test:
// in every scenario that calls send the client is expected to be live.
func (c *wsClient) send(ctx context.Context, payload string) {
	c.t.Helper()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		c.t.Fatalf("%s: write %q: %v", c.name, payload, err)
	}
}

// expectSnapshotFrame consumes the first frame and asserts it is the
// snapshot envelope (every fresh join's first frame). It returns the raw
// snapshot bytes so a scenario can assert the late-join state.
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

// drainFrames collects every inbound frame that arrives within a quiet window
// of d (the window restarts on each frame), returning them all. Used by the
// no-dup race to gather all live mirror frames (put OR delete) the joiner
// received for a round so a per-key occurrence count can be asserted. It does
// NOT fail on timeout - the absence of frames is itself an assertion the
// caller makes.
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
		// A non-matching frame (e.g. a snapshot that slipped through) is
		// skipped; keep reading until the deadline.
	}
	c.t.Fatalf("%s: expected %s within %s, none arrived", c.name, what, d)
	return nil
}

// expectSilence asserts NO frame arrives within d (the isolation / no-echo
// assertion). A frame arriving fails the test.
func (c *wsClient) expectSilence(ctx context.Context, d time.Duration, why string) {
	c.t.Helper()
	if f := c.nextFrame(ctx, d); f != nil {
		c.t.Fatalf("%s: expected silence (%s) but received %q", c.name, why, f)
	}
}

// expectClosed asserts the connection's reader errored within d - the
// observable signal that the server closed/reaped/dropped this connection.
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
// A relayed peer frame is opaque app bytes and never has this shape, so it
// distinguishes a snapshot / put / delete envelope from a peer message.
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
// client's messages; no client receives its own echo. The 2-client version
// is in ws_test.go; this drives N=5 with each client both sending and
// receiving, so the fan-out is asserted across the whole set.
//
// WEAKEN DEMO: drop the `if id == from { continue }` guard in
// relay/hub.go's broadcast, so the fan-out includes the sender. Demonstrated
// RED: "c0 received its OWN frame (echo leak)". Restoring the guard is green.
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

	// Each client sends a uniquely-identifiable frame.
	for i, c := range clients {
		c.send(ctx, fmt.Sprintf("from-c%d", i))
	}

	// Every client must receive every OTHER client's frame exactly, and must
	// NOT receive its own. We collect each client's inbound peer frames and
	// assert the set equals {from-cJ : J != i}.
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
		// And nothing else (no echo, no dup): a short read is silent.
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
// WEAKEN DEMO: key the hub on the room-uuid ALONE (drop the app slug from
// the RoomKey the registry maps on), so two different apps' rooms that
// share a uuid collide into one hub. The cross-app frame then leaks and the
// expectSilence on the OTHER app fails. The same-uuid-different-app pair
// below is what makes the app-slug in the key load-bearing: with distinct
// uuids the room-vs-room half is structural, but two apps colliding on one
// uuid is exactly the leak a slug-less key would open. Demonstrated RED by
// normalizing key.App = "" in the registry's hub lookups: "bShared:
// expected silence ... but received secret-of-room-A1". Restoring the app
// slug in the key is green.
// ---------------------------------------------------------------------------

func TestRelayHarness_IsolationAcrossRoomsAndApps(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const appA, appB = "appz2345", "bcde2345"
	roomA1 := mkRoom(t, rooms, appA)
	roomA2 := mkRoom(t, rooms, appA) // same app, different room
	roomB1 := mkRoom(t, rooms, appB) // different app, different uuid

	// A room under appB that SHARES appA's roomA1 uuid. The hub key is
	// (app-slug, room-uuid), so these are DIFFERENT hubs; a slug-less key
	// would collide them. This is the cross-app collision the weaken opens.
	mkRoomWithID(t, rooms, appB, roomA1)

	a1 := newWSClient(t, ctx, ts, "a1", appA, roomA1)
	defer a1.close()
	a1b := newWSClient(t, ctx, ts, "a1b", appA, roomA1) // a real peer in A1
	defer a1b.close()
	a2 := newWSClient(t, ctx, ts, "a2", appA, roomA2)
	defer a2.close()
	b1 := newWSClient(t, ctx, ts, "b1", appB, roomB1)
	defer b1.close()
	// A client in appB's room that shares appA's uuid: it must NOT see appA's
	// traffic despite the shared uuid (the app slug keeps the hubs disjoint).
	bShared := newWSClient(t, ctx, ts, "bShared", appB, roomA1)
	defer bShared.close()
	for _, c := range []*wsClient{a1, a1b, a2, b1, bShared} {
		c.expectSnapshotFrame(ctx)
	}

	// a1 broadcasts. Its real peer a1b must receive it; a2 (same app, other
	// room), b1 (other app), and bShared (other app, SAME uuid) get NOTHING.
	a1.send(ctx, "secret-of-room-A1")
	a1b.expectFrame(ctx, 3*time.Second, "the in-room peer frame", func(f []byte) bool {
		return string(f) == "secret-of-room-A1"
	})
	a2.expectSilence(ctx, 400*time.Millisecond, "different room of the same app must not see A1's traffic")
	b1.expectSilence(ctx, 400*time.Millisecond, "different app must not see A1's traffic")
	bShared.expectSilence(ctx, 400*time.Millisecond, "different app sharing the uuid must not see A1's traffic")
}

// mkRoomWithID creates a room under slug with an EXPLICIT id (so two apps
// can deliberately share a room-uuid) by going straight to the repo, which
// takes a full domain.Room. Used to make the app-slug component of the hub
// key load-bearing in the isolation scenario.
func mkRoomWithID(t *testing.T, rooms *service.Rooms, slug, id string) {
	t.Helper()
	now := time.Now().UTC()
	room := domain.Room{
		AppSlug:   domain.Slug(slug),
		ID:        domain.RoomID(id),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RoomRetentionWindow),
	}
	if err := rooms.Repo.CreateRoom(room, "203.0.113.0/24", rooms.PerAppByteCap, now); err != nil {
		t.Fatalf("create room %s/%s: %v", slug, id, err)
	}
}

// ---------------------------------------------------------------------------
// LATE-JOIN: a client that connects after durable state exists receives the
// current room state in its snapshot (from the KV), consistent with the
// peers already in the room, then live updates with no gap and no dup.
//
// This extends ws_test.go's single-late-joiner check to the MULTI-CLIENT
// shape: an established client is already in the room (and sees the live
// stream), the late joiner's snapshot agrees with what the established
// client has, and a post-join PUT reaches BOTH as exactly one live frame.
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

	// Established client joins and sees both cells in its snapshot.
	early := newWSClient(t, ctx, ts, "early", slug, id)
	defer early.close()
	earlySnap := early.expectSnapshotFrame(ctx)
	for _, want := range []string{`"cell/1":"busy"`, `"cell/2":"free"`} {
		if !containsSub(earlySnap, want) {
			t.Fatalf("early snapshot missing %s: %q", want, earlySnap)
		}
	}

	// A third cell lands while only `early` is connected: early sees it live.
	httpPut(t, ts, slug, id, "cell/3", []byte(`"taken"`))
	early.expectFrame(ctx, 3*time.Second, "live mirror of cell/3", func(f []byte) bool {
		return hasType(f, relay.TypePut) && containsSub(f, `"key":"cell/3"`)
	})

	// LATE joiner connects: its snapshot must reflect ALL THREE cells (the
	// pre-join cell/3 too), so it is consistent with `early`. No gap.
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
// NO-DUP under a concurrent PUT || join race. This is the gate for the
// spec's "every change is APPLIED exactly once" guarantee, asserted at the
// level the bug actually lives: an HTTP durable PUT racing a fresh
// WebSocket join. Per the spec's register-first join, the no-dup rule is
// the CLIENT's seq discard (drop any frame with seq <= the snapshot's S -
// SPEC "The client splice contract"), not a server lock: a frame CAN
// legally arrive on the wire whose effect the snapshot already reflects,
// and the discard rule is what applies it zero more times. So the gate
// counts occurrences the way a spec-compliant client does: once if the
// snapshot reflects the mutation, plus once per live frame that SURVIVES
// the discard rule (seq > S). hostthis is payload-opaque and makes no
// idempotency assumption, so an applied dup is a real defect.
//
// The test fires, per round, a PUT of a round-unique key concurrently with a
// fresh join, then drains the joiner's snapshot + live frames and asserts the
// mutation is APPLIED exactly once. Zero is a gap (the write was lost); two
// is a dup (the snapshot reflected it AND a frame with seq > S also carried
// it - a broken S stamp or a broken discard rule). Many rounds are run
// because the interleave is timing-dependent.
//
// WEAKEN DEMO (both run, observed, restored; the reds land on the sibling
// gates because this gate's failing interleave is timing-sampled):
//   - dropped mirror: in relay/registry.go's commitAndMirror, delete the
//     local `hub.broadcast(0, mirror)`. Deterministic RED on
//     TestRelay_LateJoinSnapshotThenStreamNoGapNoDup and
//     TestRelayHarness_LateJoinConsistentWithRoomThenLive (the post-join
//     PUT's live mirror never arrives).
//   - mis-stamped S: in relay/codec.go's encodeSnapshot, stamp
//     `Seq: kv.Seq - 1`. Deterministic RED on
//     TestMultiPod_BroadcastReachesAllPods ("fresh room snapshots at seq
//     18446744073709551615..., want 0/0") - the exact-S stamp the discard
//     rule fences on is load-bearing everywhere the splice client runs.
// ---------------------------------------------------------------------------

func TestRelayHarness_ConcurrentPutAndJoinNoDupNoGap(t *testing.T) {
	runConcurrentMutateAndJoinNoDupNoGap(t, "put",
		// mutate: PUT this round's key.
		func(ts *httptest.Server, slug, id, key string, round int) {
			val := fmt.Sprintf("%q", fmt.Sprintf("v%d", round))
			httpPut(t, ts, slug, id, key, []byte(val))
		},
		// occurrences: the PUT is APPLIED once if its key+value is in the
		// snapshot, plus once per live TypePut frame for the key that survives
		// the client discard rule (seq > the snapshot's S). Exactly one total:
		// 0 is a gap (the write was lost), 2 a dup (snapshotted AND applied
		// from the stream).
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
// PUT no-dup gate. The durable DELETE path (deleteRoomValue ->
// CommitAndMirror with EncodeDelete) rides the identical commit-then-mirror
// path + client discard rule the PUT does, so it is structurally covered -
// but this pins it directly: per round it SEEDS a key, then races a DELETE
// of that key against a fresh join, and asserts the joiner APPLIES the
// deletion exactly once: the deletion is "reflected in the snapshot" when
// the key is ABSENT (the commit landed before the snapshot read; the
// delete frame's seq is then <= S and the discard rule drops it), and
// "applied from the stream" when a TypeDelete frame for the key SURVIVES
// the discard rule (seq > S). Zero is a gap (the key is still present AND
// no surviving delete frame: the delete was lost); two is a dup (the key
// is absent in the snapshot AND a frame with seq > S also carried the
// delete: a broken S stamp). A dup matters because hostthis is
// payload-opaque and an app may treat a delete as a state transition.
//
// WEAKEN DEMO: same two edits as the PUT gate (drop the local mirror
// broadcast in registry.commitAndMirror; understate the snapshot's S in
// codec.encodeSnapshot), observed RED on the deterministic sibling gates
// named there.
func TestRelayHarness_ConcurrentDeleteAndJoinNoDupNoGap(t *testing.T) {
	runConcurrentMutateAndJoinNoDupNoGap(t, "delete",
		// mutate: DELETE this round's (pre-seeded) key.
		func(ts *httptest.Server, slug, id, key string, round int) {
			httpDelete(t, ts, slug, id, key)
		},
		// occurrences: the deletion is applied once when the key is ABSENT
		// from the snapshot, plus once per live TypeDelete frame that survives
		// the discard rule (seq > S). Exactly one total.
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
		// seed: write the key BEFORE the round's race so there is something to
		// delete and so it would appear in a snapshot read before the commit.
		func(ts *httptest.Server, slug, id, key string, round int) {
			httpPut(t, ts, slug, id, key, fmt.Appendf(nil, "%q", fmt.Sprintf("seed%d", round)))
		},
	)
}

// runConcurrentMutateAndJoinNoDupNoGap is the shared body of the PUT and DELETE
// no-dup gates. Per round it (optionally) seeds the round's key, then races
// `mutate` (a durable PUT or DELETE through the HTTP verb: the server
// commits, then mirrors, with no lock held across the commit) against a
// fresh WebSocket join, and asserts `occurrences` (the count of the
// mutation as a spec-compliant client APPLIES it: snapshot state + live
// frames surviving the seq <= S discard rule) is EXACTLY ONE - never 0 (a
// gap: the change was neither snapshotted nor streamed) and never >1 (an
// applied dup). Many rounds run because the failing interleaves are
// timing-dependent.
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

	// An established client keeps the hub alive across all rounds so a fresh
	// joiner each round is racing against a live hub (the broadcast set is
	// non-empty), which is the configuration the dup needs: the mirror has a
	// hub to fan out to, and the joiner can register into it.
	keepalive := newWSClient(t, ctx, ts, "keepalive", slug, id)
	defer keepalive.close()
	keepalive.expectSnapshotFrame(ctx)

	const rounds = 40
	for round := range rounds {
		key := fmt.Sprintf("cell/%d", round)

		// Seed the round's key BEFORE the race, if the variant needs a starting
		// value (the DELETE variant deletes a pre-existing key; the keepalive
		// client's frame for the seed is drained on the next round's quiet wait).
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

		// The joiner's first frame is its snapshot (may or may not reflect this
		// round's mutation, depending on whether the commit landed before the
		// join's snapshot read). Its exact seq S is the discard fence the
		// occurrence count applies. Then drain every live frame for a short
		// window.
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
// re-syncs from the KV snapshot and rejoins the live stream correctly, AND
// the hub reaps the dead connection so there is no leak (the room's
// connection count returns to exactly the survivors).
//
// WEAKEN DEMO: drop the `rl.reg.release(key, id)` in relay/relay.go's Serve
// teardown defer, so a dropped connection leaks its hub slot. Demonstrated
// RED: after the drop the room never returns to 1 conn ("live conns = 2 ...
// want 1"). Restoring the release is green.
// ---------------------------------------------------------------------------

func TestRelayHarness_DisconnectReconnectResyncsAndReapsNoLeak(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A survivor stays connected for the whole test so the hub is never torn
	// down out from under us - we are asserting the DROPPED client's slot is
	// reaped, leaving exactly the survivor.
	survivor := newWSClient(t, ctx, ts, "survivor", slug, id)
	defer survivor.close()
	survivor.expectSnapshotFrame(ctx)

	dropper := newWSClient(t, ctx, ts, "dropper", slug, id)
	dropper.expectSnapshotFrame(ctx)
	waitForRoomConns(t, rl, slug, 2, 3*time.Second)

	// The dropper vanishes (a hard close, like a yanked link). The hub must
	// reap its slot: the per-app count returns to 1 (just the survivor).
	dropper.close()
	waitForRoomConns(t, rl, slug, 1, 5*time.Second)

	// While the client is away, durable state changes.
	httpPut(t, ts, slug, id, "k", []byte(`"v"`))

	// Reconnect: a fresh join re-syncs the FULL current state from the KV
	// snapshot (no incremental "catch me up from N" to get wrong).
	again := newWSClient(t, ctx, ts, "again", slug, id)
	defer again.close()
	againSnap := again.expectSnapshotFrame(ctx)
	if !containsSub(againSnap, `"k":"v"`) {
		t.Fatalf("reconnect snapshot missing the away-time write: %q", againSnap)
	}
	waitForRoomConns(t, rl, slug, 2, 3*time.Second)

	// The reconnected client rejoins the LIVE stream: a fresh PUT reaches it.
	httpPut(t, ts, slug, id, "k2", []byte(`"v2"`))
	again.expectFrame(ctx, 3*time.Second, "live mirror after reconnect", func(f []byte) bool {
		return hasType(f, relay.TypePut) && containsSub(f, `"key":"k2"`)
	})

	// Final no-leak check: when BOTH clients leave, the hub is torn down (the
	// registry returns to zero rooms, no dangling empty hub).
	survivor.close()
	again.close()
	waitForRooms(t, rl, 0, 5*time.Second)
}

// waitForRoomConns polls the registry until the app's live connection count
// equals want, or fails after d. Used to assert the reap (no-leak) property
// against the REAL socket path, where teardown is asynchronous.
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
// HEARTBEAT (positive): a HEALTHY client that keeps reading (and so
// auto-pongs the server's pings) stays connected across multiple
// heartbeat intervals - the heartbeat must not reap a live client. The
// negative half (a vanished client reaped within the timeout) is in
// ws_test.go (TestRelay_HeartbeatReapsDeadConnection); here we pin the
// complementary property so a too-aggressive reap can't pass by reaping
// everyone.
// ---------------------------------------------------------------------------

func TestRelayHarness_HealthyClientSurvivesIdleAcrossHeartbeats(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	// What this test verifies: a healthy reading client is NOT reaped while it
	// keeps answering pings across MANY heartbeat intervals. The load-bearing
	// knob is the ping INTERVAL being short relative to the idle window (so the
	// window genuinely spans many heartbeats, not one). The pong TIMEOUT is the
	// reap budget; making it tight does NOT strengthen this test (it only asserts
	// the live client survives, never that a slow pong is reaped - that is the
	// separate negative half, TestRelay_HeartbeatReapsDeadConnection). A tight
	// 80ms budget instead introduced a flake: under load / -race the pong
	// round-trip (server Ping -> client readPump goroutine schedules -> auto-pong
	// -> server reads it) is gated by goroutine scheduling, which can exceed 80ms
	// on a busy machine and falsely reap a genuinely healthy client. So use a
	// short interval (the property under test) with a generous reap budget well
	// above any realistic scheduling delay (jitter is microseconds-to-low-ms; 2s
	// has orders of magnitude of headroom) so jitter can never reap a live peer.
	const hbInterval = 50 * time.Millisecond
	rl.SetHeartbeat(hbInterval, 2*time.Second)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The client's readPump keeps reading, so coder/websocket answers each
	// server ping with a pong. The client sends NOTHING (idle) - liveness
	// rides on the pong alone.
	c := newWSClient(t, ctx, ts, "healthy", slug, id)
	defer c.close()
	c.expectSnapshotFrame(ctx)

	// Span many heartbeat intervals (idleWindow / hbInterval = ~20) over ~1s of
	// pure idle, so the survival is asserted across MANY heartbeats, not one.
	const idleWindow = 1 * time.Second
	deadline := time.Now().Add(idleWindow)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.readErr:
			t.Fatalf("healthy idle client was reaped: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Still a live hub member after the idle window: the heartbeat kept it,
	// did not reap it.
	if got := rl.Registry().AppConns(domainSlug(slug)); got != 1 {
		t.Fatalf("healthy client conns = %d after idle window, want 1 (it was wrongly reaped)", got)
	}
	// And it still receives live traffic (a peer joins and broadcasts).
	peer := newWSClient(t, ctx, ts, "peer", slug, id)
	defer peer.close()
	peer.expectSnapshotFrame(ctx)
	peer.send(ctx, "still-alive")
	c.expectFrame(ctx, 2*time.Second, "live frame after the idle window", func(f []byte) bool {
		return string(f) == "still-alive"
	})
}

// ---------------------------------------------------------------------------
// BACKPRESSURE: a slow / stuck client must not stall the broadcast to the
// others. The room keeps flowing for the fast clients while a stuck client
// (one that never reads) is present; the laggard is buffered-then-dropped,
// never head-of-line-blocking. The drop-the-laggard half is proven
// deterministically against the real wsConn adapter + hub in the relay
// package (TestWSConn_SendBufferBoundsAndDropSignal +
// TestHub_SlowClientDroppedWithoutBlockingRoom); over a real socket the
// kernel's TCP buffers absorb a non-reading client's backlog unreliably, so
// this integration scenario pins the OBSERVABLE non-stalling property with
// MULTIPLE fast clients (a stricter shape than ws_test.go's single-fast
// version): every fast client keeps flowing despite the stuck peer.
//
// WEAKEN DEMO: replace wsConn.Send's non-blocking buffered send in
// relay/relay.go (`select { case c.send <- f: ...; default: return false }`)
// with a blocking `c.send <- f`, so a stuck client head-of-line-blocks the
// room instead of being dropped. Demonstrated RED: "fast0 received only 26
// frames ... room stalled on the laggard". Restoring the non-blocking
// drop-the-laggard send is green.
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

	// The stuck client never reads after its snapshot. We dial it raw (not a
	// wsClient, whose readPump would drain it) and only read the snapshot.
	stuck := dial(t, ctx, ts, slug, id)
	defer stuck.CloseNow()
	if _, _, err := stuck.Read(ctx); err != nil {
		t.Fatalf("stuck client snapshot read: %v", err)
	}
	// stuck never reads again.

	sender := newWSClient(t, ctx, ts, "sender", slug, id)
	defer sender.close()
	sender.expectSnapshotFrame(ctx)

	// Count frames each fast client receives via its readPump.
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

	// Burst large frames. Despite the stuck peer, EVERY fast client must keep
	// accumulating - the broadcast is wait-free per client.
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
// LIMITS: each cap rejects past its bound and the room stays healthy. The
// single-axis checks (per-room cap, frame-size cap, rate-limit) are in
// ws_test.go; this adds the PER-APP and TOTAL-ROOMS caps over the real HTTP
// path (they are unit-tested in the registry, but the spec's gate calls for
// the cap being enforced at the upgrade), and asserts that after a
// rejection the already-connected clients keep working (the room is not
// collateral-damaged by the refusal).
//
// WEAKEN DEMO: remove the per-app cap check in the registry's admit (the
// `if r.perApp[key.App] >= MaxConnsPerApp { return ErrAppFull }` block), so
// the over-cap upgrade gets a 101 instead of a 429. Demonstrated RED: "dial
// past the per-app cap succeeded, want refusal". Restoring the check is green.
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

	// The room stays healthy after the refusal: the two admitted clients still
	// relay to each other (the refusal did not collateral-damage the hub).
	c1.send(ctx, "after-refusal")
	c2.expectFrame(ctx, 2*time.Second, "in-room relay survives the per-app refusal", func(f []byte) bool {
		return string(f) == "after-refusal"
	})
	// And a durable PUT is still mirrored live to both (the durable path is
	// unaffected by the connection cap).
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
	// And the live room relays between the two joiners.
	c1.send(ctx, "in-room")
	c1b.expectFrame(ctx, 2*time.Second, "in-room relay survives the cap", func(f []byte) bool {
		return string(f) == "in-room"
	})
}

// assertDialStatus dials and asserts the upgrade is refused with want before
// any 101 (the cap is enforced at the HTTP layer, not after a socket opens).
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
// CONCURRENTLY through the real upgrade + hub paths. Run under -race, this
// catches a data race in the hub's maps or the registry's per-app counters
// and a panic on a torn-down hub. The hub-level race is also unit-tested
// (TestHub_ConcurrentRegisterBroadcastUnregisterRace); this drives it
// through the FULL socket stack so the goroutine choreography (reader,
// writer, heartbeat, teardown) races too. The terminal assertion is the
// no-leak invariant: once the storm settles, the registry returns to zero
// rooms / zero app conns.
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
			// Connect.
			conn := dial(t, ctx, ts, rr.slug, rr.id)
			// Drain the snapshot.
			rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
			_, _, _ = conn.Read(rctx)
			rcancel()
			// Send a few frames (fans out to whoever else is in this room).
			for i := range 5 {
				if err := conn.Write(ctx, websocket.MessageText, fmt.Appendf(nil, "w%d-%d", w, i)); err != nil {
					break
				}
			}
			// Drain whatever arrived (peer frames) briefly, then disconnect.
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

	// Once the storm settles, every connection has been reaped: no leaked hub,
	// no leaked per-app counter. This is the no-leak invariant under churn.
	// Both axes are polled to their settled value, not read once: teardown is
	// asynchronous and the hub-drop (Rooms) and per-app decrement (AppConns)
	// land in separate steps, so a single read of one right after the other
	// settles can observe an in-flight intermediate. Each axis converges to 0.
	for _, slug := range []string{"appz2345", "bcde2345"} {
		waitForAppConns(t, rl, slug, 0, 10*time.Second)
	}
	waitForRooms(t, rl, 0, 10*time.Second)
}

// ---------------------------------------------------------------------------
// DRAIN HINT: on shutdown begin the relay broadcasts {"type":"reconnect"}
// to every live connection BEFORE any close (SPEC "Drain hint:
// reconnect-before-shutdown") - this drives the exact sequence main runs
// (AnnounceDrain, a serve-through grace, then CloseAll) over real sockets
// and asserts each client receives the hint frame first and the normal
// closure after. The hint carries no seq (it is not a room mutation).
//
// WEAKEN DEMO: delete the AnnounceDrain broadcast (or reorder it after
// CloseAll in main). RED: "c1: expected the reconnect drain hint within
// 3s, none arrived" - clients are hard-cut with no re-home window.
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

	// Drain start: the hint fires while the relay is still serving.
	rl.AnnounceDrain()
	for _, c := range []*wsClient{c1, c2} {
		c.expectFrame(ctx, 3*time.Second, "the reconnect drain hint", func(f []byte) bool {
			return hasType(f, relay.TypeReconnect)
		})
	}

	// The relay is still serving after the hint: a fresh join succeeds (the
	// make-before-break window a hint-acting client re-homes in).
	c3 := newWSClient(t, ctx, ts, "c3", slug, id)
	defer c3.close()
	c3.expectSnapshotFrame(ctx)

	// Then the final close, as main does after the grace window: every
	// connection observes a close AFTER having received the hint.
	rl.Registry().CloseAll()
	c1.expectClosed(ctx, 5*time.Second, "shutdown close after the drain hint")
	c2.expectClosed(ctx, 5*time.Second, "shutdown close after the drain hint")
}

// waitForAppConns polls the registry until the app's live connection count
// equals want, or fails after d. Separate from waitForRoomConns only in the
// failure message (this is the terminal no-leak convergence check).
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
