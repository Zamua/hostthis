// Package abuselog writes one structured line per abuse event and nothing for
// normal traffic. The only network identity in a line is the client subnet,
// truncated before the line is built (docs/SPEC.md "Abuse log on hostthisd").
package abuselog

import (
	"encoding/json"
	"net"
	"net/netip"
)

// Event names what happened. The names are shared with the Worker's line.
type Event string

const (
	Created     Event = "created"
	RateLimited Event = "rate-limited"
	Capacity    Event = "capacity"
	Oversize    Event = "oversize"
	Rejected    Event = "rejected"
	AuthFailure Event = "auth-failure"
)

// Logger writes abuse lines through a printf-style sink. The nil Logger drops
// every line.
type Logger struct {
	printf func(format string, args ...any)
}

// New writes each line through printf; a nil printf drops them.
func New(printf func(format string, args ...any)) *Logger {
	return &Logger{printf: printf}
}

type line struct {
	Event  Event  `json:"event"`
	Route  string `json:"route"`
	Status int    `json:"status"`
	Subnet string `json:"subnet"`
}

// Log records one abuse event. addr is a parameter only so it can be
// truncated here: the line holds its subnet and nothing else of the request.
// route is a bounded kind such as "collection" or "ssh", never a path.
func (l *Logger) Log(event Event, route string, status int, addr net.IP) {
	if l == nil || l.printf == nil {
		return
	}
	b, err := json.Marshal(line{Event: event, Route: route, Status: status, Subnet: Subnet(addr)})
	if err != nil {
		return
	}
	l.printf("%s", b)
}

// Subnet is the IPv4 /24 or IPv6 /40 of addr, an IPv4-mapped IPv6 address
// counting as IPv4. Anything that is not a 4- or 16-byte address is "unknown".
func Subnet(addr net.IP) string {
	a, ok := netip.AddrFromSlice(addr)
	if !ok {
		return "unknown"
	}
	if a = a.Unmap(); a.Is4() {
		return netip.PrefixFrom(a, 24).Masked().String()
	}
	return netip.PrefixFrom(a, 40).Masked().String()
}
