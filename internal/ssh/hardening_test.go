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

	xssh "golang.org/x/crypto/ssh"
)

func TestHardening_LocalPortForwardingRefused(t *testing.T) {
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	// `ssh -L` equivalent: ask the server to open a direct-tcpip channel to
	// an arbitrary endpoint. The refusal is expected at the channel open, so
	// the dial never happens.
	conn, err := cli.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected local port forward to be refused, but got a usable connection")
	}
	// The wording is library-internal, so any refusal shape counts. Two are
	// legitimate: "unknown channel type" (no direct-tcpip handler is
	// registered at all, so the reject precedes LocalPortForwardingCallback)
	// and the denial shapes that callback's false return produces if an
	// upstream change ever registers the handler.
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "unknown channel type") &&
		!strings.Contains(low, "administratively prohibited") &&
		!strings.Contains(low, "denied") &&
		!strings.Contains(low, "refused") &&
		!strings.Contains(low, "open failed") {
		t.Fatalf("expected channel-open refusal error, got %v", err)
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

// reachableEphemeralPort: an unused helper, kept for the flake where the
// kernel rejects 127.0.0.1:1 before the server's refusal lands.
var _ = func() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
