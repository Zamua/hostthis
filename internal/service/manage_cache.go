package service

import (
	"errors"
	"io"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteManager is the verb-level surface the SSH layer consumes. Exposing an
// interface rather than the concrete *Manage is what lets cache invalidation be
// a decorator the composition root layers on, invisible to callers and to the
// verb service itself.
type PasteManager interface {
	List(owner string) ([]domain.Paste, error)
	Show(slug domain.Slug, owner string) (domain.Paste, []byte, error)
	Update(slug domain.Slug, owner string, body io.Reader, typeHint string) (UpdateResult, error)
	Rename(slug domain.Slug, owner, name string) error
	Delete(slug domain.Slug, owner string) error
	Versions(slug domain.Slug, owner string) ([]domain.Version, error)
	DeleteVersion(slug domain.Slug, owner string, verNum int) (DeleteVersionResult, error)
	Pin(slug domain.Slug, owner string, verNum int) (domain.Version, error)
	Unpin(slug domain.Slug, owner string) error
	Whoami(owner, subnet string) (WhoamiInfo, error)
}

var _ PasteManager = (*Manage)(nil)

// CacheInvalidating decorates a PasteManager with CDN cache invalidation,
// firing the purger after a SUCCESSFUL mutation that changes the bytes served
// at a paste's public URL (Update / Delete / Pin / Unpin). Verbs that do NOT
// change served bytes are delegated untouched through the embedded interface,
// so no purge fires; that includes DeleteVersion, which is refused outright for
// the currently-served version.
//
// The inner verb service holds no CachePurger and makes no purge calls: that is
// the point of composing invalidation here at the edge.
//
// The purge is best-effort. PurgePaste errors are swallowed (the mutation
// already succeeded on origin, and the impl logs internally), so a transient
// CDN issue never turns a successful update or delete into a failure.
type CacheInvalidating struct {
	PasteManager
	purger CachePurger
}

// NewCacheInvalidating wraps inner so its mutating verbs also purge the CDN. A
// nil purger means no CDN in front and returns inner unwrapped, so the
// composition root can call this unconditionally.
func NewCacheInvalidating(inner PasteManager, purger CachePurger) PasteManager {
	if purger == nil {
		return inner
	}
	return &CacheInvalidating{PasteManager: inner, purger: purger}
}

func (c *CacheInvalidating) Update(slug domain.Slug, owner string, body io.Reader, typeHint string) (UpdateResult, error) {
	res, err := c.PasteManager.Update(slug, owner, body, typeHint)
	if err == nil {
		_ = c.purger.PurgePaste(slug)
	}
	return res, err
}

// Delete purges even on error, unlike its siblings. A failed update leaves the
// cached bytes correct; a failed delete may not, because the removal can land
// and the call still report an error, and the edge would then serve deleted
// content until max-age expires. Purging a paste that survived is harmless, so
// the ambiguous case resolves toward purging.
//
// The pre-mutation rejections are excluded, and that exclusion is load-bearing:
// without it, attempting to delete a slug you do not own would purge it, letting
// any caller spend the CDN's finite purge budget on paste they have no rights to.
func (c *CacheInvalidating) Delete(slug domain.Slug, owner string) error {
	err := c.PasteManager.Delete(slug, owner)
	if err == nil || mayHaveMutated(err) {
		_ = c.purger.PurgePaste(slug)
	}
	return err
}

// mayHaveMutated reports whether a delete error leaves it unknown that the paste
// survived. Only the ownership pre-check's rejections prove no write was
// attempted; every other failure is ambiguous by construction.
func mayHaveMutated(err error) bool {
	switch {
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrNotOwner),
		errors.Is(err, ErrEmptyOwner):
		return false
	}
	return true
}

func (c *CacheInvalidating) Pin(slug domain.Slug, owner string, verNum int) (domain.Version, error) {
	ver, err := c.PasteManager.Pin(slug, owner, verNum)
	if err == nil {
		_ = c.purger.PurgePaste(slug)
	}
	return ver, err
}

func (c *CacheInvalidating) Unpin(slug domain.Slug, owner string) error {
	err := c.PasteManager.Unpin(slug, owner)
	if err == nil {
		_ = c.purger.PurgePaste(slug)
	}
	return err
}
