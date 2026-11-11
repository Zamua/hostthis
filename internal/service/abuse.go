package service

import (
	"net"

	"github.com/Zamua/hostthis/internal/domain"
)

// AbuseLog records one abuse event with the client's address. Nothing else
// of the request is handed over.
type AbuseLog interface {
	Log(event domain.AbuseEvent, route string, status int, addr net.IP)
}
