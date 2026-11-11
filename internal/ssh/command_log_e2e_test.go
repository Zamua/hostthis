package ssh_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestEverySessionLogsItsCommandWithoutPayloadOrLabel(t *testing.T) {
	logged := &syncBuffer{}
	s := startStack(t, withLogOutput(logged))
	out, _, code := s.run("--name secretlabel", []byte("<!doctype html><p>private body</p>"))
	if code != 0 {
		t.Fatalf("upload exit %d", code)
	}
	slug := extractSlug(out)
	if _, _, code := s.run("versions "+slug, nil); code != 0 {
		t.Fatalf("versions exit %d", code)
	}

	want := "ssh: command verb=versions slug=" + slug + " outcome=ok"
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logged.String(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logged.String()
	if !strings.Contains(got, want) || !strings.Contains(got, "ssh: command verb=upload slug=- outcome=ok") {
		t.Fatalf("log = %q, want an upload line and %q", got, want)
	}
	if strings.Contains(got, "secretlabel") || strings.Contains(got, "private body") {
		t.Fatalf("log = %q, carries a label or payload", got)
	}
}

// The session line says who ran the command and from where: the short hash of
// the key, how the session logged in, and the client's address and subnet. A
// session that offered a key without signing and logged in by password has no
// key hash and names its method.
func TestSessionLineNamesKeyMethodAndAddress(t *testing.T) {
	logged := &syncBuffer{}
	s := startStack(t, withLogOutput(logged))
	if _, _, code := s.run("whoami", nil); code != 0 {
		t.Fatalf("whoami exit %d", code)
	}
	if _, _, code := s.runAnon("whoami", nil); code != 3 {
		t.Fatalf("keyless whoami exit %d, want 3", code)
	}
	sum := sha256.Sum256([]byte("key:" + s.keyedOwner))
	keyed := "key=" + hex.EncodeToString(sum[:8]) + " auth=publickey addr=127.0.0.1 subnet=127.0.0.0/24"
	keyless := "outcome=error_3 ms="
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logged.String(), "ssh: command") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logged.String()
	if !strings.Contains(got, keyed) {
		t.Fatalf("log = %q, want a line with %q", got, keyed)
	}
	if !strings.Contains(got, keyless) || !strings.Contains(got, "key=- auth=password addr=127.0.0.1 subnet=127.0.0.0/24") {
		t.Fatalf("log = %q, want a keyless password line", got)
	}
}

// A key's first session on a network leaves the address it came from on the
// identity's record, beside the subnet.
func TestNewKeyRecordsTheAddressItCameFrom(t *testing.T) {
	s := startStack(t, withKeyGate(5))
	if _, _, code := s.run("whoami", nil); code != 0 {
		t.Fatalf("whoami exit %d", code)
	}
	target := fmt.Sprintf("%s/identity/subnetlist?scope=%s&now=%d", s.endpoint,
		url.QueryEscape("key:"+s.keyedOwner), time.Now().UnixMilli())
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var got struct {
		Subnets []struct{ Subnet string } `json:"subnets"`
		Addrs   []struct{ Addr string }   `json:"addrs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	if len(got.Addrs) != 1 || got.Addrs[0].Addr != "127.0.0.1" || len(got.Subnets) != 1 || got.Subnets[0].Subnet != "127.0.0.0/24" {
		t.Fatalf("record = %+v, want addr 127.0.0.1 on 127.0.0.0/24", got)
	}
}
