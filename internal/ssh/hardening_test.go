package ssh_test

// Pins the session-hardening refusals: port-forward (-L) at the channel open,
// reverse port-forward (-R) at the global request, subsystem requests (sftp /
// scp / arbitrary), and x11-req - with normal verb sessions unaffected.
//
// Driven through the real wish server and the real golang.org/x/crypto/ssh
// client, so an upstream default flip lights this red too.

import (
	"net"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

func TestHardening_LocalPortForwardingRefused(t *testing.T) {
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	// The forward target is a listener the TEST owns and keeps accepting on.
	// That is what makes the refusal observable: aiming at a closed port
	// instead, a forward that WORKED would fail the dial and report the same
	// "open failed" / "connection refused" the refusal produces, so no
	// assertion on the error can tell the two apart.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("forward target listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	accepts := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts <- c
		}
	}()

	// Control: the target really is reachable, so a working forward would
	// connect. Without this the test cannot distinguish a refusal from a
	// target that was never dialable.
	direct, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("control dial to the forward target failed: %v", err)
	}
	_ = direct.Close()
	select {
	case c := <-accepts:
		_ = c.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("control dial never reached the listener; this test cannot tell a refusal from an unreachable target")
	}

	// `ssh -L` equivalent: ask the server to open a direct-tcpip channel to
	// that endpoint. The refusal is expected at the channel open, so the
	// server-side dial never happens.
	conn, err := cli.Dial("tcp", ln.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected local port forward to be refused, but got a usable connection to a reachable listener")
	}
	select {
	case c := <-accepts:
		_ = c.Close()
		t.Fatalf("the server dialed the forward target, so -L was not refused at the channel open (client err: %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestHardening_ReversePortForwardingRefused(t *testing.T) {
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	// `ssh -R` equivalent: ask the server to listen on its side and forward
	// back. ReversePortForwardingCallback returns false, so it denies.
	ln, err := cli.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = ln.Close()
		t.Fatalf("expected reverse port forward to be refused, but the server accepted the listen request")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "denied") &&
		!strings.Contains(strings.ToLower(err.Error()), "refused") &&
		!strings.Contains(strings.ToLower(err.Error()), "tcpip-forward") {
		t.Fatalf("expected reverse-forward denial error, got %v", err)
	}
}

func TestHardening_SubsystemRefused(t *testing.T) {
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	// SessionRequestCallback refuses any "subsystem" request, so
	// RequestSubsystem errors instead of starting a stream.
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Fatalf("expected subsystem request to be refused, got nil error")
	}
}

func TestHardening_AgentForwardRequestIsNoop(t *testing.T) {
	// The library acknowledges auth-agent-req@openssh.com but hostthis never
	// sets up a forwarding socket, so the request is a no-op. Asserting "no
	// socket exists" needs the agent extension; the reachable invariant is
	// that the request does not break the session.
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	if _, err := sess.SendRequest("auth-agent-req@openssh.com", true, nil); err != nil {
		t.Fatalf("agent-req SendRequest: %v", err)
	}
	if err := sess.Run("whoami"); err != nil {
		t.Fatalf("verb run after agent-req: %v", err)
	}
}

func TestHardening_NormalVerbSessionStillWorks(t *testing.T) {
	// The hardening must not block the verb path. `whoami` is the cheapest
	// verb: no body, no slug.
	s := startStack(t)
	stdout, stderr, exit := s.run("whoami", nil)
	if exit != 0 {
		t.Fatalf("whoami should still work post-hardening: exit %d stderr %q", exit, stderr)
	}
	if !strings.Contains(stdout, "key:") {
		t.Fatalf("whoami output should include the key line, got %q", stdout)
	}
}

func TestHardening_X11RequestRefused(t *testing.T) {
	// The library has no x11-req handler, so the request falls to the
	// default case, which replies false.
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	// Payload shape per RFC 4254 6.3.1; the values are placeholders, since
	// the refusal lands before the server reads the body.
	payload := xssh.Marshal(struct {
		SingleConnection bool
		AuthProtocol     string
		AuthCookie       string
		ScreenNumber     uint32
	}{
		SingleConnection: false,
		AuthProtocol:     "MIT-MAGIC-COOKIE-1",
		AuthCookie:       "00",
		ScreenNumber:     0,
	})
	ok, err := sess.SendRequest("x11-req", true, payload)
	if err != nil {
		t.Fatalf("x11-req SendRequest: %v", err)
	}
	if ok {
		t.Fatalf("x11-req should be refused, but server replied ok=true")
	}
}
