package service

import "github.com/Zamua/hostthis/internal/domain"

// CachePurger drops a paste's cached representations from a CDN edge.
//
// The caller names only the slug; the implementation owns how its provider
// addresses everything cached for that slug, and purges all of it.
//
// A purge is best-effort: the CacheInvalidating decorator swallows the returned
// error so a transient CDN issue never blocks the mutation that triggered it.
// Implementations still return it so it stays testable.
type CachePurger interface {
	PurgePaste(slug domain.Slug) error
}
