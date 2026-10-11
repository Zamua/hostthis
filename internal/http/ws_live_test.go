package http

// The realtime room contract against the celld Worker: handler -> proxy ->
// room cell, the production path end to end. These resurrect the guarantees
// the hub-relay suite pinned before its deletion - broadcast fan-out, the
// late-join splice, isolation, the admission cap - now stated against the
// stack that actually ships.
//
// Runs against the local runtime, or the node CELLD_TEST_ENDPOINT names. Slugs
// and rooms are namespaced per run: a node is durable and a rerun must not
// inherit state.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/celldtest"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

func liveCelldServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	base := celldtest.Target(t)
	repo := celld.NewPasteRepo(base, nil)
	srv := &Server{
		// Empty apex: path mode plus no origin verification, which is what a
		// loopback harness needs and exactly how `make run` dev serves.
		ApexDomain: "",
		Pastes:     repo,
		Sites:      liveAppSiteReader{},
		Rooms:      service.NewRooms(celld.NewRoomRepo(base, nil)),
		Relay:      celld.NewRoomProxy(base, nil),
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

// roomValue is one value of testdata/room-wire-vectors.json written to a room.
type roomValue struct {
	name  string
	bytes []byte
	json  bool
	text  string // the wire text of a JSON value, the decoded string of any other
}

// The shared vectors, but those nested 10000 deep: inside the whole-room
// object they pass Go's decoding depth.
func roomValues(t *testing.T) []roomValue {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/room-wire-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name  string          `json:"name"`
			In    string          `json:"in"`
			Hex   string          `json:"hex"`
			Nest  json.RawMessage `json:"nest"`
			JSON  bool            `json:"json"`
			Wire  *string         `json:"wire"`
			Value *string         `json:"value"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	var out []roomValue
	for _, v := range file.Vectors {
		if v.Nest != nil {
			continue
		}
		b := []byte(v.In)
		if v.Hex != "" {
			if b, err = hex.DecodeString(v.Hex); err != nil {
				t.Fatal(err)
			}
		}
		text := string([]rune(string(b)))
		if v.Wire != nil {
			text = *v.Wire
		} else if v.Value != nil {
			text = *v.Value
		}
		out = append(out, roomValue{name: v.Name, bytes: b, json: v.JSON, text: text})
	}
	return out
}

func liveReadRaw(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return data
}

// Every value reaches a client byte for byte as the whole-room GET serves it,
// in the snapshot and in a put frame: the text the cell stored, never a
// re-serialization of it (which turns 1e400 into null and -0 into 0, and
// reorders an object's integer-like names).
func TestLiveRoom_FramesCarryTheBytesTheGETServes(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	values := roomValues(t)
	// Keys put out of order: both the GET and the snapshot list them as Go
	// sorts a map's keys.
	values = append(values, roomValue{name: "\U0001F600", bytes: []byte("1"), json: true, text: "1"},
		roomValue{name: "�", bytes: []byte("2"), json: true, text: "2"})
	for _, v := range values {
		livePut(t, ts, slug, id, url.PathEscape(v.name), string(v.bytes))
	}

	resp, err := nethttp.Get(ts.URL + "/p/" + slug + "/api/rooms/" + id)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var scan map[string]json.RawMessage
	if err := json.Unmarshal(body, &scan); err != nil {
		t.Fatalf("scan %q: %v", body, err)
	}
	for _, v := range values {
		raw := scan[v.name]
		if v.json {
			if got := string(raw); got != strings.Trim(v.text, " \t\r\n") {
				t.Errorf("%s: GET carries %q, want the stored text %q", v.name, got, v.text)
			}
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != v.text {
			t.Errorf("%s: GET carries %q (%v), want a string of %q", v.name, raw, err, v.text)
		}
	}

	c := liveDialRoom(t, ctx, ts, slug, id)
	defer c.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	snapshot := liveReadRaw(t, ctx, c)
	want := fmt.Sprintf(`{"type":"snapshot","seq":%d,"state":%s}`, len(values), bytes.TrimSuffix(body, []byte("\n")))
	if string(snapshot) != want {
		t.Fatalf("snapshot is not the GET's bytes:\n got %q\nwant %q", snapshot, want)
	}

	for _, v := range values {
		livePut(t, ts, slug, id, url.PathEscape(v.name), string(v.bytes))
		var put struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		frame := liveReadRaw(t, ctx, c)
		if err := json.Unmarshal(frame, &put); err != nil || put.Key != v.name {
			t.Fatalf("put frame %q (%v), want key %q", frame, err, v.name)
		}
		if !bytes.Equal(put.Value, scan[v.name]) {
			t.Errorf("%s: put frame carries %q, GET %q", v.name, put.Value, scan[v.name])
		}
	}
}

// A JSON value holding bytes that are not UTF-8 reads back as the same valid
// UTF-8 from the whole-room GET, the snapshot and the put frame, so no client
// decoder is left to replace the bytes its own way.
func TestLiveRoom_ScanAndFramesCarryOneWireValue(t *testing.T) {
	ts, slug := liveCelldServer(t)
	id := liveCreateRoom(t, ts, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const value = "\"a\xe2\x82\xffb\""
	const want = "a���b"
	livePut(t, ts, slug, id, "before", value)
	c := liveDialRoom(t, ctx, ts, slug, id)
	defer c.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	snap := liveRead(t, ctx, c)
	livePut(t, ts, slug, id, "after", value)
	put := liveRead(t, ctx, c)

	resp, err := nethttp.Get(ts.URL + "/p/" + slug + "/api/rooms/" + id)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !utf8.Valid(body) {
		t.Fatalf("scan body %q is not valid UTF-8", body)
	}
	var scan map[string]json.RawMessage
	if err := json.Unmarshal(body, &scan); err != nil {
		t.Fatalf("scan %q: %v", body, err)
	}
	for name, raw := range map[string]json.RawMessage{
		"scan before": scan["before"], "scan after": scan["after"],
		"snapshot": snap.State["before"], "put frame": put.Value,
	} {
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != want {
			t.Errorf("%s value %q (%v), want %q", name, raw, err, want)
		}
	}
}
