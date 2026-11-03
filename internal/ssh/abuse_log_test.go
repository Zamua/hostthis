package ssh_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/abuselog"
	"github.com/Zamua/hostthis/internal/service"
)

type abuseSink struct {
	mu    sync.Mutex
	lines []string
}

func (a *abuseSink) printf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, fmt.Sprintf(format, args...))
}

// await returns the lines once n have arrived: the session logs after the
// client has already seen its exit status.
func (a *abuseSink) await(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		got := append([]string(nil), a.lines...)
		a.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantSSHLine(t *testing.T, lines []string, event string, status int) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want one abuse line, got %q", lines)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not JSON: %q", lines[0])
	}
	if got["event"] != event || got["route"] != "ssh" || got["status"] != float64(status) || got["subnet"] != "127.0.0.0/24" || len(got) != 4 {
		t.Fatalf("line %v, want event=%s route=ssh status=%d subnet=127.0.0.0/24", got, event, status)
	}
	if strings.Contains(lines[0], "127.0.0.1") {
		t.Fatalf("line carries the full address: %q", lines[0])
	}
}

func TestAbuseLog_SybilRefusalIsRateLimited(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withKeyGate(2), withAbuse(abuselog.New(sink.printf)))
	for range 2 {
		c, _ := newKeyClient(t, g.sshAddr)
		if _, _, e := g.runOn(c, "whoami", nil); e != 0 {
			t.Fatalf("admitted key: exit %d", e)
		}
	}
	c3, _ := newKeyClient(t, g.sshAddr)
	if _, _, e := g.runOn(c3, "whoami", nil); e != 6 {
		t.Fatalf("third key: exit %d, want 6", e)
	}
	lines := sink.await(t, 3)
	if len(lines) != 3 {
		t.Fatalf("want two creations then a refusal, got %q", lines)
	}
	wantSSHLine(t, lines[2:], "rate-limited", 6)
}

func TestAbuseLog_BannedNetworkIsAnAuthFailure(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withDenylist(), withAbuse(abuselog.New(sink.printf)))
	g.denylistOp("add", `{"entry":"127.0.0.0/8","reason":""}`)
	c, _ := newKeyClient(t, g.sshAddr)
	if _, _, e := g.runOn(c, "whoami", nil); e != 7 {
		t.Fatalf("exit %d, want 7", e)
	}
	wantSSHLine(t, sink.await(t, 1), "auth-failure", 7)
}

func TestAbuseLog_KeylessSessionIsAnAuthFailure(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withKeyGate(5), withAbuse(abuselog.New(sink.printf)))
	if _, _, e := g.runAnon("whoami", nil); e != 3 {
		t.Fatalf("exit %d, want 3", e)
	}
	wantSSHLine(t, sink.await(t, 1), "auth-failure", 3)
}

func TestAbuseLog_RefusedUploadIsRejected(t *testing.T) {
	sink := &abuseSink{}
	f := &fakeScreener{deny: denyContaining("EVIL")}
	g := startStack(t, withScreen(&service.ScreenGate{Screener: f}), withAbuse(abuselog.New(sink.printf)))
	if _, _, e := g.run("", []byte("<!doctype html><h1>EVIL</h1>")); e != 8 {
		t.Fatalf("exit %d, want 8", e)
	}
	wantSSHLine(t, sink.await(t, 1), "rejected", 8)
}

func TestAbuseLog_NormalSessionsLogNothing(t *testing.T) {
	sink := &abuseSink{}
	logged := &syncBuffer{}
	g := startStack(t, withKeyGate(5), withAbuse(abuselog.New(sink.printf)), withLogOutput(logged))
	if _, _, e := g.run("", []byte("<!doctype html><p>hi</p>")); e != 0 {
		t.Fatalf("upload exit %d", e)
	}
	if _, _, e := g.run("list", nil); e != 0 {
		t.Fatalf("list exit %d", e)
	}
	// The command line is logged after the session, so its arrival means the
	// abuse hook has already run for both.
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logged.String(), "ssh: command") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Only the key's first session, its creation, is logged.
	wantSSHLine(t, sink.await(t, 2), "created", 0)
}

// A key new to its subnet is a creation event at admission, status 0: the
// session has not exited yet. A returning key and a refused key are not.
func TestAbuseLog_NewKeyOnASubnetIsACreation(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withKeyGate(2), withAbuse(abuselog.New(sink.printf)))
	_, cfg := freshKeyConfig(t)
	dial := func() *xssh.Client {
		cli, err := xssh.Dial("tcp", g.sshAddr, cfg)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = cli.Close() })
		return cli
	}
	if _, _, e := g.runOn(dial(), "whoami", nil); e != 0 {
		t.Fatalf("first session: exit %d", e)
	}
	wantSSHLine(t, sink.await(t, 1), "created", 0)

	// The same key again: no second line.
	if _, _, e := g.runOn(dial(), "whoami", nil); e != 0 {
		t.Fatalf("returning session: exit %d", e)
	}
	if got := sink.await(t, 2); len(got) != 1 {
		t.Fatalf("returning key logged: %q", got)
	}
}

func TestAbuseLog_RefusedKeyIsNotACreation(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withKeyGate(1), withAbuse(abuselog.New(sink.printf)))
	c1, _ := newKeyClient(t, g.sshAddr)
	g.runOn(c1, "whoami", nil)
	c2, _ := newKeyClient(t, g.sshAddr)
	if _, _, e := g.runOn(c2, "whoami", nil); e != 6 {
		t.Fatalf("second key: exit %d, want 6", e)
	}
	lines := sink.await(t, 2)
	if len(lines) != 2 || !strings.Contains(lines[0], `"event":"created"`) || !strings.Contains(lines[1], `"event":"rate-limited"`) {
		t.Fatalf("lines %q, want created then rate-limited", lines)
	}
}

// Property: behind the PROXY protocol, whatever the client address, no line
// spells it and the subnet is the /24 or /40 of it.
func TestAbuseLog_ProxiedAddressNeverReachesALine(t *testing.T) {
	sink := &abuseSink{}
	g := startStack(t, withProxyProto(), withKeyGate(100), withAbuse(abuselog.New(sink.printf)))
	for _, tc := range []struct{ family, ip, subnet string }{
		{"TCP4", "203.0.113.77", "203.0.113.0/24"},
		{"TCP4", "198.51.100.200", "198.51.100.0/24"},
		{"TCP6", "2001:db8:abcd:1234::77", "2001:db8:ab00::/40"},
		{"TCP6", "2a0f:5707:ac12:34ab:cdef:1:2:3", "2a0f:5707:ac00::/40"},
	} {
		cli := dialWithProxy(t, g.sshAddr, tc.family, tc.ip, 40000)
		if _, _, e := g.runOn(cli, "whoami", nil); e != 0 {
			t.Fatalf("%s: exit %d", tc.ip, e)
		}
		lines := sink.await(t, 1)
		line := lines[len(lines)-1]
		if !strings.Contains(line, `"subnet":"`+tc.subnet+`"`) {
			t.Fatalf("%s: line %q, want subnet %s", tc.ip, line, tc.subnet)
		}
		for _, l := range lines {
			if strings.Contains(l, tc.ip) {
				t.Fatalf("line %q carries the address %s", l, tc.ip)
			}
		}
		sink.mu.Lock()
		sink.lines = nil
		sink.mu.Unlock()
	}
}
