package cache

import (
	"net"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// scope is everything one slug's purge covers: its host in subdomain mode,
// else its path prefix on the apex. Exactly one field is set.
type scope struct {
	Host   string
	Prefix string
}

// slugScope maps a slug to its scope. A purge covers every URL under it, so no
// list of query variants has to track what the shells fetch. Purges name hosts,
// not origins, so a port on the apex is dropped.
func slugScope(apex, mode string, slug domain.Slug) scope {
	if host, _, err := net.SplitHostPort(apex); err == nil {
		apex = host
	}
	if strings.EqualFold(mode, "path") {
		// Slugs have a fixed length, so the prefix names one slug.
		return scope{Prefix: apex + "/p/" + slug.String()}
	}
	return scope{Host: slug.String() + "." + apex}
}
