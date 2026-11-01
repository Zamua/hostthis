package http

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// ClientIPSource names where the room-creation limit reads the client
// address from (docs/SPEC.md "Trusted source-IP derivation").
type ClientIPSource int

const (
	// ClientIPPeer is the TCP peer: hostthisd faces clients directly.
	ClientIPPeer ClientIPSource = iota
	// ClientIPForwardedFor is the right-most X-Forwarded-For hop, the address
	// a trusted proxy in front recorded.
	ClientIPForwardedFor
	// ClientIPCloudflare is ClientIPForwardedFor, except that a hop inside
	// Cloudflare's published ranges hands over to CF-Connecting-IP: the proxy
	// in front sees Cloudflare's edge for a proxied host and the client for a
	// direct one.
	ClientIPCloudflare
)

// ParseClientIPSource reads HOSTTHIS_HTTP_CLIENT_IP_SOURCE; "" is the peer.
func ParseClientIPSource(v string) (ClientIPSource, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "peer":
		return ClientIPPeer, nil
	case "xff-rightmost":
		return ClientIPForwardedFor, nil
	case "cloudflare":
		return ClientIPCloudflare, nil
	}
	return ClientIPPeer, fmt.Errorf("client ip source %q: want peer, xff-rightmost or cloudflare", v)
}

// cloudflareRanges are https://www.cloudflare.com/ips-v4 and /ips-v6.
var cloudflareRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
		"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
		"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
		"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
		"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

func fromCloudflare(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range cloudflareRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientSubnet derives the canonical /24 (IPv4) or /48 (IPv6) subnet of the
// requester for the per-IP room-creation rate limit. Only the right-most
// X-Forwarded-For hop is ever read: the left-most is the client's own claim,
// and a fresh one per POST would be a fresh bucket. CF-Connecting-IP counts
// only behind a Cloudflare hop, since a direct client can send it too. An
// unparseable address becomes the "unknown" bucket.
func clientSubnet(r *http.Request, source ClientIPSource) string {
	return domain.SubnetOf(clientAddress(r, source))
}

// clientAddress is the requester's address under source, nil when unparseable.
func clientAddress(r *http.Request, source ClientIPSource) net.IP {
	host := ""
	if source != ClientIPPeer {
		host = rightmostForwardedFor(r.Header.Values("X-Forwarded-For"))
	}
	if source == ClientIPCloudflare && fromCloudflare(host) {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			host = cf
		}
	}
	if host == "" {
		h, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		} else {
			host = h
		}
	}
	return net.ParseIP(host)
}

// rightmostForwardedFor is the last value of the X-Forwarded-For list that the
// header lines form together, "" for none. A client can send a line of its
// own ahead of the one the trusted proxy appends, so only the last line can
// hold the proxy's hop.
func rightmostForwardedFor(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	last := lines[len(lines)-1]
	return strings.TrimSpace(last[strings.LastIndexByte(last, ',')+1:])
}
