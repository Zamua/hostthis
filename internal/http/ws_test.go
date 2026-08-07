package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

const wsTestApex = "hostthis.test"

// wsTestServer wires a real Rooms service over a real sqlite repo plus a relay,
// fronted by an httptest.Server, so tests dial real WebSocket connections
// through the full mux + upgrade handler.
func wsTestServer(t *testing.T, limits relay.Limits) (*httptest.Server, *service.Rooms, *relay.Relay) {
	t.Helper()
	rooms := service.NewRooms(storage.NewShaleRoomRepo(storagetest.NewRepo(t)))
	rl := relay.NewRelay(rooms, limits)
	srv := &Server{
		ApexDomain: wsTestApex,
		Rooms:      rooms,
		Relay:      rl,
		Sites:      liveAppSiteReader{},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, rooms, rl
}

// mkRoom creates a room directly through the service, bypassing HTTP.
func mkRoom(t *testing.T, rooms *service.Rooms, slug string) string {
	t.Helper()
	room, err := rooms.Create(domain.Slug(slug), "203.0.113.0/24")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	return room.ID.String()
}

// wsURL builds the subdomain-mode relay URL. The slug is not in the path: the
// dial's Host header is what slugFromHost resolves, and what makes the Origin
// policy read the request as same-origin.
func wsURL(ts *httptest.Server, _ /*slug*/, id string) string {
	base := strings.Replace(ts.URL, "http://", "ws://", 1)
	return base + "/api/rooms/" + id + "/ws"
}

// dial opens a relay WebSocket against ts for (slug, id).
func dial(t *testing.T, ctx context.Context, ts *httptest.Server, slug, id string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, wsURL(ts, slug, id), &websocket.DialOptions{
		Host: slug + "." + wsTestApex,
	})
	if err != nil {
		t.Fatalf("dial %s/%s: %v", slug, id, err)
	}
	// Raise the CLIENT read limit (default 32 KiB) for the larger frames the
	// backpressure test sends. Unrelated to the server's MaxMessageBytes cap,
	// which bounds INBOUND frames.
	c.SetReadLimit(1 << 20)
	return c
}

func readJSON(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return m
}

// expectSnapshot asserts the next frame is the snapshot and returns its state.
func expectSnapshot(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	m := readJSON(t, ctx, c)
	if m["type"] != relay.TypeSnapshot {
		t.Fatalf("first frame type = %v, want snapshot", m["type"])
	}
	state, _ := m["state"].(map[string]any)
	return state
}

func TestRelay_BroadcastReachesPeersNotSender(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a := dial(t, ctx, ts, slug, id)
	defer a.CloseNow() //nolint:errcheck
	b := dial(t, ctx, ts, slug, id)
	defer b.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, a)
	expectSnapshot(t, ctx, b)

	// B must receive A's frame verbatim; A must not see its own.
	if err := a.Write(ctx, websocket.MessageText, []byte("cursor:10,20")); err != nil {
		t.Fatalf("a write: %v", err)
	}
	_, got, err := b.Read(ctx)
	if err != nil {
		t.Fatalf("b read: %v", err)
	}
	if string(got) != "cursor:10,20" {
		t.Fatalf("b got %q, want the verbatim relayed frame", got)
	}

	// Nothing queued for A, so a short-deadline read times out.
	shortCtx, scancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer scancel()
	if _, _, err := a.Read(shortCtx); err == nil {
		t.Fatal("sender A received its own relayed frame")
	}
}

// httpPut PUTs a room value through the live server, so the relay's
// durable-mirror path fires on the real wired relay.
func httpPut(t *testing.T, ts *httptest.Server, slug, id, key string, body []byte) {
	t.Helper()
	req, err := nethttp.NewRequest("PUT", ts.URL+"/api/rooms/"+id+"/"+key, bytesReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = slug + "." + wsTestApex
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("http put: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != nethttp.StatusNoContent {
		t.Fatalf("http put status = %d, want 204", resp.StatusCode)
	}
}

// httpDelete is the DELETE twin of httpPut, driving the relay's durable-delete
// mirror path. A present and an absent key both return 204 (idempotent).
func httpDelete(t *testing.T, ts *httptest.Server, slug, id, key string) {
	t.Helper()
	req, err := nethttp.NewRequest("DELETE", ts.URL+"/api/rooms/"+id+"/"+key, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = slug + "." + wsTestApex
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("http delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != nethttp.StatusNoContent {
		t.Fatalf("http delete status = %d, want 204", resp.StatusCode)
	}
}

func TestRelay_LateJoinSnapshotThenStreamNoGapNoDup(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Durable state committed BEFORE the late joiner connects.
	if _, err := rooms.Put(domain.Slug(slug), domain.RoomID(id), "cell/1", []byte(`"busy"`)); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	// No gap: a pre-join write is already in the joiner's first frame.
	late := dial(t, ctx, ts, slug, id)
	defer late.CloseNow() //nolint:errcheck
	state := expectSnapshot(t, ctx, late)
	if state["cell/1"] != "busy" {
		t.Fatalf("snapshot cell/1 = %v, want busy (pre-join write must be in snapshot)", state["cell/1"])
	}

	// A durable PUT after the join arrives as exactly one live mirror frame.
	httpPut(t, ts, slug, id, "cell/2", []byte(`"free"`))
	m := readJSON(t, ctx, late)
	if m["type"] != relay.TypePut || m["key"] != "cell/2" || m["value"] != "free" {
		t.Fatalf("live mirror frame = %v, want put cell/2=free", m)
	}

	// No dup: a short read after that one frame times out.
	shortCtx, scancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer scancel()
	if _, _, err := late.Read(shortCtx); err == nil {
		t.Fatal("late joiner received a duplicate of the single PUT mirror")
	}
}

func TestRelay_ReconnectReSyncsFromSnapshot(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect, then disconnect: a page reload.
	first := dial(t, ctx, ts, slug, id)
	expectSnapshot(t, ctx, first)
	first.CloseNow() //nolint:errcheck

	// A durable write lands while the client is away.
	httpPut(t, ts, slug, id, "k", []byte(`"v"`))

	// A fresh join re-syncs full current state from the KV snapshot: there is
	// no incremental "catch me up from N" replay to get wrong.
	again := dial(t, ctx, ts, slug, id)
	defer again.CloseNow() //nolint:errcheck
	state := expectSnapshot(t, ctx, again)
	if state["k"] != "v" {
		t.Fatalf("reconnect snapshot k = %v, want v (the away-time write must be caught up)", state["k"])
	}
}

// TestRelay_StuckClientDoesNotStallTheRoom pins that a non-reading client does
// not head-of-line-block the room: a fast client keeps receiving while a stuck
// one is a hub member.
//
// Only the non-stalling half is testable here. A socket-level drop over
// loopback is unreproducible (the kernel's TCP buffers absorb the backlog), so
// the DROP-the-laggard half is pinned in the relay package instead
// (TestWSConn_SendBufferBoundsAndDropSignal,
// TestHub_SlowClientDroppedWithoutBlockingRoom).
func TestRelay_StuckClientDoesNotStallTheRoom(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxMsgsPerSec = 0         // isolate from the rate limit
	lim.MaxMessageBytes = 1 << 20 // allow the larger frames this test sends
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fast := dial(t, ctx, ts, slug, id)
	defer fast.CloseNow()               //nolint:errcheck
	stuck := dial(t, ctx, ts, slug, id) // never reads after its snapshot
	defer stuck.CloseNow()              //nolint:errcheck
	sender := dial(t, ctx, ts, slug, id)
	defer sender.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, fast)
	expectSnapshot(t, ctx, stuck)
	expectSnapshot(t, ctx, sender)

	// fast drains promptly; stuck never reads again.
	var fastGot int32
	go func() {
		for {
			if _, _, err := fast.Read(ctx); err != nil {
				return
			}
			atomic.AddInt32(&fastGot, 1)
		}
	}()

	// The broadcast is wait-free per client, so this burst must keep reaching
	// fast with the stuck peer present.
	payload := make([]byte, 64<<10)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := sender.Write(ctx, websocket.MessageBinary, payload); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fastGot) >= 50 {
			return // room flowing for fast despite the stuck peer
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fast received only %d frames with a stuck peer present: room stalled on the laggard", atomic.LoadInt32(&fastGot))
}

func TestRelay_HeartbeatReapsDeadConnection(t *testing.T) {
	ts, rooms, rl := wsTestServer(t, relay.NewLimits())
	// Aggressive heartbeat so the reap fires quickly.
	rl.SetHeartbeat(100*time.Millisecond, 150*time.Millisecond)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dial(t, ctx, ts, slug, id)
	defer c.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, c)

	// coder/websocket only processes control frames during a Read, so a client
	// that stops reading never pongs: that models a vanished client, and the
	// server's Ping times out and reaps it.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if rl.Registry().Rooms() == 0 {
			return // reaped: the hub was torn down
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dead connection was not reaped: registry still has %d rooms", rl.Registry().Rooms())
}

func TestRelay_StrictIsolationAcrossRoomsAndApps(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const appA, appB = "appz2345", "bcde2345"
	roomA1 := mkRoom(t, rooms, appA)
	roomA2 := mkRoom(t, rooms, appA) // same app, different room
	roomB1 := mkRoom(t, rooms, appB) // different app

	a1 := dial(t, ctx, ts, appA, roomA1)
	defer a1.CloseNow() //nolint:errcheck
	a2 := dial(t, ctx, ts, appA, roomA2)
	defer a2.CloseNow() //nolint:errcheck
	b1 := dial(t, ctx, ts, appB, roomB1)
	defer b1.CloseNow() //nolint:errcheck
	for _, c := range []*websocket.Conn{a1, a2, b1} {
		expectSnapshot(t, ctx, c)
	}

	// A frame in roomA1 reaches no one in roomA2 (same app) or roomB1
	// (different app), so every other connection's short read times out.
	if err := a1.Write(ctx, websocket.MessageText, []byte("secret")); err != nil {
		t.Fatalf("a1 write: %v", err)
	}
	for name, c := range map[string]*websocket.Conn{"roomA2": a2, "roomB1": b1} {
		shortCtx, scancel := context.WithTimeout(ctx, 300*time.Millisecond)
		if _, _, err := c.Read(shortCtx); err == nil {
			scancel()
			t.Fatalf("%s saw roomA1's frame: isolation broken", name)
		}
		scancel()
	}
}

func TestRelay_PerRoomConnectionCapRefuses(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxConnsPerRoom = 2
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c1 := dial(t, ctx, ts, slug, id)
	defer c1.CloseNow() //nolint:errcheck
	c2 := dial(t, ctx, ts, slug, id)
	defer c2.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, c1)
	expectSnapshot(t, ctx, c2)

	// The third upgrade is refused with a 429 before the 101 handshake.
	_, resp, err := websocket.Dial(ctx, wsURL(ts, slug, id), &websocket.DialOptions{Host: slug + "." + wsTestApex})
	if err == nil {
		t.Fatal("third dial succeeded past the per-room cap")
	}
	if resp == nil || resp.StatusCode != nethttp.StatusTooManyRequests {
		t.Fatalf("over-cap dial status = %v, want 429", resp)
	}
}

func TestRelay_FrameSizeCapClosesConnection(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxMessageBytes = 64 // tiny cap
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dial(t, ctx, ts, slug, id)
	defer c.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, c)

	// Over the cap: the server's SetReadLimit closes with
	// StatusMessageTooBig, so the client's next read errors.
	big := make([]byte, 256)
	_ = c.Write(ctx, websocket.MessageText, big)
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	if _, _, err := c.Read(rctx); err == nil {
		t.Fatal("connection not closed after an over-cap frame")
	}
}

func TestRelay_RateLimitDropsFloodingClient(t *testing.T) {
	lim := relay.NewLimits()
	lim.MaxMsgsPerSec = 5 // low ceiling
	lim.SendBuffer = 256  // big buffer so the DROP is the rate limit, not backpressure
	ts, rooms, _ := wsTestServer(t, lim)
	const slug = "appz2345"
	id := mkRoom(t, rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	flooder := dial(t, ctx, ts, slug, id)
	defer flooder.CloseNow() //nolint:errcheck
	expectSnapshot(t, ctx, flooder)

	// A burst past the per-second ceiling: the server drops the connection
	// once the bucket empties.
	for range 100 {
		if err := flooder.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
			break
		}
	}
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	if _, _, err := flooder.Read(rctx); err == nil {
		t.Fatal("flooding client was not dropped by the rate limit")
	}
}

func TestRelay_UpgradeValidation(t *testing.T) {
	ts, rooms, _ := wsTestServer(t, relay.NewLimits())
	const liveSlug = "appz2345"
	id := mkRoom(t, rooms, liveSlug)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("unknown app slug 404", func(t *testing.T) {
		// liveAppSiteReader treats every slug as live, so the unknown-slug
		// path is not reachable through this harness; the appExists unit
		// coverage carries it. What is asserted here is the other 404: a
		// well-formed but nonexistent room.
		bogus := domain.NewRoomID().String()
		_, resp, err := websocket.Dial(ctx, wsURL(ts, liveSlug, bogus), &websocket.DialOptions{Host: liveSlug + "." + wsTestApex})
		if err == nil {
			t.Fatal("dial to a nonexistent room succeeded")
		}
		if resp == nil || resp.StatusCode != nethttp.StatusNotFound {
			t.Fatalf("nonexistent room status = %v, want 404", resp)
		}
	})

	t.Run("malformed uuid 400", func(t *testing.T) {
		base := strings.Replace(ts.URL, "http://", "ws://", 1)
		_, resp, err := websocket.Dial(ctx, base+"/api/rooms/not-a-uuid/ws", &websocket.DialOptions{Host: liveSlug + "." + wsTestApex})
		if err == nil {
			t.Fatal("dial with a malformed uuid succeeded")
		}
		if resp == nil || resp.StatusCode != nethttp.StatusBadRequest {
			t.Fatalf("malformed uuid status = %v, want 400", resp)
		}
	})

	t.Run("non-upgrade GET is not 101", func(t *testing.T) {
		// websocket.Accept refuses a GET with no Upgrade header, so the /ws
		// path never answers 101.
		req, _ := nethttp.NewRequest("GET", ts.URL+"/api/rooms/"+id+"/ws", nil)
		req.Host = liveSlug + "." + wsTestApex
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("plain GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == nethttp.StatusSwitchingProtocols {
			t.Fatal("a non-Upgrade GET got a 101")
		}
	})
}
