// The site surface, served by the paste family.
//
// A directory IS a paste whose version manifest holds N entries, so this
// satisfies the service's SiteRepo port without a second key family, a second
// enumeration index, or a second quota scan. It exists so the service layer
// keeps one vocabulary; when that vocabulary collapses too, the port goes.

package storage

import (
	"context"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Sites adapts the paste repo onto the site port.

type Sites struct {
	repo *ShaleRepo
}

func NewSites(repo *ShaleRepo) *Sites {
	return &Sites{repo: repo}
}

// Get returns the directory owning slug.
//
// A slug that is a DOCUMENT reads as not-found, not as a one-file site: the
// caller asked for a directory, and answering with a document would let a
// paste be served through the site path. The shape is read from the kind, not
// inferred from the manifest's size.
func (a *Sites) Get(slug domain.Slug) (domain.Site, error) {
	p, err := a.repo.Get(slug)
	if err != nil {
		return domain.Site{}, err
	}
	if p.Kind != domain.KindSite {
		return domain.Site{}, ErrNotFound
	}
	return siteFromArtifact(p), nil
}

func siteFromArtifact(p domain.Paste) domain.Site {
	return domain.Site{
		Slug:      p.Slug,
		Identity:  p.Identity,
		Manifest:  p.Manifest,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// InsertWithQuotaCheck stores a new directory as a paste.
//
// storedBytes is the CHARGED size: a directory's distinct blob total, which is
// what the quota counts, rather than the root file's size.
func (a *Sites) InsertWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error {
	root, _ := s.Manifest.Lookup("/")
	return a.repo.InsertWithQuotaCheck(ctx, domain.Paste{
		Slug:       s.Slug,
		Identity:   s.Identity,
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindSite,
		ContentSHA: root.SHA,
		Size:       storedBytes,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
		Manifest:   s.Manifest,
	}, userCap, now)
}

// ReplaceWithQuotaCheck re-deploys an existing directory by APPENDING the new
// manifest as a version.
//
// Prior versions stay live, exactly as they do for a document, so a directory
// pins, rolls back and rolls forward like anything else. That is the point of
// one paste model: a redeploy is an update, and an update has never thrown
// away what it replaced.
//
// It therefore CHARGES like an update too - every live version counts against
// quota, and an owner reclaims bytes by deleting versions they no longer want.
// Blob dedup keeps the cost proportional to what actually changed: a redeploy
// touching one file of two hundred stores and charges for one blob.
//
// Ownership is enforced here rather than inside the append: a slug that is not
// a directory, and one owned by another identity, both yield the not-found
// sentinel, so "not yours" stays indistinguishable from "does not exist".
func (a *Sites) ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error {
	existing, err := a.repo.Get(s.Slug)
	if err != nil {
		return err
	}
	if existing.Kind != domain.KindSite || existing.Identity != s.Identity {
		return ErrNotFound
	}
	root, _ := s.Manifest.Lookup("/")
	_, err = a.repo.AppendManifestVersion(ctx, s.Slug, s.Manifest, root, storedBytes, userCap, now)
	return err
}

func (a *Sites) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	return a.repo.Delete(slug, wantIdentity, wantCreatedAt)
}

// SumActiveBytesByOwner reports ZERO, and that is not a stub.
//
// A directory IS a paste, so its bytes are already in the paste sum the
// service adds this to. Reporting them again would bill every directory twice.
func (a *Sites) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	return 0, nil
}

// ListSitesByOwner returns NOTHING, for the reason the sum returns zero: a
// directory is already in the paste listing the caller concatenates this
// onto, so returning it here would show it twice.
func (a *Sites) ListSitesByOwner(string, time.Time) ([]domain.Site, error) {
	return nil, nil
}

// PreClaimSlug stakes the paste claim, so a directory's files stage on the
// same shard its manifest commits to.
func (a *Sites) PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error {
	return a.repo.PreClaimSlug(ctx, slug, owner, now)
}

// ReleaseSlugClaim drops a claim whose deploy never landed.
func (a *Sites) ReleaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) error {
	return a.repo.ReleaseSlugClaim(ctx, slug, owner)
}
