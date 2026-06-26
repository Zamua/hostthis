package service

import "github.com/Zamua/hostthis/internal/domain"

// CachePurger drops a paste's cached representations from a CDN edge.
// Implementations: noop (no CDN in front), cloudflare, fastly, etc.
//
// The caller names only the slug; the implementation owns the knowledge
// of which URL variants that slug is reachable at (e.g. the page plus the
// markdown shell's "?raw=1" content fetch) and purges all of them.
//
// A purge is best-effort: the CacheInvalidating decorator logs/swallows
// the returned error so a transient CDN issue never blocks the mutation
// that triggered it. Implementations should still return the error so it
// is testable.
type CachePurger interface {
	PurgePaste(slug domain.Slug) error
}

// URLBuilder turns a slug into the public URL a CDN would cache.
// Same shape used by the SSH layer for stdout URL emission.
type URLBuilder func(domain.Slug) string
