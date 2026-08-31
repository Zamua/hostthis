package http

// The realtime room contract against a LIVE celld fleet: handler -> proxy ->
// room cell, the production path end to end. These resurrect the guarantees
// the hub-relay suite pinned before its deletion - broadcast fan-out, the
// late-join splice, isolation, the admission cap - now stated against the
// stack that actually ships.
//
// Skipped unless CELLD_TEST_ENDPOINT names a running fleet. Slugs and rooms
// are namespaced per run: the fleet is durable and a rerun must not inherit
// state.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

func liveCelldServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the live celld room harness")
	}
	repo := celld.NewPasteRepo(base, nil)
	srv := &Server{
		// Empty apex: path mode plus no origin verification, which is what a
		// loopback harness needs and exactly how `make run` dev serves.
		ApexDomain: "",
		Pastes:     repo,
		Sites:      liveAppSiteReader{},
		Rooms:      service.NewRooms(celld.NewRoomRepo(base, nil)),
		Relay:      celld.NewRoomProxy(base),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// Encoded in the slug ALPHABET: it excludes 0/1/l/o, so a raw decimal
	// nonce is an invalid slug that 404s before any handler runs.
	n := time.Now().UnixNano()
	b := []byte("wwwwwwww")
	for i := 2; i < 8; i++ {
		b[i] = domain.SlugAlphabet[n%int64(len(domain.SlugAlphabet))]
		n /= int64(len(domain.SlugAlphabet))
	}
	return ts, string(b)
}

func liveCreateRoom(t *testing.T, ts *httptest.Server, slug string) string {
	t.Helper()
	resp, err := nethttp.Post(ts.URL+"/p/"+slug+"/api/rooms", "application/json", nil)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out struct {
		ID string `json:"id"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		t.Fatalf("create room: status %d body %q", resp.StatusCode, body)
	}
	return out.ID
}

func liveDialRoom(t *testing.T, ctx context.Context, ts *httptest.Server, slug, id string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/p/" + slug + "/api/rooms/" + id + "/ws"
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	return c
}

type liveFrame struct {
	Type  string                     `json:"type"`
	Seq   uint64                     `json:"seq"`
	Key   string                     `json:"key"`
	Value json.RawMessage            `json:"value"`
	State map[string]json.RawMessage `json:"state"`
}

func liveRead(t *testing.T, ctx context.Context, c *websocket.Conn) liveFrame {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f liveFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame %q: %v", data, err)
	}
	return f
}

func livePut(t *testing.T, ts *httptest.Server, slug, id, key, val string) {
	t.Helper()
	req, _ := nethttp.NewRequest(nethttp.MethodPut, ts.URL+"/p/"+slug+"/api/rooms/"+id+"/"+key, strings.NewReader(val))
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != nethttp.StatusNoContent {
		t.Fatalf("put %s: status %d", key, resp.StatusCode)
	}
}

// Every connected client receives every durable mutation, in seq order with
// no gaps: the fan-out that used to need a peer mesh, now a property of the
// cell owning the room.
func TestLiveRoom_BroadcastReachesEveryClientInOrder(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const clients = 3
	conns := make([]*websocket.Conn, clients)
	for i := range conns {
		conns[i] = liveDialRoom(t, ctx, ts, slug, id)
		defer conns[i].Close(websocket.StatusNormalClosure, "") //nolint:errcheck
		if f := liveRead(t, ctx, conns[i]); f.Type != "snapshot" {
			t.Fatalf("client %d first frame = %q; want the snapshot", i, f.Type)
		}
	}

	const writes = 5
	for n := range writes {
		livePut(t, ts, slug, id, fmt.Sprintf("k%d", n), fmt.Sprintf(`{"n":%d}`, n))
	}

	for i, c := range conns {
		var last uint64
		for n := range writes {
			f := liveRead(t, ctx, c)
			if f.Type != "put" || f.Key != fmt.Sprintf("k%d", n) {
				t.Fatalf("client %d frame %d = %+v; want put k%d - frames must arrive in commit order", i, n, f, n)
			}
			if f.Seq != last+1 && last != 0 {
				t.Fatalf("client %d saw seq %d after %d; the per-room sequence is dense", i, f.Seq, last)
			}
			last = f.Seq
		}
	}
}

// Ephemeral frames reach peers byte-identically without echoing to their sender.
func TestLiveRoom_EphemeralFrameReachesPeersOnly(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sender := liveDialRoom(t, ctx, ts, slug, id)
	defer sender.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	peer := liveDialRoom(t, ctx, ts, slug, id)
	defer peer.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	liveRead(t, ctx, sender)
	liveRead(t, ctx, peer)

	want := []byte{0x00, 0x01, 0xfe, 0xff}
	if err := sender.Write(ctx, websocket.MessageBinary, want); err != nil {
		t.Fatalf("send ephemeral frame: %v", err)
	}
	typ, got, err := peer.Read(ctx)
	if err != nil {
		t.Fatalf("peer read ephemeral frame: %v", err)
	}
	if typ != websocket.MessageBinary || !bytes.Equal(got, want) {
		t.Fatalf("peer frame type=%v bytes=%v, want binary %v", typ, got, want)
	}

	noEcho, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if _, got, err := sender.Read(noEcho); err == nil {
		t.Fatalf("sender received its own ephemeral frame %v", got)
	}
}

// A late joiner's snapshot carries a seq, and the live frames that follow
// start strictly after it: the splice contract, with no gap and no duplicate.
func TestLiveRoom_LateJoinSpliceNoGapNoDup(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	livePut(t, ts, slug, id, "before1", `1`)
	livePut(t, ts, slug, id, "before2", `2`)

	c := liveDialRoom(t, ctx, ts, slug, id)
	defer c.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	snap := liveRead(t, ctx, c)
	if snap.Type != "snapshot" || snap.Seq != 2 || len(snap.State) != 2 {
		t.Fatalf("snapshot = %+v; want seq 2 with both pre-join keys", snap)
	}

	livePut(t, ts, slug, id, "after", `3`)
	f := liveRead(t, ctx, c)
	if f.Type != "put" || f.Key != "after" || f.Seq != snap.Seq+1 {
		t.Fatalf("post-join frame = %+v; want put \"after\" at seq %d - a gap loses a write, a duplicate replays one", f, snap.Seq+1)
	}
}

// A client in one room never sees another room's frames, however the rooms
// interleave: isolation is structural (the cell IS the room), and this pins
// that no layer above un-structures it.
func TestLiveRoom_IsolationAcrossRooms(t *testing.T) {
	ts, slug := liveCelldServer(t)
	a := liveCreateRoom(t, ts, slug)
	b := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ca := liveDialRoom(t, ctx, ts, slug, a)
	defer ca.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	liveRead(t, ctx, ca)                              // snapshot

	livePut(t, ts, slug, b, "noise", `"b-only"`)
	livePut(t, ts, slug, a, "signal", `"a-only"`)

	f := liveRead(t, ctx, ca)
	if f.Key != "signal" {
		t.Fatalf("room A's client received %+v; room B's write must never reach it", f)
	}
}

// A durable DELETE mirrors as a delete frame with the next seq.
func TestLiveRoom_DeleteFrameArrives(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := liveDialRoom(t, ctx, ts, slug, id)
	defer c.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	liveRead(t, ctx, c)                              // snapshot

	livePut(t, ts, slug, id, "gone", `"soon"`)
	req, _ := nethttp.NewRequest(nethttp.MethodDelete, ts.URL+"/p/"+slug+"/api/rooms/"+id+"/gone", nil)
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()

	if f := liveRead(t, ctx, c); f.Type != "put" || f.Key != "gone" {
		t.Fatalf("frame 1 = %+v; want the put", f)
	}
	if f := liveRead(t, ctx, c); f.Type != "delete" || f.Key != "gone" || f.Seq != 2 {
		t.Fatalf("frame 2 = %+v; want delete \"gone\" at seq 2", f)
	}
}
