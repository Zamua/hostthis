package domain

import "net/netip"

// SubnetOf returns the canonical subnet bucket of a 4- or 16-byte address (a
// net.IP): IPv4 and IPv4-mapped IPv6 to /24, other IPv6 to /48. Any other
// input, nil included, is the one stable "unknown" bucket.
func SubnetOf(ip []byte) string {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return "unknown"
	}
	if addr = addr.Unmap(); addr.Is4() {
		return netip.PrefixFrom(addr, 24).Masked().Addr().String() + "/24"
	}
	return netip.PrefixFrom(addr, 48).Masked().Addr().String() + "/48"
}
