// The site surface, served by the paste family: a directory IS a paste whose
// version manifest holds N entries, so this satisfies the service's SiteRepo
// port without a second key family, enumeration index, or quota scan.

package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrNotDirectoryKind rejects a write whose Site carries a kind that is not a
// directory kind. Get reads the shape from the kind, so a row stored under any
// other one answers as not-found: the deploy would report success and hand out
// a dead URL. Failing the write is the only place this is still visible.
var ErrNotDirectoryKind = errors.New("storage: not a directory kind")

func checkDirectoryKind(s domain.Site) error {
	if !s.Kind.IsDirectory() {
		return fmt.Errorf("%w: %q (slug %s)", ErrNotDirectoryKind, s.Kind, s.Slug)
	}
	return nil
}

// SiteBackingRepo is the slice of a paste repo the site surface needs. An
// interface rather than a concrete repo: the translation is pure vocabulary,
// directory to paste and back, so every backend shares this one implementation.
type SiteBackingRepo interface {
	Get(domain.Slug) (domain.Paste, error)
	InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error
	AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string, kind domain.ContentKind,
		uploadID string, m domain.Manifest, size int, userCap int64, now time.Time) (AppendResult, error)
	Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) ([]string, error)
}

// Sites adapts the paste repo onto the site port.
type Sites struct {
	repo SiteBackingRepo
}

func NewSites(repo SiteBackingRepo) *Sites {
	return &Sites{repo: repo}
}

// Get returns the directory owning slug.
//
// A slug that is a DOCUMENT reads as not-found, not as a one-file site: the
// caller asked for a directory, and answering with a document would let a
// paste be served through the site path. The shape is read from the kind, not
// inferred from the manifest's size; both directory kinds answer here, since
// storage treats a site and a knowledge base identically.
func (a *Sites) Get(slug domain.Slug) (domain.Site, error) {
	p, err := a.repo.Get(slug)
	if err != nil {
		return domain.Site{}, err
	}
	if !p.Kind.IsDirectory() {
		return domain.Site{}, ErrNotFound
	}
	return siteFromArtifact(p), nil
}

func siteFromArtifact(p domain.Paste) domain.Site {
	return domain.Site{
		Slug:      p.Slug,
		Identity:  p.Identity,
		Kind:      p.Kind,
		UploadID:  p.UploadID,
		Manifest:  p.Manifest,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// InsertWithQuotaCheck stores a new directory as a paste.
//
// storedBytes is the CHARGED size: every manifest path's compressed size, which
// is what the quota counts, rather than the root file's size.
func (a *Sites) InsertWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error {
	if err := checkDirectoryKind(s); err != nil {
		return err
	}
	return a.repo.InsertWithQuotaCheck(ctx, domain.Paste{
		Slug:       s.Slug,
		Generation: domain.NewPasteGeneration(),
		Identity:   s.Identity,
		Status:     domain.PasteStatusReady,
		Kind:       s.Kind,
		UploadID:   s.UploadID,
		Size:       storedBytes,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
		Manifest:   s.Manifest,
	}, userCap, now)
}

// ReplaceWithQuotaCheck re-deploys an existing directory by APPENDING the new
// manifest as a version. Prior versions stay live, so a directory pins and
// rolls back like a document, and each live manifest version is charged in
// full, matching the objects it holds.
//
// Ownership is enforced here rather than inside the append: a slug that is not
// a directory, and one owned by another identity, both yield not-found, so
// "not yours" stays indistinguishable from "does not exist".
func (a *Sites) ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error {
	if err := checkDirectoryKind(s); err != nil {
		return err
	}
	existing, err := a.repo.Get(s.Slug)
	if err != nil {
		return err
	}
	if !existing.Kind.IsDirectory() || existing.Identity != s.Identity {
		return ErrNotFound
	}
	// The appended version carries the shape THIS deploy decided, so a redeploy
	// that adds or drops a root index.html changes what the slug serves as.
	_, err = a.repo.AppendManifestVersion(
		ctx, s.Slug, existing.Generation, s.Kind, s.UploadID, s.Manifest, storedBytes, userCap, now,
	)
	return err
}

func (a *Sites) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) ([]string, error) {
	return a.repo.Delete(slug, wantIdentity, wantCreatedAt)
}

// SumActiveBytesByOwner reports ZERO, and that is not a stub.
//
// A directory IS a paste, so its bytes are already in the paste sum the
// service adds this to. Reporting them again would bill every directory twice.
func (a *Sites) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	return 0, nil
}

// ListSitesByOwner returns nothing, for the reason the sum returns zero: a
// directory is already in the paste listing this is concatenated onto.
func (a *Sites) ListSitesByOwner(string, time.Time) ([]domain.Site, error) {
	return nil, nil
}
