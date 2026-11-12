package service

import "github.com/Zamua/hostthis/internal/domain"

// CachePurger drops a paste's cached representations from a CDN edge.
//
// The caller names only the slug; the implementation owns how its provider
// addresses everything cached for that slug, and purges all of it.
// PurgePaste is for a slug whose content changed; PurgeRemoved is for one that
// no longer serves, and goes ahead of every PurgePaste when purges queue.
//
// A purge is best-effort: the CacheInvalidating decorator swallows the returned
// error so a transient CDN issue never blocks the mutation that triggered it.
// Implementations still return it so it stays testable.
type CachePurger interface {
	PurgePaste(slug domain.Slug) error
	PurgeRemoved(slug domain.Slug) error
}

// PurgeExempt reports owners whose writes purge nothing: test traffic whose
// reads never fill the edge cache. Nil exempts no one.
type PurgeExempt func(domain.Owner) bool

// purges is what the cache decorators share: the purger and who is exempt.
type purges struct {
	purger CachePurger
	exempt PurgeExempt
}

// changed purges after a mutation that succeeded or may have. A write can
// commit and the call still fail (a lost response), leaving the edge serving
// the old state until its TTL ends; purging a slug that did not change is
// harmless, so the ambiguous case resolves toward purging.
func (p purges) changed(slug domain.Slug, owner domain.Owner, err error) {
	if p.owed(owner, err) {
		_ = p.purger.PurgePaste(slug)
	}
}

// removed is changed for a delete.
func (p purges) removed(slug domain.Slug, owner domain.Owner, err error) {
	if p.owed(owner, err) {
		_ = p.purger.PurgeRemoved(slug)
	}
}

func (p purges) owed(owner domain.Owner, err error) bool {
	if p.exempt != nil && p.exempt(owner) {
		return false
	}
	return err == nil || mayHaveMutated(err)
}
