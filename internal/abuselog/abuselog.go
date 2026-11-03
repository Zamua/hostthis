// Package abuselog writes one structured line per abuse event and nothing for
// normal traffic. The only network identity in a line is the client subnet,
// truncated before the line is built (docs/SPEC.md "Abuse log on hostthisd").
package abuselog

import (
	"encoding/json"
	"net"

	"github.com/Zamua/hostthis/internal/domain"
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
	Event  domain.AbuseEvent `json:"event"`
	Route  string            `json:"route"`
	Status int               `json:"status"`
	Subnet string            `json:"subnet"`
}

// Log records one abuse event. addr is a parameter only so it can be
// truncated here: the line holds its subnet and nothing else of the request.
// route is a bounded kind such as "collection" or "ssh", never a path.
func (l *Logger) Log(event domain.AbuseEvent, route string, status int, addr net.IP) {
	if l == nil || l.printf == nil {
		return
	}
	b, err := json.Marshal(line{Event: event, Route: route, Status: status, Subnet: domain.AbuseSubnet(addr)})
	if err != nil {
		return
	}
	l.printf("%s", b)
}
