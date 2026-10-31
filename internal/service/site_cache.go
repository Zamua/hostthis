package service

import (
	"io"

	"github.com/Zamua/hostthis/internal/domain"
)

// SiteCacheInvalidating decorates a SiteDeployer with CDN cache invalidation:
// an in-place redeploy and a delete change what the slug serves, so each
// purges it by the same rule as the paste verbs (purgeUnlessUntouched). A new
// deploy mints a slug nothing has cached, so it passes through.
type SiteCacheInvalidating struct {
	SiteDeployer
	purger CachePurger
}

// NewSiteCacheInvalidating wraps inner so its writes also purge the CDN. A nil
// purger returns inner unwrapped.
func NewSiteCacheInvalidating(inner SiteDeployer, purger CachePurger) SiteDeployer {
	if purger == nil {
		return inner
	}
	return &SiteCacheInvalidating{SiteDeployer: inner, purger: purger}
}

func (c *SiteCacheInvalidating) DeployToSlug(slug domain.Slug, body io.Reader, owner domain.Owner) (SiteResult, error) {
	res, err := c.SiteDeployer.DeployToSlug(slug, body, owner)
	purgeUnlessUntouched(c.purger, slug, err)
	return res, err
}

func (c *SiteCacheInvalidating) Delete(slug domain.Slug, owner domain.Owner) error {
	err := c.SiteDeployer.Delete(slug, owner)
	purgeUnlessUntouched(c.purger, slug, err)
	return err
}
