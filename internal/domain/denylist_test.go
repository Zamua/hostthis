package domain

import (
	"net/netip"
	"strings"
	"testing"
)

var bannedKey = "SHA256:" + strings.Repeat("ab", 32)

func TestDenylist_MatchesKeyAnywhereAndNetworkForAnyKey(t *testing.T) {
	d := NewDenylist([]DenyEntry{
		{Entry: bannedKey, Reason: "abuse"},
		{Entry: "203.0.113.0/24", Reason: "hostile network"},
		{Entry: "2001:db8:aa::/48"},
	})
	other := "SHA256:" + strings.Repeat("cd", 32)
	cases := []struct {
		fp, ip     string
		banned     bool
		wantReason string
	}{
		{bannedKey, "198.51.100.7", true, "abuse"},
		{other, "203.0.113.200", true, "hostile network"},
		{other, "::ffff:203.0.113.9", true, "hostile network"},
		{other, "2001:db8:aa:ffff::1", true, ""},
		{other, "203.0.114.1", false, ""},
		{other, "2001:db8:ab::1", false, ""},
	}
	for _, c := range cases {
		reason, banned := d.Match(c.fp, netip.MustParseAddr(c.ip))
		if banned != c.banned || reason != c.wantReason {
			t.Errorf("Match(%s, %s) = %q, %v; want %q, %v", c.fp[:10], c.ip, reason, banned, c.wantReason, c.banned)
		}
	}
}

func TestDenylist_KeyMatchWinsWhenBothMatch(t *testing.T) {
	d := NewDenylist([]DenyEntry{{Entry: "203.0.113.0/24", Reason: "net"}, {Entry: bannedKey, Reason: "key"}})
	if reason, _ := d.Match(bannedKey, netip.MustParseAddr("203.0.113.1")); reason != "key" {
		t.Fatalf("reason = %q, want the key entry's", reason)
	}
}

func TestDenylist_IgnoresUnparseableEntriesAndInvalidAddress(t *testing.T) {
	d := NewDenylist([]DenyEntry{{Entry: "not-an-entry"}, {Entry: "203.0.113.0/24"}})
	if d.Len() != 1 {
		t.Fatalf("Len = %d, want 1", d.Len())
	}
	if _, banned := d.Match("SHA256:x", netip.Addr{}); banned {
		t.Fatal("an unknown address matched a network entry")
	}
	var zero Denylist
	if _, banned := zero.Match(bannedKey, netip.MustParseAddr("203.0.113.1")); banned {
		t.Fatal("the zero Denylist banned a session")
	}
}
