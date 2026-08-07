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
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// LegacySites is the pre-unification site family: rows written before a
// directory was an artifact. Read-only, and consulted ONLY when the artifact
// families do not answer, so a deploy that predates the collapse keeps serving
// until it is migrated. Nil once none remain, which is when this port and the
// family behind it both go.
type LegacySites interface {
	Get(domain.Slug) (domain.Site, error)
	ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error)
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
}

// ArtifactSites adapts an artifact repo onto the site port, falling back to the
// legacy family for rows the collapse has not reached.
type ArtifactSites struct {
	repo   *ShaleRepo
	legacy LegacySites // nil when there is nothing left to fall back to
}

func NewArtifactSites(repo *ShaleRepo, legacy LegacySites) *ArtifactSites {
	return &ArtifactSites{repo: repo, legacy: legacy}
}

// Get returns the directory artifact owning slug.
//
// A slug that is a DOCUMENT reads as not-found, not as a one-file site: the
// caller asked for a directory, and answering with a document would let a
// paste be served through the site path. The shape is read from the kind, not
// inferred from the manifest's size.
func (a *ArtifactSites) Get(slug domain.Slug) (domain.Site, error) {
	p, err := a.repo.Get(slug)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return a.legacyGet(slug)
		}
		return domain.Site{}, err
	}
	if p.Kind != domain.KindSite {
		// A document owns the slug, so no legacy site can: the families share
		// one namespace, enforced by a cross-family check on every insert.
		return domain.Site{}, ErrNotFound
	}
	return siteFromArtifact(p), nil
}

func (a *ArtifactSites) legacyGet(slug domain.Slug) (domain.Site, error) {
	if a.legacy == nil {
		return domain.Site{}, ErrNotFound
	}
	return a.legacy.Get(slug)
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

// ReplaceWithQuotaCheck re-deploys an existing directory: it appends the new
// manifest as a version, then tombstones the one it supersedes.
//
// The tombstone is what keeps a redeploy's COST unchanged. Leaving prior
// versions live would charge every redeploy cumulatively, and redeploying a
// slug in place is the normal way a directory is iterated on - so uniform
// version retention would turn the ordinary workflow into a quota cliff.
// Retention is a feature to add deliberately, not a side effect of the
// collapse.
//
// NOT atomic across the two steps: a failure between them leaves the previous
// version live and the owner over-charged until the next redeploy or an
// explicit version delete. That direction is the safe one - the new content is
// already serving, and the error is visible in the owner's own listing rather
// than silently losing bytes.
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
	live, err := a.repo.ListVersions(s.Slug)
	if err != nil {
		return err
	}
	root, _ := s.Manifest.Lookup("/")
	res, err := a.repo.AppendManifestVersion(ctx, s.Slug, s.Manifest, root, dedupedSize, userCap, now)
	if err != nil {
		return err
	}
	for _, v := range live {
		if v.Deleted || v.VerNum == res.NewVer {
			continue
		}
		if derr := a.repo.DeleteVersion(s.Slug, v.VerNum); derr != nil {
			return derr
		}
	}
	return nil
}

func (a *ArtifactSites) Delete(slug domain.Slug) error { return a.repo.Delete(slug) }

// SumActiveBytesByOwner reports the LEGACY bytes only.
//
// An artifact-backed directory contributes ZERO here, and that is not a stub:
// its bytes are already in the artifact sum the service adds this to, so
// reporting them again would bill every directory twice. Only rows still in the
// old family are unaccounted for, and only those are counted. The figure
// reaches zero when the migration does.
func (a *ArtifactSites) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	if a.legacy == nil {
		return 0, nil
	}
	return a.legacy.SumActiveBytesByOwner(owner, now)
}

// ListSitesByOwner returns the owner's directories, filtered out of the one
// artifact listing rather than read from a second enumeration index.
// ListSitesByOwner returns the owner's LEGACY directories only.
//
// The same rule the sum follows, for the same reason: an artifact-backed
// directory is already in the artifact listing the caller concatenates this
// onto, so returning it here would show it twice. Only rows the artifact
// families do not cover are reported, and that set empties with the migration.
func (a *ArtifactSites) ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error) {
	if a.legacy == nil {
		return nil, nil
	}
	return a.legacy.ListSitesByOwner(owner, now)
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
