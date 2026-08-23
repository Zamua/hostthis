package celld_test

// Measures what a celld room cell costs and what happens to its clients when
// the cell moves. Skipped unless CELLD_TEST_ENDPOINT names a running fleet.
//
// Rooms are the part of hostthis that does NOT scale down: celld will not shed
// a cell with a live host WebSocket, so a room holds its memory for as long as
// anyone is connected. That makes rooms the one surface that could send the
// port back to the topology, which is why it is measured before the remaining
// adapters are written rather than after.
//
// These are MEASUREMENTS, not assertions about a contract. They print numbers
// for a human and fail only on something that would invalidate the reading.
// Upstream flags cross-node close codes and reconnection as its thinnest test
// coverage, so an oddity here is as likely to be an upstream gap as a bug here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func celldEndpoint(t *testing.T) string {
	t.Helper()
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld room probe")
	}
	return base
}

type roomCount struct {
	Sockets int `json:"sockets"`
	Seen    int `json:"seen"`
}

func readCount(t *testing.T, base, room string) roomCount {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/room/count?room=%s", base, room))
	if err != nil {
		t.Fatalf("count %s: %v", room, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var c roomCount
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	return c
}

func dialRoom(t *testing.T, ctx context.Context, base, room string) *websocket.Conn {
	t.Helper()
	u := strings.Replace(base, "http", "ws", 1) + "/room/join?room=" + room
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", room, err)
	}
	return c
}

// A connected room echoes and counts, and the count survives in storage. This
// is the baseline every other reading depends on: if the socket does not work
// at all, a RAM number means nothing.
func TestRoomProbe_EchoAndPersist(t *testing.T) {
	base := celldEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const room = "probe-echo"
	c := dialRoom(t, ctx, base, room)
	defer c.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck

	for i := range 3 {
		if err := c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf("hello-%d", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !strings.Contains(string(data), fmt.Sprintf("hello-%d", i)) {
			t.Fatalf("echo %d = %s; want it to contain the sent frame", i, data)
		}
	}
	got := readCount(t, base, room)
	if got.Sockets < 1 {
		t.Fatalf("cell reports %d sockets; want at least the one this test holds", got.Sockets)
	}
	t.Logf("MEASURED connected room: sockets=%d seen=%d", got.Sockets, got.Seen)
}

// What a room costs. Opens N rooms with one client each and leaves them
// connected, so an operator can read pod RSS against a known cell count.
// CELLD_PROBE_ROOMS sets N; the default keeps a casual run cheap.
func TestRoomProbe_ResidentCost(t *testing.T) {
	base := celldEndpoint(t)
	n := 20
	if v := os.Getenv("CELLD_PROBE_ROOMS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			t.Fatalf("CELLD_PROBE_ROOMS=%q: %v", v, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	conns := make([]*websocket.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close(websocket.StatusNormalClosure, "done")
		}
	}()
	for i := range n {
		room := fmt.Sprintf("probe-cost-%d", i)
		c := dialRoom(t, ctx, base, room)
		conns = append(conns, c)
		// One frame each, so the cell has actually done work rather than only
		// accepted a socket.
		if err := c.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
			t.Fatalf("write to %s: %v", room, err)
		}
		if _, _, err := c.Read(ctx); err != nil {
			t.Fatalf("read from %s: %v", room, err)
		}
	}
	t.Logf("MEASURED %d rooms connected and echoing; hold here and read pod RSS", n)
	// Held briefly so an external `kubectl top` lands while the sockets are up.
	hold := 20 * time.Second
	if v := os.Getenv("CELLD_PROBE_HOLD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			hold = d
		}
	}
	time.Sleep(hold)
	t.Logf("MEASURED still connected after %s", hold)
}

// What a client sees when the cell moves. Run with the fleet restarted or
// scaled during CELLD_PROBE_HOLD; the close code is the reading, because
// upstream's own limitations page calls cross-node close codes its thinnest
// coverage and "it reconnected" is a weaker claim than "it closed with 1001".
func TestRoomProbe_CloseCodeOnMove(t *testing.T) {
	base := celldEndpoint(t)
	if os.Getenv("CELLD_PROBE_MOVE") == "" {
		t.Skip("CELLD_PROBE_MOVE not set; this probe needs the fleet moved underneath it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const room = "probe-move"
	c := dialRoom(t, ctx, base, room)
	if err := c.Write(ctx, websocket.MessageText, []byte("before-move")); err != nil {
		t.Fatalf("write before move: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read before move: %v", err)
	}
	t.Log("MEASURED connected and echoing; move the fleet now")

	// Blocks until the peer goes away. The error carries the close code, which
	// is the whole point of the probe.
	_, _, err := c.Read(ctx)
	if err == nil {
		t.Fatal("read succeeded after the move window; the cell did not move")
	}
	t.Logf("MEASURED client observed on move: status=%v err=%v", websocket.CloseStatus(err), err)
}
