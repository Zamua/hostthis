package domain

import "net/netip"

// AbuseEvent names what an abuse log line reports. The names are shared with
// the Worker's line (docs/SPEC.md "Abuse log on hostthisd").
type AbuseEvent string

const (
	AbuseCreated     AbuseEvent = "created"
	AbuseRateLimited AbuseEvent = "rate-limited"
	AbuseCapacity    AbuseEvent = "capacity"
	AbuseOversize    AbuseEvent = "oversize"
	AbuseRejected    AbuseEvent = "rejected"
	AbuseAuthFailure AbuseEvent = "auth-failure"
)

// AbuseSubnet is the subnet an abuse line groups a client under: the IPv4 /24
// or IPv6 /40 of a 4- or 16-byte address (a net.IP), an IPv4-mapped IPv6
// address counting as IPv4. Any other input is "unknown". Admission buckets
// are SubnetOf's, narrower for IPv6; the line carries the address itself too.
func AbuseSubnet(ip []byte) string {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return "unknown"
	}
	if addr = addr.Unmap(); addr.Is4() {
		return netip.PrefixFrom(addr, 24).Masked().String()
	}
	return netip.PrefixFrom(addr, 40).Masked().String()
}

// AddrText is the canonical text of a client address for a log line or a
// record: an IPv4-mapped IPv6 address reads as IPv4, and a missing or
// unparseable one as "unknown".
func AddrText(ip []byte) string {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return "unknown"
	}
	return addr.Unmap().String()
}
