package service

import "github.com/Zamua/hostthis/internal/domain"

// CachePurger drops cached versions of paste URLs from a CDN edge.
// Implementations: noop (no CDN in front), cloudflare, fastly, etc.
// Purge calls are best-effort from the service's perspective; an
// implementation that fails should log internally and return the error
// for tests, but service-layer callers swallow it so the underlying
// operation (delete/update) is never blocked by a transient CDN issue.
type CachePurger interface {
	PurgeURLs(urls []string) error
}

// URLBuilder turns a slug into the public URL a CDN would cache.
// Same shape used by the SSH layer for stdout URL emission.
type URLBuilder func(domain.Slug) string
