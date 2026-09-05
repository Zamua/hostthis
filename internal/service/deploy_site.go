package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Zamua/hostthis/internal/archive"
	"github.com/Zamua/hostthis/internal/domain"
)

// SiteRepo is the persistence interface the site-deploy + site-read
// services need. internal/storage.SiteRepo satisfies it.
type SiteRepo interface {
	InsertWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error
	// ReplaceWithQuotaCheck re-deploys an EXISTING owned site in place. s.Slug
	// names the target; s.Identity is the connecting key. A slug that is not a
	// site, and a site owned by another identity, both return
	// domain.ErrNotFound, so "not yours" is indistinguishable from "does not
	// exist". ErrServiceFull / ErrOverUserQuota on quota overflow.
	ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Site, error)
	// Delete re-checks wantIdentity + wantCreatedAt inside its {slug}
	// transaction, so a delete+re-mint of the slug by another identity in the
	// window cannot destroy the new owner's paste.
	Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error
	// SumActiveBytesByOwner returns the identity's active SITE bytes. The
	// deploy path adds the paste-side sum to compute the budget the untar may
	// fill before the persistence-time check.
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
	// ListSitesByOwner returns the identity's active sites. A site counts
	// against the shared quota, so `list` must show it or the quota is
	// invisible and unfreeable.
	ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error)
}

// PasteByteSummer is the slice of the paste repo the deploy path needs for
// its remaining-budget computation. internal/storage.PasteRepo satisfies it.
type PasteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
}

// DeploySite is the application service for a static-site upload: safe-untar,
// content-addressed blob per file, manifest, persisted Site. The per-identity
// quota is enforced BOTH mid-untar (the decompression-bomb guard) AND at
// persistence time (InsertWithQuotaCheck).
type DeploySite struct {
	Sites  SiteRepo
	Pastes PasteByteSummer
	Blob   BlobUnit
	Now    func() time.Time
}

// NewDeploySite wires defaults.
func NewDeploySite(sites SiteRepo, pastes PasteByteSummer, blob BlobUnit) *DeploySite {
	return &DeploySite{Sites: sites, Pastes: pastes, Blob: blob, Now: time.Now}
}

// ListSites returns the owner's active static sites. Anonymous / empty owners
// get an empty list: only keyed identities own content.
func (d *DeploySite) ListSites(owner string) ([]domain.Site, error) {
	if !domain.Identity(owner).IsKeyed() {
		return nil, nil
	}
	return d.Sites.ListSitesByOwner(owner, d.Now().UTC())
}

// SiteResult is what Deploy produced for the SSH layer to format.
type SiteResult struct {
	Site domain.Site
}

// ErrEmptySite is returned when an archive safe-untars to zero files.
var ErrEmptySite = errors.New("service: archive contains no files")

// maxDeployRetries bounds the slug reservation retry budget.
const maxDeployRetries = 5

// Deploy reads a gzip-tar archive from body, untars it safely, stores the
// files, and persists a new Site owned by owner. body is consumed once,
// mid-untar, so peak memory is one file at a time, never the inflated archive.
//
// Returns:
//   - domain.ErrUnsupportedKind: not a valid gzip-tar, or holds no web content
//   - ErrOverQuota: the archive expands past the owner's remaining quota,
//     caught mid-untar by the decompression-bomb guard or at persistence time
//     by the atomic check
//   - ErrTooManyFiles-family errors surfaced verbatim via domain
func (d *DeploySite) Deploy(body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()
	man, err := d.extract(body, owner, now)
	if err != nil {
		return SiteResult{}, err
	}

	site := domain.Site{
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
	}
	stored := man.CompressedSize()

	// Staged blobs are content-addressed, so a slug collision only retries the
	// metadata insert. The one-shot archive never needs to be read again.
	for range maxDeployRetries {
		site.Slug = domain.NewRandomSlug()
		err := d.Sites.InsertWithQuotaCheck(context.Background(), site, stored, int64(domain.UserQuotaBytes), now)
		if errors.Is(err, domain.ErrSlugTaken) {
			continue
		}
		if _, terr := classifyCommitErr(err); terr != nil {
			return SiteResult{}, terr
		}
		return SiteResult{Site: site}, nil
	}
	return SiteResult{}, ErrSlugTaken
}

// extract safe-untars body into staged blobs under the owner's remaining
// budget and returns the manifest. The decompression-bomb guard aborts the
// instant the running total would cross that budget, so a site can never be
// extracted over-quota. Bucket-quota rejections translate via the classifier;
// ErrUnsafeArchive / ErrTooManyFiles / ErrNoWebContent surface verbatim so the
// SSH layer can message them precisely.
func (d *DeploySite) extract(body io.Reader, owner string, now time.Time) (domain.Manifest, error) {
	usedPaste, err := d.Pastes.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return domain.Manifest{}, fmt.Errorf("sum paste bytes: %w", err)
	}
	usedSite, err := d.Sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return domain.Manifest{}, fmt.Errorf("sum site bytes: %w", err)
	}
	budget := siteExtractBudget(int64(domain.UserQuotaBytes), int64(usedPaste), usedSite)

	man, err := archive.Untar(body, &blobSink{blob: d.Blob}, budget)
	switch {
	case errors.Is(err, domain.ErrArchiveTooLarge):
		return domain.Manifest{}, ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		return domain.Manifest{}, domain.ErrUnsupportedKind
	case err != nil:
		_, terr := classifyCommitErr(err)
		return domain.Manifest{}, terr
	case len(man.Files) == 0:
		return domain.Manifest{}, ErrEmptySite
	case !man.HasWebContent():
		return domain.Manifest{}, domain.ErrNoWebContent
	}
	return man, nil
}

// Delete removes an owned static site by slug. A non-site slug and a
// foreign-owned site both collapse to ErrNotFound, the same sentinel the
// paste-delete path returns, so existence and ownership never leak.
func (d *DeploySite) Delete(slug domain.Slug, owner string) error {
	if owner == "" {
		return ErrEmptyOwner
	}
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("get site: %w", err)
	}
	if existing.Identity.String() != owner {
		return ErrNotFound
	}
	return d.Sites.Delete(slug, existing.Identity, existing.CreatedAt)
}

// DeployToSlug appends a SITE version at an existing owned slug. Same pipeline
// as Deploy, but it targets slug instead of minting one and charges the complete
// retained version.
//
// The untar budget is the owner's remaining allowance. Prior versions remain
// retained and charged, so a targeted redeploy receives no replacement credit.
// Returns:
//   - ErrEmptyOwner: anonymous / empty identity
//   - ErrNotFound: slug is not a site owned by owner
//   - domain.ErrUnsupportedKind / domain.ErrNoWebContent: not web content
//   - ErrOverQuota / ErrServiceFull: over the per-identity / service cap
//   - ErrEmptySite: the archive safe-untars to zero files
func (d *DeploySite) DeployToSlug(slug domain.Slug, body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()

	// Verify ownership BEFORE reading the body.
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return SiteResult{}, ErrNotFound
		}
		return SiteResult{}, fmt.Errorf("get site: %w", err)
	}
	if existing.Identity.String() != owner {
		return SiteResult{}, ErrNotFound
	}

	man, err := d.extract(body, owner, now)
	if err != nil {
		return SiteResult{}, err
	}

	site := domain.Site{
		Slug:      slug,
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: existing.CreatedAt, // preserved across re-deploys
		UpdatedAt: now,
	}
	err = d.Sites.ReplaceWithQuotaCheck(context.Background(), site, man.CompressedSize(), int64(domain.UserQuotaBytes), now)
	switch class, terr := classifyCommitErr(err); {
	case class == commitOK:
		return SiteResult{Site: site}, nil
	case errors.Is(err, domain.ErrNotFound):
		// Deleted between the ownership check and the swap.
		// Same shape the up-front check would have yielded.
		return SiteResult{}, ErrNotFound
	default:
		return SiteResult{}, terr
	}
}

// blobSink streams each file through the detached byte plane.
type blobSink struct {
	blob BlobUnit
}

// siteExtractBudget is the unallocated owner quota available to the next
// retained manifest version.
func siteExtractBudget(cap, usedPaste, usedSite int64) int64 {
	return domain.Allowance{Cap: cap, Used: usedPaste + usedSite}.Remaining()
}

func (s *blobSink) Store(p string, r io.Reader, _ int64) (string, int, error) {
	// No buffer: the body streams through the compressor into the object store,
	// hashing as it goes, so an in-flight file costs the compressor window and a
	// copy buffer rather than its own size. The untar guard admits a single file
	// up to the owner's whole remaining allowance, so buffering would size peak
	// memory to the QUOTA.
	sha, compressedSize, err := s.blob.StageEncoding(context.Background(), r)
	if err != nil {
		// The untar's cap sentinel has to survive so SafeUntar can tell a
		// too-large archive from a real I/O failure.
		if errors.Is(err, domain.ErrArchiveTooLarge) {
			return "", 0, domain.ErrArchiveTooLarge
		}
		return "", 0, fmt.Errorf("blob put %q: %w", p, err)
	}
	return sha, compressedSize, nil
}

// ArchiveAdapter presents DeploySite as the upload service's ArchiveDeployer,
// so one Create call handles both cardinalities and no transport has to know
// there are two services behind it.
type ArchiveAdapter struct{ Deployer *DeploySite }

func (a ArchiveAdapter) Deploy(body io.Reader, owner string) (Result, error) {
	res, err := a.Deployer.Deploy(body, owner)
	if err != nil {
		return Result{}, err
	}
	// Only the slug and owner are read downstream; the manifest stays on the
	// stored paste rather than being copied into the response.
	return Result{Paste: domain.Paste{
		Slug:      res.Site.Slug,
		Identity:  res.Site.Identity,
		Kind:      domain.KindSite,
		CreatedAt: res.Site.CreatedAt,
		UpdatedAt: res.Site.UpdatedAt,
	}}, nil
}
