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
	Show(slug domain.Slug, owner string) (domain.Paste, io.ReadCloser, error)
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
// firing the purger after a mutation that may have changed the bytes served at
// a paste's public URL (Update / Delete / Pin / Unpin; see mayHaveMutated).
// Verbs that do not change served bytes pass through the embedded interface
// untouched; that includes DeleteVersion, which is refused outright for the
// served version. The inner verb service holds no CachePurger.
//
// The purge is best-effort: errors are swallowed (the impl logs), so a
// transient CDN issue never turns a mutation into a failure.
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
	purgeUnlessUntouched(c.purger, slug, err)
	return res, err
}

func (c *CacheInvalidating) Delete(slug domain.Slug, owner string) error {
	err := c.PasteManager.Delete(slug, owner)
	purgeUnlessUntouched(c.purger, slug, err)
	return err
}

func (c *CacheInvalidating) Pin(slug domain.Slug, owner string, verNum int) (domain.Version, error) {
	ver, err := c.PasteManager.Pin(slug, owner, verNum)
	purgeUnlessUntouched(c.purger, slug, err)
	return ver, err
}

func (c *CacheInvalidating) Unpin(slug domain.Slug, owner string) error {
	err := c.PasteManager.Unpin(slug, owner)
	purgeUnlessUntouched(c.purger, slug, err)
	return err
}

// purgeUnlessUntouched purges after a mutation that succeeded or may have. A
// write can commit and the call still fail (a lost response), leaving the edge
// serving the old state until its TTL ends; purging a slug that did not change
// is harmless, so the ambiguous case resolves toward purging.
func purgeUnlessUntouched(purger CachePurger, slug domain.Slug, err error) {
	if err == nil || mayHaveMutated(err) {
		_ = purger.PurgePaste(slug)
	}
}

// mayHaveMutated reports whether an error leaves it unknown that nothing was
// written. Only rejections raised before any write prove that. Every error a
// verb can raise before ownership is proven must be listed: otherwise any
// caller could spend the CDN's finite purge budget on slugs they do not own.
func mayHaveMutated(err error) bool {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrEmptyOwner),
		errors.Is(err, ErrRawTooLarge), errors.Is(err, ErrCompressedTooLarge),
		errors.Is(err, ErrEmptyUpload), errors.Is(err, errStaging):
		return false
	}
	return true
}
