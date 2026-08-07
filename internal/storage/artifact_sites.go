// The site surface, served by the artifact families.
//
// A directory IS an artifact whose version manifest holds N entries, so this
// satisfies the service's SiteRepo port without a second key family, a second
// enumeration index, or a second quota scan. It exists so the service layer
// keeps one vocabulary while the storage underneath collapses; once no legacy
// site rows remain, the port itself is what goes.

package storage

import (
	"context"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ArtifactSites adapts an artifact repo onto the site port.
type ArtifactSites struct{ repo *ShaleRepo }

func NewArtifactSites(repo *ShaleRepo) *ArtifactSites { return &ArtifactSites{repo: repo} }

// Get returns the directory artifact owning slug.
//
// A slug that is a DOCUMENT reads as not-found, not as a one-file site: the
// caller asked for a directory, and answering with a document would let a
// paste be served through the site path. The shape is read from the kind, not
// inferred from the manifest's size.
func (a *ArtifactSites) Get(slug domain.Slug) (domain.Site, error) {
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

// InsertWithQuotaCheck stores a new directory as an artifact.
//
// dedupedSize is the CHARGED size: a directory's distinct blob total, which is
// what the quota counts, rather than the root file's size.
func (a *ArtifactSites) InsertWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	root, _ := s.Manifest.Lookup("/")
	return a.repo.InsertWithQuotaCheck(ctx, domain.Paste{
		Slug:       s.Slug,
		Identity:   s.Identity,
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindSite,
		ContentSHA: root.SHA,
		Size:       dedupedSize,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
		Manifest:   s.Manifest,
	}, userCap, now)
}

// ReplaceWithQuotaCheck re-deploys an existing directory by APPENDING a
// version, which is what gives a directory the history, pin and rollback a
// document has always had (docs/SPEC.md "A version is a whole-manifest
// snapshot").
//
// Ownership is enforced here rather than inside the append: a slug that is not
// a directory, and one owned by another identity, both yield the not-found
// sentinel, so "not yours" stays indistinguishable from "does not exist".
func (a *ArtifactSites) ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	existing, err := a.repo.Get(s.Slug)
	if err != nil {
		return err
	}
	if existing.Kind != domain.KindSite || existing.Identity != s.Identity {
		return ErrNotFound
	}
	root, _ := s.Manifest.Lookup("/")
	_, err = a.repo.AppendManifestVersion(ctx, s.Slug, s.Manifest, root, dedupedSize, userCap, now)
	return err
}

func (a *ArtifactSites) Delete(slug domain.Slug) error { return a.repo.Delete(slug) }

// SumActiveBytesByOwner reports ZERO, always.
//
// Not a stub: a directory is an artifact, so its bytes are ALREADY in the
// artifact sum the service adds this to. Returning the real figure here would
// bill every directory twice. The method survives only because the port still
// names it; it goes when the port does.
func (a *ArtifactSites) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	return 0, nil
}

// ListSitesByOwner returns the owner's directories, filtered out of the one
// artifact listing rather than read from a second enumeration index.
func (a *ArtifactSites) ListSitesByOwner(owner string, _ time.Time) ([]domain.Site, error) {
	all, err := a.repo.ListByOwner(owner)
	if err != nil {
		return nil, err
	}
	var out []domain.Site
	for _, p := range all {
		if p.Kind == domain.KindSite {
			out = append(out, siteFromArtifact(p))
		}
	}
	return out, nil
}

// PreClaimSlug stakes the artifact claim, so a directory's files stage on the
// same shard its manifest commits to.
func (a *ArtifactSites) PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error {
	return a.repo.PreClaimSiteSlug(ctx, slug, owner, now)
}

// ReleaseSlugClaim drops a claim whose deploy never landed.
func (a *ArtifactSites) ReleaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) error {
	return a.repo.ReleaseSiteSlugClaim(ctx, slug, owner)
}
