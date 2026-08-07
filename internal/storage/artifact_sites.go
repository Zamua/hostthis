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
	// Delete drops a row the artifact families have taken over. Not a
	// convenience: a migrated directory left in both places is listed twice and
	// charged twice.
	Delete(domain.Slug) error
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

// ReplaceWithQuotaCheck re-deploys an existing directory by APPENDING the new
// manifest as a version.
//
// Prior versions stay live, exactly as they do for a document, so a directory
// pins, rolls back and rolls forward like anything else. That is the point of
// one artifact model: a redeploy is an update, and an update has never thrown
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
func (a *ArtifactSites) ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	existing, err := a.repo.Get(s.Slug)
	if errors.Is(err, ErrNotFound) {
		// No artifact yet, so this is a directory deployed before the collapse.
		// Redeploying MIGRATES it: the new manifest lands as an artifact, which
		// is also what stops a legacy row becoming permanently
		// un-redeployable once the artifact path is wired.
		return a.migrateOnReplace(ctx, s, dedupedSize, userCap, now)
	}
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

// migrateOnReplace turns a legacy directory into an artifact by writing the
// redeployed manifest through the ordinary insert.
//
// The legacy row is dropped once the artifact lands. It cannot be kept as a
// rollback escape hatch: a directory present in both families is enumerated by
// both, so the owner would see it twice and be charged for it twice. The
// artifact is written FIRST, so a failure between the two steps leaves the row
// still readable rather than losing it.
func (a *ArtifactSites) migrateOnReplace(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	prior, err := a.legacyGet(s.Slug)
	if err != nil {
		return err // genuinely absent: the not-found a replace should report
	}
	if prior.Identity != s.Identity {
		return ErrNotFound
	}
	fresh := s
	fresh.CreatedAt = prior.CreatedAt // the artifact inherits the original age
	if err := a.InsertWithQuotaCheck(ctx, fresh, dedupedSize, userCap, now); err != nil {
		return err
	}
	return a.legacy.Delete(s.Slug)
}
