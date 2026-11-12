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
	List(owner domain.Owner) ([]domain.Paste, error)
	Show(slug domain.Slug, owner domain.Owner) (domain.Paste, io.ReadCloser, error)
	Update(slug domain.Slug, owner domain.Owner, body io.Reader, typeHint string) (UpdateResult, error)
	Rename(slug domain.Slug, owner domain.Owner, name string) error
	Delete(slug domain.Slug, owner domain.Owner) error
	Versions(slug domain.Slug, owner domain.Owner) ([]domain.Version, error)
	DeleteVersion(slug domain.Slug, owner domain.Owner, verNum int) (DeleteVersionResult, error)
	Pin(slug domain.Slug, owner domain.Owner, verNum int) (domain.Version, error)
	Unpin(slug domain.Slug, owner domain.Owner) error
	Whoami(owner domain.Owner, subnet string) (WhoamiInfo, error)
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
	purges
}

// NewCacheInvalidating wraps inner so its mutating verbs also purge the CDN. A
// nil purger means no CDN in front and returns inner unwrapped, so the
// composition root can call this unconditionally.
func NewCacheInvalidating(inner PasteManager, purger CachePurger, exempt PurgeExempt) PasteManager {
	if purger == nil {
		return inner
	}
	return &CacheInvalidating{PasteManager: inner, purges: purges{purger, exempt}}
}

func (c *CacheInvalidating) Update(slug domain.Slug, owner domain.Owner, body io.Reader, typeHint string) (UpdateResult, error) {
	res, err := c.PasteManager.Update(slug, owner, body, typeHint)
	c.changed(slug, owner, err)
	return res, err
}

func (c *CacheInvalidating) Delete(slug domain.Slug, owner domain.Owner) error {
	err := c.PasteManager.Delete(slug, owner)
	c.removed(slug, owner, err)
	return err
}

func (c *CacheInvalidating) Pin(slug domain.Slug, owner domain.Owner, verNum int) (domain.Version, error) {
	ver, err := c.PasteManager.Pin(slug, owner, verNum)
	c.changed(slug, owner, err)
	return ver, err
}

func (c *CacheInvalidating) Unpin(slug domain.Slug, owner domain.Owner) error {
	err := c.PasteManager.Unpin(slug, owner)
	c.changed(slug, owner, err)
	return err
}

// mayHaveMutated reports whether an error leaves it unknown that nothing was
// written. Only rejections raised before any write prove that. Every error a
// verb can raise before ownership is proven must be listed: otherwise any
// caller could spend the CDN's finite purge budget on slugs they do not own.
func mayHaveMutated(err error) bool {
	switch {
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrRawTooLarge), errors.Is(err, ErrCompressedTooLarge),
		errors.Is(err, ErrEmptyUpload), errors.Is(err, errStaging):
		return false
	}
	return true
}
