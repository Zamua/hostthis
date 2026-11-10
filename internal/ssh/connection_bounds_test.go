package ssh_test

import (
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

func closedWithin(cli *xssh.Client, d time.Duration) bool {
	done := make(chan struct{})
	go func() { _ = cli.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// A connection that carries nothing is closed after the idle timeout.
func TestConnectionBounds_IdleConnectionIsClosed(t *testing.T) {
	s := startStack(t, withConnBounds(300*time.Millisecond, 0))
	if !closedWithin(s.keyed, 5*time.Second) {
		t.Fatal("idle connection still open after the idle timeout")
	}
}

// A connection is closed at its lifetime limit even while it stays busy.
func TestConnectionBounds_BusyConnectionEndsAtTheLimit(t *testing.T) {
	s := startStack(t, withConnBounds(0, 1500*time.Millisecond))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				_, _, _ = s.keyed.SendRequest("keepalive@openssh.com", true, nil)
			}
		}
	}()
	if closedWithin(s.keyed, 700*time.Millisecond) {
		t.Fatal("busy connection closed before its lifetime limit")
	}
	if !closedWithin(s.keyed, 5*time.Second) {
		t.Fatal("busy connection still open after its lifetime limit")
	}
}

// Without limits a quiet connection stays open.
func TestConnectionBounds_UnsetLeavesConnectionsOpen(t *testing.T) {
	s := startStack(t)
	if closedWithin(s.keyed, 600*time.Millisecond) {
		t.Fatal("connection closed with no limits set")
	}
	if _, _, code := s.run("whoami", nil); code != 0 {
		t.Fatalf("whoami after a quiet spell: exit %d", code)
	}
}
