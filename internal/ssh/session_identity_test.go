package ssh_test

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// offeredOnly presents a public key and cannot sign for it.
type offeredOnly struct{ pub xssh.PublicKey }

func (o offeredOnly) PublicKey() xssh.PublicKey { return o.pub }
func (o offeredOnly) Sign(io.Reader, []byte) (*xssh.Signature, error) {
	return nil, errors.New("no private key")
}

// offerThenSwitch names one key in the unsigned query and another in the
// signed request, which it signs with the second key's private half.
type offerThenSwitch struct {
	xssh.PublicKey // the key that signs
	offered        xssh.PublicKey
	with           xssh.Signer
	marshals       atomic.Int32
}

// The client marshals the key twice for the query (the request, then the
// check of the server's acknowledgement) and once more to sign.
func (o *offerThenSwitch) Marshal() []byte {
	if o.marshals.Add(1) <= 2 {
		return o.offered.Marshal()
	}
	return o.PublicKey.Marshal()
}

type switchSigner struct{ key *offerThenSwitch }

func (s switchSigner) PublicKey() xssh.PublicKey { return s.key }
func (s switchSigner) Sign(r io.Reader, data []byte) (*xssh.Signature, error) {
	return s.key.with.Sign(r, data)
}

func dialAuth(t *testing.T, addr string, auth ...xssh.AuthMethod) *xssh.Client {
	t.Helper()
	cli, err := xssh.Dial("tcp", addr, &xssh.ClientConfig{
		User:            "anyone",
		Auth:            auth,
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// A session has the identity of the key that authenticated it, and none when
// no key did, whatever keys the client offered on the way.
func TestSessionIdentity_ComesFromTheVerifiedKey(t *testing.T) {
	g := startStack(t)
	other, _ := freshKeyConfig(t)
	ownerSigner, ownerCfg := freshKeyConfig(t)
	owner, err := xssh.Dial("tcp", g.sshAddr, ownerCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	url, stderr, code := g.runOn(owner, "", []byte("# owned\n"))
	if code != 0 {
		t.Fatalf("owner paste: exit %d %q", code, stderr)
	}
	slug := extractSlug(url)
	ownerPub := ownerSigner.PublicKey()

	keyless := map[string]xssh.AuthMethod{
		"password": xssh.Password("anything"),
		"keyboard-interactive": xssh.KeyboardInteractive(
			func(string, string, []string, []bool) ([]string, error) { return nil, nil }),
	}
	for name, method := range keyless {
		t.Run("offered then "+name, func(t *testing.T) {
			cli := dialAuth(t, g.sshAddr, xssh.PublicKeys(offeredOnly{ownerPub}), method)
			for _, cmd := range []string{"whoami", "list", "rm " + slug} {
				stdout, stderr, code := g.runOn(cli, cmd, nil)
				if code != 3 || !strings.Contains(stderr, "ssh key required") {
					t.Fatalf("%s: exit %d stdout %q stderr %q, want 3 key required", cmd, code, stdout, stderr)
				}
			}
		})
	}

	t.Run("offered then another key", func(t *testing.T) {
		key := &offerThenSwitch{PublicKey: other.PublicKey(), offered: ownerPub, with: other}
		cli := dialAuth(t, g.sshAddr, xssh.PublicKeys(switchSigner{key}))
		if n := key.marshals.Load(); n != 3 {
			t.Fatalf("client marshalled the key %d time(s), want 3: the offer may not have named the other key", n)
		}
		stdout, stderr, code := g.runOn(cli, "whoami", nil)
		if code != 0 || !strings.Contains(stdout, fingerprintSigner(other.PublicKey())) {
			t.Fatalf("whoami: exit %d stdout %q stderr %q, want the signing key", code, stdout, stderr)
		}
		if _, _, code := g.runOn(cli, "rm "+slug, nil); code == 0 {
			t.Fatal("rm of the offered key's paste succeeded")
		}
	})

	stdout, stderr, code := g.runOn(owner, "list", nil)
	if code != 0 || !strings.Contains(stdout, slug) {
		t.Fatalf("owner list: exit %d stdout %q stderr %q, want %s", code, stdout, stderr, slug)
	}
}
