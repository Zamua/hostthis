package ssh_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

func (s *stack) denylistOp(op, body string) {
	s.t.Helper()
	res, err := http.Post(s.endpoint+"/denylist/"+op, "application/json", bytes.NewBufferString(body))
	if err != nil || res.StatusCode != http.StatusOK {
		s.t.Fatalf("denylist %s %s: %v %v", op, body, res, err)
	}
	_ = res.Body.Close()
	if err := s.denyGate.Refresh(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

// A banned key is refused with exit 7 before the Sybil gate records it.
func TestDenylist_BannedKeyExits7AndLeavesNoKeygateRow(t *testing.T) {
	g := startStack(t, withKeyGate(5), withDenylist())
	g.denylistOp("add", `{"entry":"`+g.keyedOwner+`","reason":"probing a third party"}`)
	_, stderr, code := g.run("whoami", nil)
	if code != 7 {
		t.Fatalf("exit %d (%q), want 7", code, stderr)
	}
	for _, want := range []string{"this key or network is banned from hostthis", "reason: probing a third party"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q lacks %q", stderr, want)
		}
	}
	n, _, err := g.keyGate.Repo.SubnetSnapshot("127.0.0.0/24", time.Now(), 24*time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("keygate rows after a banned session: %d, %v; want 0", n, err)
	}
}

// A network entry refuses any key from inside it; removing it readmits.
func TestDenylist_NetworkBanCoversEveryKeyUntilRemoved(t *testing.T) {
	g := startStack(t, withDenylist())
	g.denylistOp("add", `{"entry":"127.0.0.0/8","reason":""}`)
	c, _ := newKeyClient(t, g.sshAddr)
	_, stderr, code := g.runOn(c, "whoami", nil)
	if code != 7 || strings.Contains(stderr, "reason:") {
		t.Fatalf("exit %d (%q), want 7 with no reason line", code, stderr)
	}
	g.denylistOp("remove", `{"entry":"127.0.0.0/8"}`)
	if _, stderr, code := g.runOn(c, "whoami", nil); code != 0 {
		t.Fatalf("after removal: exit %d (%q), want 0", code, stderr)
	}
}

// Keyless sessions get the key-required refusal, never the ban message.
func TestDenylist_KeylessSessionStillGetsKeyRequired(t *testing.T) {
	g := startStack(t, withDenylist())
	g.denylistOp("add", `{"entry":"127.0.0.0/8","reason":""}`)
	if _, stderr, code := g.runAnon("whoami", nil); code != 3 || !strings.Contains(stderr, "ssh key required") {
		t.Fatalf("anon: exit %d (%q), want 3 key required", code, stderr)
	}
}

// With an allow-list, only a listed key or a session from a listed network
// gets in.
func TestAllowList_AdmitsListedKeysAndNetworksOnly(t *testing.T) {
	g := startStack(t, withAllow("203.0.113.0/24"))
	if _, stderr, code := g.run("whoami", nil); code != 7 || !strings.Contains(stderr, "not on its allow-list") {
		t.Fatalf("unlisted key: exit %d %q, want 7", code, stderr)
	}
	g = startStack(t, withAllow("127.0.0.0/8"))
	if _, stderr, code := g.run("whoami", nil); code != 0 {
		t.Fatalf("listed network: exit %d %q, want 0", code, stderr)
	}
	signer, cfg := freshKeyConfig(t)
	g = startStack(t, withAllow(fingerprintSigner(signer.PublicKey())))
	cli, err := xssh.Dial("tcp", g.sshAddr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, stderr, code := g.runOn(cli, "whoami", nil); code != 0 {
		t.Fatalf("listed key: exit %d %q, want 0", code, stderr)
	}
	if _, _, code := g.run("whoami", nil); code != 7 {
		t.Fatalf("the stack's own unlisted key: exit %d, want 7", code)
	}
}
