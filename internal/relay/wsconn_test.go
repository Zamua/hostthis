package relay

import (
	"testing"
	"time"

	"github.com/coder/websocket"
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

// TestWSConn_WriteTimeoutFromConfiguredPingTimeout pins that the write reap
// window comes from the relay's CONFIGURED ping timeout, which SetHeartbeat
// shortens, not the package constant. Bounding each write with the const would
// leave the PING and WRITE reap windows inconsistent whenever SetHeartbeat is
// used.
func TestWSConn_WriteTimeoutFromConfiguredPingTimeout(t *testing.T) {
	rl := NewRelay(nil, NewLimits())
	const short = 7 * time.Millisecond
	rl.SetHeartbeat(3*time.Millisecond, short)

	// Serve threads rl.pingTimeout into newWSConn.
	c := newWSConn(1, &websocket.Conn{}, rl.reg.limits.SendBuffer, rl.pingTimeout)
	if c.writeTimeout != short {
		t.Fatalf("writeTimeout = %v, want the configured %v (SetHeartbeat must shorten the write reap window too)", c.writeTimeout, short)
	}
	if c.writeTimeout == PingTimeout {
		t.Fatalf("writeTimeout fell back to the package constant %v; SetHeartbeat did not reach the write deadline", PingTimeout)
	}
}
