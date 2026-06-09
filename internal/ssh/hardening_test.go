package ssh_test

// Pins the session-hardening refusal behavior:
//   - port-forward (-L) is refused at the channel open
//   - reverse port-forward (-R) is refused at the global-request
//   - subsystem requests (sftp / scp / arbitrary) are refused
//   - normal verb sessions are unaffected by the hardening
//
// The test exercises the real wish/charmbracelet server through the
// real golang.org/x/crypto/ssh client so a future upstream default flip
// (or an accidental hardening regression) lights this test red.

import (
	"net"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func TestHardening_LocalPortForwardingRefused(t *testing.T) {
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	// Client-side `ssh -L` equivalent: open a direct-tcpip channel by
	// asking the server to dial out to an arbitrary tcp endpoint.
	// We never actually want it to dial; we expect the server to refuse
	// the channel open at the protocol layer.
	conn, err := cli.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected local port forward to be refused, but got a usable connection")
	}
	// Error message comes from the server's channel-open reject. The
	// exact wording is library-internal; we just assert refusal at the
	// channel layer. There are two valid refusal shapes:
	//   - "unknown channel type": the wish server's default channel-
	//     handler map has no "direct-tcpip" entry, so the channel-open
	//     is rejected before our LocalPortForwardingCallback runs.
	//     This is a stronger refusal than the callback (the handler
	//     itself doesn't exist) and is the actual behavior today.
	//   - "administratively prohibited" / "denied" / "open failed":
	//     the shapes the callback's `false` return produces if a
	//     future upstream change registers the direct-tcpip handler
	//     and consults the callback. We accept any of them so the
	//     test stays meaningful across library updates.
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

	// Client-side `ssh -R` equivalent: ask the server to start
	// listening on its side and forward back. ReversePortForwardingCallback
	// returns false so the server replies with a denial.
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

	// Try the canonical sftp subsystem. The server's
	// SessionRequestCallback refuses any "subsystem" request, so the
	// client's RequestSubsystem returns an error rather than starting
	// a subsystem stream.
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Fatalf("expected subsystem request to be refused, got nil error")
	}
}

func TestHardening_AgentForwardRequestIsNoop(t *testing.T) {
	// Agent forwarding requests (auth-agent-req@openssh.com) are
	// acknowledged by the library but hostthis never sets up the
	// forwarding socket, so the request is functionally a no-op. We
	// don't have a clean assertion for "no forwarding socket exists"
	// without poking the agent extension; instead we pin the
	// invariant that sending the request does not break the session
	// (the verb still runs and exits cleanly).
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	if _, err := sess.SendRequest("auth-agent-req@openssh.com", true, nil); err != nil {
		// SendRequest itself shouldn't error; if it does the agent
		// path was broken in a way the test cares about.
		t.Fatalf("agent-req SendRequest: %v", err)
	}
	// The session still runs whoami correctly. The verb-output details
	// are covered elsewhere; we only assert the run succeeds.
	if err := sess.Run("whoami"); err != nil {
		t.Fatalf("verb run after agent-req: %v", err)
	}
}

func TestHardening_NormalVerbSessionStillWorks(t *testing.T) {
	// Sanity: the hardening doesn't accidentally block the verb path.
	// `whoami` is the cheapest verb to exercise: no body, no slug.
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
	// The library doesn't have an x11-req handler in its switch, so
	// any x11-req falls to the default case which replies(false).
	// We send the raw request and assert the server rejects it.
	s := startStack(t)
	cli, _ := newKeyClient(t, s.sshAddr)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	// Payload shape per RFC 4254 §6.3.1; the values are placeholders
	// because the request is expected to be refused before the server
	// looks at the body.
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

// reachableEphemeralPort guards the local-forward test against the
// edge case where 127.0.0.1:1 isn't accepted by the kernel before the
// server's refusal lands. Not currently used, but kept here so a
// future flake-fix has the helper.
var _ = func() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
