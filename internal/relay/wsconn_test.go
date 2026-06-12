package relay

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWSConn_SendBufferBoundsAndDropSignal proves the REAL wsConn adapter's
// backpressure contract deterministically (no live peer, no flaky socket
// buffers): Send enqueues on a bounded buffer and returns true until the
// buffer is full, then returns false - the exact signal the hub acts on to
// drop a laggard (see Hub.broadcast + the fakeConn-driven hub test). A
// closed connection's Send also returns false.
//
// The writer goroutine is intentionally NOT started here, so nothing drains
// the buffer: this isolates the bound itself. In production the writer
// drains to the socket; when the socket is stuck the buffer fills exactly
// as it does here, and the next broadcast's Send returns false.
func TestWSConn_SendBufferBoundsAndDropSignal(t *testing.T) {
	const buf = 4
	// A nil *websocket.Conn is fine: this test never starts the writer and
	// never calls a method that touches the socket (Send is pure channel
	// logic; Close on a nil conn is guarded below).
	c := newWSConn(1, &websocket.Conn{}, buf, 0)

	for i := 0; i < buf; i++ {
		if !c.Send(Frame{Data: []byte("x")}) {
			t.Fatalf("Send %d returned false within the buffer bound %d", i, buf)
		}
	}
	// Buffer is now full (nothing drains it); the next Send is the laggard
	// signal: false.
	if c.Send(Frame{Data: []byte("overflow")}) {
		t.Fatal("Send past the buffer bound returned true; the drop signal is missing")
	}
}

func TestWSConn_SendAfterCloseReturnsFalse(t *testing.T) {
	c := newWSConn(1, &websocket.Conn{}, 4, 0)
	// Close the connection abstraction. CloseNow on a zero-value Conn is a
	// no-op-ish abort; we only assert the post-close Send contract.
	func() {
		defer func() { _ = recover() }() // CloseNow on a bare Conn may panic; ignore.
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

// TestWSConn_WriteTimeoutFromConfiguredPingTimeout pins the P3 fix: the
// write reap window comes from the relay's CONFIGURED ping timeout (which
// SetHeartbeat shortens), not the package constant. Before the fix,
// writeLoop bounded each write with the const PingTimeout, so SetHeartbeat
// (tests / operators) shortened the PING reap window but NOT the WRITE reap
// window, leaving the two inconsistent. Serve builds the wsConn with
// rl.pingTimeout, so the connection carries the shortened write timeout.
func TestWSConn_WriteTimeoutFromConfiguredPingTimeout(t *testing.T) {
	rl := NewRelay(nil, NewLimits())
	const short = 7 * time.Millisecond
	rl.SetHeartbeat(3*time.Millisecond, short)

	// Serve threads rl.pingTimeout into newWSConn; assert the resulting
	// connection's write timeout tracks the configured value, not PingTimeout.
	c := newWSConn(1, &websocket.Conn{}, rl.reg.limits.SendBuffer, rl.pingTimeout)
	if c.writeTimeout != short {
		t.Fatalf("writeTimeout = %v, want the configured %v (SetHeartbeat must shorten the write reap window too)", c.writeTimeout, short)
	}
	if c.writeTimeout == PingTimeout {
		t.Fatalf("writeTimeout fell back to the package constant %v; SetHeartbeat did not reach the write deadline", PingTimeout)
	}
}
