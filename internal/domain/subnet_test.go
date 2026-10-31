package domain

import (
	"net/netip"
	"testing"
)

func TestSubnetOfBuckets(t *testing.T) {
	cases := []struct {
		ip   []byte
		want string
	}{
		{nil, "unknown"},
		{[]byte{1, 2, 3}, "unknown"},
		{[]byte{203, 0, 113, 77}, "203.0.113.0/24"},
		{netip.MustParseAddr("::ffff:203.0.113.77").AsSlice(), "203.0.113.0/24"},
		{netip.MustParseAddr("2001:db8:abcd:12::1").AsSlice(), "2001:db8:abcd::/48"},
		{netip.MustParseAddr("::1").AsSlice(), "::/48"},
	}
	for _, c := range cases {
		if got := SubnetOf(c.ip); got != c.want {
			t.Errorf("SubnetOf(%v) = %q, want %q", c.ip, got, c.want)
		}
	}
}
