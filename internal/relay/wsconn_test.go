package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
)

// TestWSConn_SendBufferBoundsAndDropSignal pins the real wsConn adapter's
// backpressure contract: Send enqueues on a bounded buffer and returns true
// until the buffer is full, then returns false, the exact signal the hub acts
// on to drop a laggard.
//
// The writer goroutine is deliberately NOT started, so nothing drains the
// buffer: that isolates the bound itself. In production the writer drains to
// the socket, and a stuck socket fills the buffer exactly as here.
func TestWSConn_SendBufferBoundsAndDropSignal(t *testing.T) {
	const buf = 4
	// A zero-value Conn is fine: the test never starts the writer and never
	// calls a method that touches the socket (Send is pure channel logic).
	c := newWSConn(1, &websocket.Conn{}, buf, 0)

	for i := range buf {
		if !c.Send(Frame{Data: []byte("x")}) {
			t.Fatalf("Send %d returned false within the buffer bound %d", i, buf)
		}
	}
	// Buffer full and nothing draining: the next Send is the laggard signal.
	if c.Send(Frame{Data: []byte("overflow")}) {
		t.Fatal("Send past the buffer bound returned true; the drop signal is missing")
	}
}

func TestWSConn_SendAfterCloseReturnsFalse(t *testing.T) {
	c := newWSConn(1, &websocket.Conn{}, 4, 0)
	// Only the post-close Send contract is under test; CloseNow on a bare Conn
	// may panic, which the recover absorbs.
	func() {
		defer func() { _ = recover() }()
		c.Close()
	}()
	if c.Send(Frame{Data: []byte("x")}) {
		t.Fatal("Send on a closed connection returned true; should be false")
	}
}

// TestWSConn_IDStable confirms the connection id is the stable hub map key.
func TestWSConn_IDStable(t *testing.T) {
	c := newWSConn(42, &websocket.Conn{}, 1, 0)
	if c.ID() != 42 {
		t.Fatalf("ID() = %d, want 42", c.ID())
	}
}

// TestServe_WriteTimeoutFromConfiguredPingTimeout pins that the write reap
// window comes from the relay's CONFIGURED ping timeout, which SetHeartbeat
// shortens, not the package constant. Bounding each write with the const would
// leave the PING and WRITE reap windows inconsistent whenever SetHeartbeat is
// used.
//
// Driven through Serve over a real accepted websocket, because the wiring IS
// the claim: constructing a wsConn from rl.pingTimeout in the test only proves
// newWSConn stores its fourth argument, and stays green with Serve passing the
// package constant instead.
func TestServe_WriteTimeoutFromConfiguredPingTimeout(t *testing.T) {
	rl := NewRelay(emptySnapshotter{}, NewLimits())
	// The interval is long enough that no ping fires during the test; the
	// timeout is the value under test and must differ from PingTimeout.
	const want = 1234 * time.Millisecond
	rl.SetHeartbeat(time.Hour, want)
	if want == PingTimeout {
		t.Fatal("fixture is degenerate: the configured timeout must differ from the package constant")
	}

	key := RoomKey{App: domain.Slug("appz2345"), ID: domain.RoomID("11111111-2222-3333-4444-555555555555")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, err := rl.Admit(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			rl.Release(key, id)
			return
		}
		rl.Serve(r.Context(), key, id, ws)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cli, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.CloseNow() //nolint:errcheck

	got := waitForServedConn(t, rl, key)
	if got.writeTimeout != want {
		t.Fatalf("writeTimeout = %v, want the configured %v (SetHeartbeat must move the write reap window too)", got.writeTimeout, want)
	}
	if got.writeTimeout == PingTimeout {
		t.Fatalf("writeTimeout fell back to the package constant %v; SetHeartbeat did not reach the write deadline", PingTimeout)
	}
}

// emptySnapshotter is the durable tier Serve's late-join step reads. An empty
// room is enough: this pins the connection's wiring, not the snapshot content.
type emptySnapshotter struct{}

func (emptySnapshotter) Scan(domain.Slug, domain.RoomID) (domain.RoomKV, error) {
	return domain.NewRoomKV(), nil
}

// waitForServedConn blocks until Serve has swapped its admission reservation
// for the real connection in key's hub.
func waitForServedConn(t *testing.T, rl *Relay, key RoomKey) *wsConn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hub := rl.reg.hub(key); hub != nil {
			hub.mu.Lock()
			var found *wsConn
			for _, c := range hub.conns {
				if wc, ok := c.(*wsConn); ok {
					found = wc
				}
			}
			hub.mu.Unlock()
			if found != nil {
				return found
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Serve never registered a real wsConn in the hub")
	return nil
}
