package ssh_test

import (
	"net"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// A connection that drops mid-upload must not save what arrived so far: the
// channel reads as EOF either way, and only a live connection proves the
// client ended its input.
func TestDroppedConnectionDoesNotSaveTruncatedUpdate(t *testing.T) {
	// One upload slot per key, so the follow-up update runs only once the
	// dropped one has finished, and its version number shows whether the
	// dropped one was saved.
	s := startStack(t, withUploads(mustAdmission(t, 1, 30*time.Second)))
	_, cfg := freshKeyConfig(t)
	dial := func() (net.Conn, *xssh.Client) {
		raw, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c, chans, reqs, err := xssh.NewClientConn(raw, s.sshAddr, cfg)
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		cli := xssh.NewClient(c, chans, reqs)
		t.Cleanup(func() { _ = cli.Close() })
		return raw, cli
	}

	_, owner := dial()
	out, errOut, code := s.runOn(owner, "", []byte("# first\n\nwhole body\n"))
	if code != 0 {
		t.Fatalf("create: exit %d: %s", code, errOut)
	}
	slug := extractSlug(out)

	raw, dropped := dial()
	sess, err := dropped.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	if err := sess.Start(slug); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := stdin.Write([]byte("# second\n\nhalf of a bo")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the partial body reach the server
	_ = raw.Close()

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, errOut, code = s.runOn(owner, slug, []byte("# third\n\nwhole body\n"))
		if !strings.Contains(errOut, "already running") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code != 0 {
		t.Fatalf("update: exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "v2 saved.") {
		t.Fatalf("the dropped upload was saved as a version; follow-up said %q", errOut)
	}
}
