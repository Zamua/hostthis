package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/Zamua/hostthis/internal/archive"
	"github.com/Zamua/hostthis/internal/domain"
)

// SiteRepo is the persistence interface the site-deploy + site-read
// services need. internal/storage.SiteRepo satisfies it.
type SiteRepo interface {
	InsertWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error
	// ReplaceWithQuotaCheck re-deploys an EXISTING owned site in place,
	// atomically swapping its manifest for s.Manifest under the same
	// serializable boundary InsertWithQuotaCheck uses. s.Slug names the
	// target; s.Identity is the connecting key.
	//
	//   - A slug that is not a site, and a site owned by another identity,
	//     both return domain.ErrNotFound: the same sentinel a missing slug
	//     yields, so "not yours" is indistinguishable from "does not exist".
	//   - The quota check is the REPLACE DELTA, evaluated against
	//     (existing_owned - old_deduped + dedupedSize) in the SAME critical
	//     section as the swap, so a same-size re-deploy does not
	//     double-count and a smaller one frees the diff. ErrServiceFull /
	//     ErrOverUserQuota on overflow.
	//   - On success manifest, deduped_size, updated_at, and expires_at are
	//     replaced from s and the expiry index is re-keyed; slug and
	//     created_at are unchanged. One transaction: the URL serves the old
	//     manifest until it lands, the new one immediately after.
	ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Site, error)
	// Delete removes a site by slug. Ownership is enforced by the caller, so
	// Delete itself takes no owner.
	Delete(domain.Slug) error
	// SumActiveBytesByOwner returns the identity's active SITE bytes. The
	// deploy path adds the paste-side sum to compute the budget the untar may
	// fill before the persistence-time check.
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
	// ListSitesByOwner returns the identity's active sites so the SSH `list`
	// verb can show them alongside pastes: a site counts against the shared
	// quota but never expires, so without this it silently consumes quota the
	// owner can neither see nor free. Read-time expiry filtered.
	ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error)
	// PreClaimSlug stakes a metadata-only single-shard claim on slug BEFORE
	// the deploy consumes the one-shot untar stream, so a transactional blob
	// path can stage every file under slug's shard and the manifest and blob
	// pointers co-route to the SAME shard at commit. It is a cheap existence
	// claim, NOT a blob reservation: no two-store coupling, no quota charge.
	// Returns domain.ErrSlugTaken if slug is already a paste or a site, so the
	// caller re-mints and re-claims before reading the body.
	//
	// On the standalone blob path the slug routes no blob, so this is a no-op
	// returning nil and the caller mints in the post-untar insert retry loop
	// where the authoritative insert is the collision authority.
	PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error
}

// SlugClaimReleaser is the compensating half of SiteRepo.PreClaimSlug: it drops
// a claim no record was committed under, so a deploy that aborts after staking
// one (unreadable archive, no web content, a failed commit) does not remove the
// slug from the namespace for good. The claim is durable and no other path
// undoes it.
//
// Optional rather than part of SiteRepo: a backend whose PreClaimSlug is a
// no-op has nothing to release, and the deploy path skips the compensation when
// the repo does not implement this.
//
// An implementation MUST NOT drop a claim a paste or site was actually
// committed under, and MUST be a no-op on an absent or foreign claim so a
// repeated release is harmless.
type SlugClaimReleaser interface {
	ReleaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) error
}

// PasteByteSummer is the slice of the paste repo the deploy path needs for
// its remaining-budget computation. internal/storage.PasteRepo satisfies it.
type PasteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
}

// DeploySite is the application service for a static-site upload. It
// safe-untars the archive, stores each file as a content-addressed blob,
// builds the manifest, and persists the Site, enforcing the per-identity quota
// BOTH mid-untar (the decompression-bomb guard) AND at persistence time (the
// atomic check in InsertWithQuotaCheck).
type DeploySite struct {
	Sites  SiteRepo
	Pastes PasteByteSummer
	Blob   BlobUnit
	Now    func() time.Time
	// Logger records outcomes the caller never sees: the compensating release
	// of a pre-claimed slug runs on the way out of a failed deploy and its own
	// failure must not replace the deploy's error. nil discards.
	Logger *log.Logger
}

func (d *DeploySite) logf(format string, args ...any) {
	if d.Logger != nil {
		d.Logger.Printf(format, args...)
	}
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

// ErrDeployFailed is the generic site-deploy failure for an unexpected backend
// error the deploy path cannot translate to a specific cause. Defensive: the
// pre-claim stages files under the manifest's own shard, so a cross-shard bind
// should never fire. If a routing regression makes one surface, this keeps the
// raw backend sentinel out of the SSH client's output; the operator sees the
// real error in the logs.
var ErrDeployFailed = errors.New("service: site deploy failed")

// maxDeployRetries bounds the slug-mint retry budget for both the pre-claim
// loop (transactional path) and the post-untar insert loop (standalone path).
const maxDeployRetries = 5

// Deploy reads a gzip-tar archive from body, untars it safely, stores the
// files, and persists a new Site owned by owner.
//
// body is the SAME live stream a single-file upload reads, consumed once
// mid-untar, so peak memory is one file at a time (buffered to hash +
// compress before the blob write), never the whole inflated archive.
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

	// The per-identity cap minus what the owner already has alive across
	// pastes AND sites. The untar's decompression-bomb guard aborts the
	// instant the running uncompressed total would cross this, so a site can
	// never be extracted (let alone persisted) over-quota.
	usedPaste, err := d.Pastes.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum paste bytes: %w", err)
	}
	usedSite, err := d.Sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum site bytes: %w", err)
	}
	budget := max(int64(domain.UserQuotaBytes)-int64(usedPaste)-usedSite, 0)

	ctx := context.Background()

	// On the TRANSACTIONAL blob path a file's blob pointer co-commits with the
	// manifest on the manifest's {slug} shard, so every file MUST stage under
	// the real slug or the bind cross-shards. The stream is one-shot and
	// cannot be re-untarred to re-route, so the slug is pre-claimed BEFORE the
	// untar. On the standalone path blobs are content-sha-keyed and the slug
	// routes nothing, so it stays empty here and is minted in the post-untar
	// insert retry loop.
	var slug domain.Slug
	committed := false
	if d.Blob.IsTransactional() {
		s, err := d.preClaimSlug(ctx, owner, now)
		if err != nil {
			return SiteResult{}, err
		}
		slug = s
		// The claim is durable and nothing else ever drops it, so every exit
		// from here that does not commit a site under slug has to undo it or
		// the slug leaves the namespace permanently. A deferred compensation
		// covers the untar, manifest and commit failures alike, plus a panic.
		defer func() {
			if !committed {
				d.releaseSlugClaim(ctx, slug, owner)
			}
		}()
	}

	sink := &blobSink{blob: d.Blob, slug: string(slug)}
	man, err := archive.Untar(body, sink, budget)
	switch {
	case errors.Is(err, domain.ErrArchiveTooLarge):
		return SiteResult{}, ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		return SiteResult{}, domain.ErrUnsupportedKind
	case err != nil:
		// A blob Put rejected by the object store's bucket quota propagates
		// through the sink and the classifier turns it into the graceful
		// "service is at capacity" response. ErrUnsafeArchive /
		// ErrTooManyFiles / ErrNoWebContent are unclassified and surface
		// verbatim so the SSH layer can message them precisely.
		_, terr := classifyCommitErr(err)
		return SiteResult{}, terr
	}

	if len(man.Files) == 0 {
		return SiteResult{}, ErrEmptySite
	}
	if !man.HasWebContent() {
		return SiteResult{}, domain.ErrNoWebContent
	}

	site := domain.Site{
		Slug:      slug, // empty on the standalone path; set below in the loop
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
	}
	deduped := man.CompressedDedupedSize()

	// Transactional path: the slug is pre-claimed and the files staged under
	// it, so the authoritative insert co-binds every pointer on the {slug}
	// shard in one CAS. One attempt suffices because the pre-claim already
	// holds the slot; the body is consumed, so a commit-time collision could
	// not re-untar anyway.
	if d.Blob.IsTransactional() {
		err := d.Blob.Commit(ctx, sink.handles, func(ctx context.Context) error {
			return d.Sites.InsertWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
		})
		res, ferr := finalizeDeploy(site, err)
		committed = ferr == nil
		return res, ferr
	}

	// Standalone path: mint the slug in a retry loop. The mid-untar guard
	// already bounded the bytes, but the persistence-time check closes the race
	// where two concurrent deploys from the same identity each pass the
	// pre-untar budget read and then both insert. The files are already durable
	// and content-sha-keyed, so re-minting on a collision needs no re-untar.
	for range maxDeployRetries {
		site.Slug = domain.NewRandomSlug()
		err := d.Blob.Commit(ctx, sink.handles, func(ctx context.Context) error {
			return d.Sites.InsertWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
		})
		switch class, terr := classifyCommitErr(err); class {
		case commitOK:
			return SiteResult{Site: site}, nil
		case commitSlugTaken:
			continue
		default:
			return SiteResult{}, terr
		}
	}
	return SiteResult{}, SlugTakenErr
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
	return d.Sites.Delete(slug)
}

// preClaimSlug mints a fresh random slug and stakes a metadata-only claim on
// it, retrying on collision until it lands a free slug or exhausts the budget.
// The returned slug is the shard every file stages under. The claim is a cheap
// existence stake (no blob reservation, no quota charge); the authoritative
// insert remains the final collision authority.
func (d *DeploySite) preClaimSlug(ctx context.Context, owner string, now time.Time) (domain.Slug, error) {
	for range maxDeployRetries {
		slug := domain.NewRandomSlug()
		err := d.Sites.PreClaimSlug(ctx, slug, owner, now)
		switch {
		case err == nil:
			return slug, nil
		case isSlugTaken(err):
			continue
		default:
			return "", err
		}
	}
	return "", SlugTakenErr
}

// releaseSlugClaim undoes a pre-claim no site was committed under. Best-effort:
// a failure re-leaks the claim (one slug of a 32^8 space) and must never
// replace the deploy error the caller is about to see. A backend whose
// PreClaimSlug is a no-op implements no releaser and needs none.
func (d *DeploySite) releaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) {
	rel, ok := d.Sites.(SlugClaimReleaser)
	if !ok {
		return
	}
	if err := rel.ReleaseSlugClaim(ctx, slug, owner); err != nil {
		d.logf("deploy: release slug claim %s: %v", slug, err)
	}
}

// finalizeDeploy translates the transactional insert's error into the deploy
// service's vocabulary. A cross-shard bind error, which the pre-claim is
// designed to prevent, is caught defensively and mapped to ErrDeployFailed so
// a raw backend sentinel never reaches the SSH client.
func finalizeDeploy(site domain.Site, err error) (SiteResult, error) {
	switch class, terr := classifyCommitErr(err); {
	case class == commitOK:
		return SiteResult{Site: site}, nil
	case class != commitOther:
		// Includes slug-taken, which surfaces as the clean SlugTakenErr: the
		// pre-claim holds the slot and the consumed stream cannot re-untar.
		return SiteResult{}, terr
	case isCrossShard(err):
		return SiteResult{}, fmt.Errorf("%w: %v", ErrDeployFailed, err)
	default:
		return SiteResult{}, err
	}
}

// isCrossShard reports whether err is domain.ErrCrossShardDeploy through any
// wrapping. The service never imports a backend package: the shale storage
// layer translates its backend's cross-shard guard error into the domain
// sentinel at the boundary, and this checks that. With the pre-claim in place
// a deploy's binds always co-route, so it should never match.
func isCrossShard(err error) bool {
	return errors.Is(err, domain.ErrCrossShardDeploy)
}

// DeployToSlug re-deploys a SITE at an existing owned slug in place. Same
// pipeline as Deploy, but it targets slug instead of minting one and charges
// the REPLACE DELTA against the owner's quota rather than the full new size.
//
// A slug that is not a site, or is a site owned by someone else, returns
// ErrNotFound: the same shape any not-found yields, so a non-owner cannot
// probe which slugs exist or who owns them. The mid-untar budget EXCLUDES this
// site's current deduped bytes, so a same-size re-deploy has the same headroom
// the original deploy did; ReplaceWithQuotaCheck applies the precise delta
// atomically.
//
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

	// Budget for the untar's decompression-bomb guard: the per-identity cap
	// minus everything else the owner holds (all pastes + all OTHER sites).
	// usedSite includes the target site, so its deduped bytes are credited
	// back below.
	usedPaste, err := d.Pastes.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum paste bytes: %w", err)
	}
	usedSite, err := d.Sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum site bytes: %w", err)
	}
	budget := siteExtractBudget(int64(domain.UserQuotaBytes), int64(usedPaste), usedSite, existing, now)

	sink := &blobSink{blob: d.Blob, slug: string(slug)}
	man, err := archive.Untar(body, sink, budget)
	switch {
	case errors.Is(err, domain.ErrArchiveTooLarge):
		return SiteResult{}, ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		return SiteResult{}, domain.ErrUnsupportedKind
	case err != nil:
		// Object-store bucket-quota rejections translate via the classifier;
		// untar-guard errors are unclassified and surface verbatim.
		_, terr := classifyCommitErr(err)
		return SiteResult{}, terr
	}

	if len(man.Files) == 0 {
		return SiteResult{}, ErrEmptySite
	}
	if !man.HasWebContent() {
		return SiteResult{}, domain.ErrNoWebContent
	}

	site := domain.Site{
		Slug:      slug,
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: existing.CreatedAt, // preserved across re-deploys
		UpdatedAt: now,
	}
	deduped := man.CompressedDedupedSize()

	// The swapped manifest and the staged-file binds commit as one unit.
	err = d.Blob.Commit(context.Background(), sink.handles, func(ctx context.Context) error {
		return d.Sites.ReplaceWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
	})
	switch class, terr := classifyCommitErr(err); {
	case class == commitOK:
		return SiteResult{Site: site}, nil
	case errors.Is(err, domain.ErrNotFound):
		// Deleted or expiry-swept between the ownership check and the swap.
		// Same shape the up-front check would have yielded.
		return SiteResult{}, ErrNotFound
	default:
		return SiteResult{}, terr
	}
}

// blobSink implements domain.FileSink: it buffers one file's bytes, hashes
// them (content-addressing is by UNCOMPRESSED bytes, matching the rest of the
// store, so identical files dedupe across deploys and sites), stages them
// through the blob seam, and returns the SHA the manifest references. It
// collects each staged handle so the deploy can Commit the manifest and the
// binds as one unit after the untar.
type blobSink struct {
	blob    BlobUnit
	slug    string
	handles []BlobHandle
}

// siteExtractBudget is the byte budget for a replace deploy's untar guard: the
// identity cap minus everything the owner holds, crediting back the site being
// replaced.
//
// The credit is StoredBytes, the figure the deploy charged and usedSite sums.
// It is NOT derivable from the manifest: per-entry compressed sizes are not
// persisted, so a loaded manifest's CompressedDedupedSize is 0 and crediting it
// would credit nothing, blocking an in-place update for an owner at their cap.
// The uncompressed DedupedSize is the opposite error, subtracting more than was
// ever charged and inflating the budget past the real remaining quota.
//
// An expired-but-unswept target is credited NOTHING: usedSite already excludes
// it, so crediting it would subtract bytes that were never counted.
func siteExtractBudget(cap, usedPaste, usedSite int64, existing domain.Site, now time.Time) int64 {
	// A site being replaced always credits its bytes back: nothing expires, so
	// an existing record is always live and always already counted.
	credit := int64(existing.StoredBytes)
	used := usedPaste + max(usedSite-credit, 0)
	return domain.Allowance{Cap: cap, Used: used}.Remaining()
}

func (s *blobSink) Store(p string, r io.Reader, _ int64) (string, int, error) {
	// Buffering the whole file is bounded: the untar's decompression-bomb
	// guard admitted these bytes against the running total, so this holds at
	// most one file and the whole site is at most the per-identity quota.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		// Propagate the cap-exceeded sentinel unchanged so SafeUntar can
		// distinguish it from a real I/O error.
		if errors.Is(err, domain.ErrArchiveTooLarge) {
			return "", 0, domain.ErrArchiveTooLarge
		}
		return "", 0, fmt.Errorf("read file %q: %w", p, err)
	}
	bodyBytes := buf.Bytes()
	sum := sha256.Sum256(bodyBytes)
	sha := hex.EncodeToString(sum[:])
	// StageEncoding reports the exact on-disk footprint, which is what the
	// deploy charges against quota. It excludes the 4-byte magic prefix, so
	// the basis matches how pastes charge their post-zstd size (compress.go).
	handle, compressedSize, err := s.blob.StageEncoding(context.Background(), s.slug, sha, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", 0, fmt.Errorf("blob put %q: %w", p, err)
	}
	s.handles = append(s.handles, handle)
	return sha, compressedSize, nil
}
