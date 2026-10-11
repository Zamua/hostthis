package parity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
)

const (
	pingFrame = `{"type":"ping"}`
	pongFrame = `{"type":"pong"}`
)

// handshake is a WebSocket upgrade request for target, as a browser on
// origin would send it.
func handshake(host, target, origin string) request {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	r := get(host, target).
		with("Upgrade", "websocket").
		with("Connection", "Upgrade").
		with("Sec-WebSocket-Version", "13").
		with("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	if origin != "" {
		r = r.with("Origin", origin)
	}
	return r
}

// socket is one client's connection to a room through one server.
type socket struct {
	via  string
	conn *websocket.Conn
}

func (w *world) join(t *testing.T, s *server, host, room string) *socket {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	r := request{host: host, target: "/api/rooms/" + room + "/ws"}.with("Origin", "http://"+host)
	c, res, err := s.dial(ctx, r)
	if err != nil {
		status := 0
		if res != nil {
			status = res.StatusCode
		}
		t.Fatalf("join %s through %s: %v (status %d)", room, s.name, err, status)
	}
	c.SetReadLimit(4 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	return &socket{via: s.name, conn: c}
}

type frame struct {
	typ  websocket.MessageType
	data []byte
}

func (s *socket) read(t *testing.T) (frame, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	typ, data, err := s.conn.Read(ctx)
	return frame{typ, data}, err
}

func (s *socket) next(t *testing.T) frame {
	t.Helper()
	f, err := s.read(t)
	if err != nil {
		t.Fatalf("read through %s: %v", s.via, err)
	}
	return f
}

func (s *socket) send(t *testing.T, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.conn.Write(ctx, typ, data); err != nil {
		t.Fatalf("write through %s: %v", s.via, err)
	}
}

// quiet proves nothing reached s before now: a ping's answer is its next
// frame.
func (s *socket) quiet(t *testing.T) {
	t.Helper()
	s.send(t, websocket.MessageText, []byte(pingFrame))
	if f := s.next(t); string(f.data) != pongFrame {
		t.Errorf("through %s: frame %q arrived where only the pong should", s.via, f.data)
	}
}

// closeCode is the code s's connection closes with once it is read.
func (s *socket) closeCode(t *testing.T) websocket.StatusCode {
	t.Helper()
	for {
		_, err := s.read(t)
		if err == nil {
			continue
		}
		if code := websocket.CloseStatus(err); code != -1 {
			return code
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("through %s: still open", s.via)
		}
		return -1
	}
}

func seqOf(t *testing.T, f frame) int {
	t.Helper()
	var v struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(f.data, &v); err != nil {
		t.Fatalf("frame %q: %v", f.data, err)
	}
	return v.Seq
}

func testWebSocket(t *testing.T, w *world) {
	if !w.writable {
		t.Skip("read-only: socket checks write; set PARITY_ALLOW_WRITES=1 and PARITY_WRITE_SLUG")
	}
	app := w.roomApp(t)
	host := w.host(app)
	room := w.createRoom(t, w.hostthisd, host)
	mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: keyTarget(room, "seed<&>"), body: []byte(`{"n": 1.50}`)}, 204)
	never := w.host(domain.NewRandomSlug().String())
	ws := "/api/rooms/" + room + "/ws"
	origin := "http://" + host

	t.Run("handshake", func(t *testing.T) {
		w.expect(t, handshake(host, "/api/rooms/not-a-uuid/ws", origin), 400)
		w.expect(t, handshake(never, ws, "http://"+never), 404)
		if w.local {
			other := w.host(w.roomApp(t))
			w.expect(t, handshake(other, ws, "http://"+other), 404)
		}
		w.expect(t, handshake(host, "/api/rooms/"+domain.NewRoomID().String()+"/ws", origin), 404)
		w.expect(t, handshake(host, ws, "https://evil.example"), 403)
		w.expect(t, handshake(host, ws, "null"), 403)
		w.expect(t, get(host, ws), 426)
		ok := handshake(host, ws, origin)
		without := func(name string) request {
			r := ok.with(name, "")
			r.header.Del(name)
			return r
		}
		w.expect(t, ok.with("Connection", "keep-alive"), 426)
		w.expect(t, ok.with("Upgrade", "h2c"), 426)
		w.expect(t, without("Upgrade"), 426)
		w.expect(t, ok.with("Sec-WebSocket-Version", "12"), 400)
		w.expect(t, without("Sec-WebSocket-Version"), 400)
		w.expect(t, without("Sec-WebSocket-Key"), 400)
		w.expect(t, ok.with("Sec-WebSocket-Key", "short"), 400)
		w.expect(t, ok.with("Connection", "keep-alive, Upgrade").with("Upgrade", "WebSocket"), 101)
		w.expect(t, ok.with("Upgrade", "h2c, websocket"), 101)
		// A failed check comes after the app and room checks, before the origin.
		w.expect(t, handshake(host, "/api/rooms/"+domain.NewRoomID().String()+"/ws", origin).with("Sec-WebSocket-Version", "12"), 404)
		w.expect(t, ok.with("Sec-WebSocket-Version", "12").with("Origin", "https://evil.example"), 400)
		for _, m := range []string{"POST", "PUT"} {
			r := ok
			r.method = m
			w.expect(t, r, 405)
			r.header = r.header.Clone()
			r.header.Del("Upgrade")
			w.expect(t, r, 426)
		}
		for _, o := range []string{origin, "", "http://other." + w.apex, "https://" + w.apex, "http://" + strings.ToUpper(host)} {
			w.expect(t, handshake(host, ws, o), 101)
		}
		w.expect(t, handshake(host, "/api/rooms/"+strings.ToUpper(room)+"/ws", origin), 101)
	})

	t.Run("frames", func(t *testing.T) {
		servers := append([]*server{w.hostthisd}, w.workers...)
		var socks []*socket
		for _, s := range servers {
			socks = append(socks, w.join(t, s, host, room))
		}
		// The snapshot comes from the Room cell on every path.
		snap := socks[0].next(t)
		for _, s := range socks[1:] {
			if f := s.next(t); !bytes.Equal(f.data, snap.data) {
				t.Errorf("snapshot through %s %q, through hostthisd %q", s.via, f.data, snap.data)
			}
		}
		seq := seqOf(t, snap)

		// Durable mirrors reach every socket with the same bytes and a dense seq,
		// whichever server took the write.
		writes := []struct {
			via  *server
			r    request
			kind string
		}{
			{w.hostthisd, request{method: "PUT", host: host, target: keyTarget(room, "k"), body: []byte(`{"b": [1e400, -0]}`)}, "put"},
			{w.workers[0], request{method: "PUT", host: host, target: keyTarget(room, "k< >"), body: []byte("\xffnot json")}, "put"},
			{w.workers[len(w.workers)-1], request{method: "DELETE", host: host, target: keyTarget(room, "k")}, "delete"},
			{w.hostthisd, request{method: "DELETE", host: host, target: keyTarget(room, "absent")}, "delete"},
		}
		for _, wr := range writes {
			mustDo(t, wr.via, wr.r, 204)
			seq++
			first := socks[0].next(t)
			if got := seqOf(t, first); got != seq || !strings.Contains(string(first.data), `"type":"`+wr.kind+`"`) {
				t.Errorf("after %s: frame %q, want a %s with seq %d", wr.r, first.data, wr.kind, seq)
			}
			for _, s := range socks[1:] {
				if f := s.next(t); !bytes.Equal(f.data, first.data) {
					t.Errorf("after %s: through %s %q, through hostthisd %q", wr.r, s.via, f.data, first.data)
				}
			}
		}

		// Ephemeral frames reach every peer byte for byte, never the sender.
		for i, sender := range socks {
			for _, typ := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
				msg := fmt.Appendf(nil, "ephemeral from %s %d", sender.via, typ)
				if typ == websocket.MessageBinary {
					msg = append(msg, 0, 0xff)
				}
				sender.send(t, typ, msg)
				for j, s := range socks {
					if i == j {
						continue
					}
					if f := s.next(t); f.typ != typ || !bytes.Equal(f.data, msg) {
						t.Errorf("ephemeral from %s through %s: %v %q, want %v %q", sender.via, s.via, f.typ, f.data, typ, msg)
					}
				}
			}
			sender.quiet(t)
		}

		// Another room of the app hears none of it.
		other := w.join(t, w.workers[0], host, w.createRoom(t, w.hostthisd, host))
		other.next(t)
		socks[0].send(t, websocket.MessageText, []byte("for this room only"))
		for _, s := range socks[1:] {
			s.next(t)
		}
		other.quiet(t)
	})

	t.Run("closes", func(t *testing.T) {
		cases := []struct {
			name  string
			frame string
			want  websocket.StatusCode
		}{
			{"reserved put", `{"type":"put","key":"k","value":1}`, websocket.StatusPolicyViolation},
			{"reserved snapshot", `{"type":"snapshot"}`, websocket.StatusPolicyViolation},
			{"reserved delete", `{"type": "delete"}`, websocket.StatusPolicyViolation},
			{"oversized", strings.Repeat("x", 32<<10+1), websocket.StatusMessageTooBig},
		}
		for _, c := range cases {
			codes := map[string]websocket.StatusCode{}
			for _, s := range append([]*server{w.hostthisd}, w.workers...) {
				sock := w.join(t, s, host, room)
				sock.next(t)
				sock.send(t, websocket.MessageText, []byte(c.frame))
				codes[s.name] = sock.closeCode(t)
			}
			for via, code := range codes {
				if code != c.want {
					t.Errorf("%s through %s closed %d, want %d", c.name, via, code, c.want)
				}
			}
		}
		// A frame at the limit is relayed.
		a, b := w.join(t, w.hostthisd, host, room), w.join(t, w.workers[0], host, room)
		a.next(t)
		b.next(t)
		at := strings.Repeat("y", 32<<10)
		a.send(t, websocket.MessageText, []byte(at))
		if f := b.next(t); string(f.data) != at {
			t.Errorf("a frame of exactly 32 KiB arrived as %d bytes", len(f.data))
		}
		b.send(t, websocket.MessageText, []byte(at))
		if f := a.next(t); string(f.data) != at {
			t.Errorf("a frame of exactly 32 KiB arrived as %d bytes", len(f.data))
		}
	})

	t.Run("socket cap", func(t *testing.T) { testSocketCap(t, w, host) })
}

// testSocketCap fills a room through each server in turn and joins once more.
func testSocketCap(t *testing.T, w *world, host string) {
	const roomCap = 64
	// Through hostthisd, its own admission refuses the next join before the
	// handshake, as the Room cell does on the Worker path.
	full := w.createRoom(t, w.hostthisd, host)
	for range roomCap {
		w.join(t, w.hostthisd, host, full).next(t)
	}
	w.expect(t, handshake(host, "/api/rooms/"+full+"/ws", "http://"+host), 429)

	// Through the Worker, hostthisd's admission has counted none of the
	// sockets, so it accepts its client before the Room cell refuses it.
	full = w.createRoom(t, w.hostthisd, host)
	for range roomCap {
		w.join(t, w.workers[0], host, full).next(t)
	}
	r := handshake(host, "/api/rooms/"+full+"/ws", "http://"+host)
	for _, s := range w.workers {
		if got := mustDo(t, s, r, 429); string(got.body) != "room connection limit reached\n\n" {
			t.Errorf("full room through %s: %q", s.name, got.body)
		}
	}
	sock := w.join(t, w.hostthisd, host, full)
	if code := sock.closeCode(t); code != websocket.StatusInternalError {
		t.Errorf("full room through hostthisd closed %d, want %d", code, websocket.StatusInternalError)
	}
	w.noteAllowance("full-room-closes-after-the-handshake")
}
