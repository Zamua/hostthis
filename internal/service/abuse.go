package service

import (
	"net"

	"github.com/Zamua/hostthis/internal/domain"
)

// AbuseLog records one abuse event. addr is a parameter only so the adapter
// can truncate it: nothing else of the request is handed over.
type AbuseLog interface {
	Log(event domain.AbuseEvent, route string, status int, addr net.IP)
}
