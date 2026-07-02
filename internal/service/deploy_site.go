package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// SiteRepo is the persistence interface the site-deploy + site-read
// services need. internal/storage.SiteRepo satisfies it.
type SiteRepo interface {
	InsertWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error
	// ReplaceWithQuotaCheck re-deploys an EXISTING owned site in place,
	// atomically swapping its manifest for s.Manifest under the same
	// serializable boundary InsertWithQuotaCheck uses. s.Slug names the
	// target; s.Identity is the connecting key. Behavior:
	//
	//   - The slug MUST already exist as a site AND be owned by s.Identity.
	//     If it does not exist as a site, OR exists but is owned by another
	//     identity, return storage.ErrNotFound - the SAME sentinel a
	//     missing slug yields, so the caller cannot distinguish "not yours"
	//     from "does not exist" (no existence leak).
	//   - The quota check is the REPLACE DELTA: the per-identity cap is
	//     evaluated against (existing_owned - old_deduped + dedupedSize),
	//     i.e. the old site's deduped bytes are credited back and the new
	//     ones charged in the SAME critical section as the swap. A same-size
	//     re-deploy does not double-count; a smaller one frees the diff.
	//     ErrServiceFull / ErrOverUserQuota on overflow.
	//   - On success the row's manifest, deduped_size, updated_at, and
	//     expires_at are all replaced from s (the retention clock restarts),
	//     and the expiry index is re-keyed. The slug and created_at are
	//     unchanged. The swap is a single transaction: the URL serves the
	//     OLD manifest until it lands, the new one immediately after.
	ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Site, error)
	// Delete removes a site by slug (its manifest + row). Ownership is
	// enforced by the caller (DeploySite.Delete reads the site and compares
	// Identity before calling this), so Delete itself takes no owner.
	Delete(domain.Slug) error
	// SumActiveBytesByOwner returns the identity's active SITE bytes.
	// The deploy path adds the paste-side sum to compute the remaining
	// quota budget the untar may fill before the persistence-time check.
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
	// ListSitesByOwner returns the identity's active (non-expired) sites so
	// the SSH `list` verb can show static sites alongside text pastes - a
	// site counts against the shared 10 MiB quota but never expires, so
	// without this it silently consumes quota the owner can neither see nor
	// free. Read-time expiry filtered.
	ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error)
	// PreClaimSlug stakes a metadata-only single-shard claim on slug BEFORE
	// the deploy consumes the (one-shot) untar stream, so a transactional
	// blob path can stage every file under slug's shard (the manifest and the
	// blob pointers then co-route to the SAME shard at commit). It is a cheap
	// existence claim, NOT a blob reservation: no two-store coupling, no quota
	// charge. It returns storage.ErrSlugTaken if slug is already a paste or a
	// site (so the caller re-mints and re-claims a fresh slug before reading
	// the body), nil if the claim succeeded.
	//
	// On the standalone (non-transactional) blob path the slug never routes a
	// blob, so this is a no-op returning nil: the caller mints the slug in the
	// post-untar insert retry loop instead, where the authoritative insert is
	// the collision authority. Only the transactional shale path calls it.
	PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error
}

// PasteByteSummer is the slice of the paste repo the deploy path needs:
// the identity's active paste bytes, summed for the remaining-budget
// computation. internal/storage.PasteRepo satisfies it.
type PasteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
}

// DeploySite is the application service for a static-site upload. It
// safe-untars the archive, stores each file as a content-addressed
// blob, builds the manifest, and persists the Site - enforcing the
// per-identity quota BOTH mid-untar (the decompression-bomb guard) AND
// at persistence time (the atomic check in InsertWithQuotaCheck).
type DeploySite struct {
	Sites  SiteRepo
	Pastes PasteByteSummer
	Blob   BlobUnit
	Now    func() time.Time
	// Retention stamps a deployed site's ExpiresAt (same policy as pastes).
	Retention domain.Retention
}

// NewDeploySite wires defaults. Retention defaults to the 30-day policy; the
// composition root overrides DeploySite.Retention from HOSTTHIS_RETENTION.
func NewDeploySite(sites SiteRepo, pastes PasteByteSummer, blob BlobUnit) *DeploySite {
	return &DeploySite{Sites: sites, Pastes: pastes, Blob: blob, Now: time.Now, Retention: domain.DefaultRetention()}
}

// ListSites returns the owner's active (non-expired) static sites so the SSH
// `list` verb can show them alongside text pastes. Owner-scoped; read-time
// expiry filtered by the repo. Anonymous / empty owners get an empty list
// (only keyed identities own content).
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

// ErrDeployFailed is the generic site-deploy failure surfaced when the
// authoritative write returns an unexpected backend error the deploy path
// cannot translate to a specific cause. It exists chiefly as a DEFENSIVE
// guard: with the pre-claim staging files under the manifest's own shard, a
// staged blob's pointer always co-routes with the row, so a cross-shard bind
// can no longer fire. Should one ever surface (a routing regression), this
// keeps the raw backend sentinel (e.g. shale's cross-shard guard error) from
// reaching the SSH client as an opaque internal string; the operator sees the
// real error in the logs.
var ErrDeployFailed = errors.New("service: site deploy failed")

// maxDeployRetries bounds the slug-mint retry budget for both the pre-claim
// loop (transactional path) and the post-untar insert loop (standalone path).
const maxDeployRetries = 5

// Deploy reads a gzip-tar archive from body, untars it safely, stores
// the files, and persists a new Site owned by owner.
//
// body is the SAME live stream a single-file upload reads - the SSH
// layer routes here after the format gate sniffed the gzip magic. The
// stream is consumed once, mid-untar, so peak memory is one file at a
// time (each file is buffered to hash + compress before the blob write),
// never the whole inflated archive.
//
// Returns:
//   - domain.ErrUnsupportedKind   - not a valid gzip-tar, or holds no
//     web content (same rejection as any unsupported upload)
//   - ErrOverQuota                - the archive expands past the owner's
//     remaining quota (caught mid-untar by the decompression-bomb guard,
//     or at persistence time by the atomic check)
//   - ErrTooManyFiles-family errors surfaced verbatim via domain
func (d *DeploySite) Deploy(body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()

	// Compute the remaining quota budget: the per-identity cap minus what
	// the owner already has alive across pastes AND sites. The untar's
	// decompression-bomb guard aborts the instant the running uncompressed
	// total would push the owner over this, so a site can never be
	// extracted (let alone persisted) over-quota.
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

	// Mint the slug. On the TRANSACTIONAL blob path (shale-collocated) a file's
	// blob pointer co-commits with the manifest on the manifest's {slug} shard,
	// so every file MUST stage under the real slug or the bind cross-shards.
	// The slug is therefore pre-claimed BEFORE the untar (the stream is one-shot;
	// we cannot re-untar to re-route), and the files stage under it. On the
	// standalone path the slug routes no blob (blobs are content-sha-keyed), so
	// it stays empty here and is minted in the post-untar insert retry loop where
	// the authoritative insert is the collision authority - byte-identical to the
	// pre-seam behavior.
	var slug domain.Slug
	if d.Blob.IsTransactional() {
		s, err := d.preClaimSlug(ctx, owner, now)
		if err != nil {
			return SiteResult{}, err
		}
		slug = s
	}

	// The sink stages each file under slug. On the transactional path slug is the
	// pre-claimed real slug, so the staged BlobRef's route shard matches the
	// manifest's {slug} shard and the bind co-commits. On the standalone path
	// slug is empty and unused (content-sha keying).
	sink := &blobSink{blob: d.Blob, slug: string(slug)}
	man, err := domain.SafeUntar(body, sink, budget)
	switch {
	case errors.Is(err, domain.ErrArchiveTooLarge):
		return SiteResult{}, ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		return SiteResult{}, domain.ErrUnsupportedKind
	case errors.Is(err, storage.ErrServiceFull):
		// A blob Put rejected by the object store's bucket quota propagates
		// through the sink (the durable total-bytes ceiling). Translate it
		// into the graceful "service is at capacity" response.
		return SiteResult{}, ErrServiceFull
	case err != nil:
		// ErrUnsafeArchive / ErrTooManyFiles / ErrNoWebContent surface
		// verbatim so the SSH layer can message them precisely.
		return SiteResult{}, err
	}

	if len(man.Files) == 0 {
		return SiteResult{}, ErrEmptySite
	}
	// A clean archive with no web content is not a site - reject as
	// unsupported, the same outcome as any non-renderable upload.
	if !man.HasWebContent() {
		return SiteResult{}, domain.ErrNoWebContent
	}

	site := domain.Site{
		Slug:      slug, // empty on the standalone path; set below in the loop
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: d.Retention.ExpiryFor(now),
	}
	deduped := man.CompressedDedupedSize()

	// Transactional path: the slug is pre-claimed and the files staged under it,
	// so the authoritative insert co-binds every pointer on the {slug} shard in
	// one CAS. The pre-claim holds the slot, so a commit-time collision is
	// effectively impossible; a single attempt suffices. (The body is consumed,
	// so a true collision here cannot re-untar - it surfaces, but the pre-claim
	// makes it astronomically unlikely.)
	if d.Blob.IsTransactional() {
		err := d.Blob.Commit(ctx, sink.handles, func(ctx context.Context) error {
			return d.Sites.InsertWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
		})
		return finalizeDeploy(site, err)
	}

	// Standalone path: persist with the atomic quota check, minting the slug in a
	// retry loop. The mid-untar guard already bounded the bytes, but the
	// persistence-time check closes the race where two concurrent deploys from the
	// same identity each pass the pre-untar budget read and then both insert. The
	// files are already durable from the sink's StageStream (content-sha-keyed,
	// slug-independent), so re-minting the slug on a collision needs no re-untar;
	// Commit just runs the InsertWithQuotaCheck.
	for range maxDeployRetries {
		site.Slug = domain.NewRandomSlug()
		err := d.Blob.Commit(ctx, sink.handles, func(ctx context.Context) error {
			return d.Sites.InsertWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
		})
		switch {
		case err == nil:
			return SiteResult{Site: site}, nil
		case errors.Is(err, storage.ErrServiceFull):
			return SiteResult{}, ErrServiceFull
		case errors.Is(err, storage.ErrOverUserQuota):
			return SiteResult{}, ErrOverQuota
		case isSlugTaken(err):
			continue
		default:
			return SiteResult{}, err
		}
	}
	return SiteResult{}, SlugTakenErr
}

// Delete removes an owned static site by slug. A non-site slug and a
// foreign-owned site both collapse to ErrNotFound - the same sentinel the
// paste-delete path returns - so existence/ownership never leaks. The verb
// dispatcher tries the paste delete first and falls through to this when the
// slug names a site rather than a paste.
func (d *DeploySite) Delete(slug domain.Slug, owner string) error {
	if owner == "" {
		return ErrEmptyOwner
	}
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
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
// it, retrying on a collision until it lands a free slug (or exhausts the
// budget). It is the BEFORE-untar half of the transactional deploy: the
// returned slug is the shard every file stages under. The claim is the cheap
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

// finalizeDeploy translates the transactional insert's error into the deploy
// service's vocabulary. A cross-shard bind error (which the pre-claim is
// designed to prevent) is caught defensively and mapped to ErrDeployFailed so a
// raw backend sentinel never reaches the SSH client.
func finalizeDeploy(site domain.Site, err error) (SiteResult, error) {
	switch {
	case err == nil:
		return SiteResult{Site: site}, nil
	case errors.Is(err, storage.ErrServiceFull):
		return SiteResult{}, ErrServiceFull
	case errors.Is(err, storage.ErrOverUserQuota):
		return SiteResult{}, ErrOverQuota
	case isSlugTaken(err):
		// The pre-claim holds the slot, so this is effectively unreachable; the
		// stream is consumed and cannot re-untar, so surface a clean failure.
		return SiteResult{}, SlugTakenErr
	case isCrossShard(err):
		return SiteResult{}, fmt.Errorf("%w: %v", ErrDeployFailed, err)
	default:
		return SiteResult{}, err
	}
}

// isCrossShard sniffs a backend cross-shard-guard error WITHOUT importing the
// backend package (the same dependency-direction discipline isSlugTaken keeps:
// the service layer must not know which concrete backend it talks to). Shale's
// cross-shard guard error message contains "cross-shard"; this is a defensive
// last line - with the pre-claim in place a deploy's binds always co-route, so
// it should never match.
func isCrossShard(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cross-shard")
}

// DeployToSlug re-deploys a SITE at an existing owned slug in place. It
// mirrors Deploy - same safe-untar, blob-store, manifest-build pipeline -
// but targets slug instead of minting a fresh random one, and charges the
// REPLACE DELTA against the owner's quota rather than the full new size.
//
// Ownership is verified up front (Get + identity compare); a slug that is
// not a site, or is a site owned by someone else, returns ErrNotFound -
// the SAME shape any not-found yields, so a non-owner cannot probe which
// slugs exist or who owns them. The mid-untar budget EXCLUDES this site's
// current deduped bytes (we are replacing them), so a same-size re-deploy
// has the same headroom the original deploy did; the persistence-time
// ReplaceWithQuotaCheck applies the precise replace-delta atomically.
//
// Returns:
//   - ErrEmptyOwner               - anonymous / empty identity
//   - ErrNotFound                 - slug is not a site owned by owner
//   - domain.ErrUnsupportedKind / domain.ErrNoWebContent - not web content
//   - ErrOverQuota / ErrServiceFull - over the per-identity / service cap
//   - ErrEmptySite                - the archive safe-untars to zero files
func (d *DeploySite) DeployToSlug(slug domain.Slug, body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()

	// Verify the slug names a site this identity owns BEFORE reading the
	// body. A non-site slug (Get -> ErrNotFound) and a foreign-owned site
	// both collapse to ErrNotFound so existence/ownership never leaks.
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return SiteResult{}, ErrNotFound
		}
		return SiteResult{}, fmt.Errorf("get site: %w", err)
	}
	if existing.Identity.String() != owner {
		return SiteResult{}, ErrNotFound
	}

	// Remaining quota budget for the untar's decompression-bomb guard. We
	// are REPLACING this site, so credit its current deduped bytes back:
	// the budget is the per-identity cap minus everything else the owner
	// holds (all pastes + all OTHER sites). usedSite includes the target
	// site, so subtract its deduped bytes to exclude it.
	usedPaste, err := d.Pastes.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum paste bytes: %w", err)
	}
	usedSite, err := d.Sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return SiteResult{}, fmt.Errorf("sum site bytes: %w", err)
	}
	// Credit the target site's current bytes back into the budget ONLY if
	// it is still live. usedSite already excludes an expired-but-unswept
	// row, so crediting an expired one would under-count and let the owner
	// extract past the cap. (ReplaceWithQuotaCheck applies the same gate at
	// persistence time, authoritatively.)
	creditOld := int64(existing.Manifest.DedupedSize())
	if !existing.ExpiresAt.After(now) {
		creditOld = 0
	}
	budget := max(int64(domain.UserQuotaBytes)-int64(usedPaste)-(usedSite-creditOld), 0)

	sink := &blobSink{blob: d.Blob, slug: string(slug)}
	man, err := domain.SafeUntar(body, sink, budget)
	switch {
	case errors.Is(err, domain.ErrArchiveTooLarge):
		return SiteResult{}, ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		return SiteResult{}, domain.ErrUnsupportedKind
	case errors.Is(err, storage.ErrServiceFull):
		// Object-store bucket quota rejection propagated through the sink.
		return SiteResult{}, ErrServiceFull
	case err != nil:
		return SiteResult{}, err
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
		ExpiresAt: d.Retention.ExpiryFor(now),
	}
	deduped := man.CompressedDedupedSize()

	// Commit the swapped manifest + bind the staged files as one unit. On the
	// standalone path the files are already durable from the sink; Commit
	// just runs the atomic ReplaceWithQuotaCheck swap.
	err = d.Blob.Commit(context.Background(), sink.handles, func(ctx context.Context) error {
		return d.Sites.ReplaceWithQuotaCheck(ctx, site, deduped, int64(domain.UserQuotaBytes), now)
	})
	switch {
	case err == nil:
		return SiteResult{Site: site}, nil
	case errors.Is(err, storage.ErrNotFound):
		// The site was deleted/expired-swept between the ownership check and
		// the swap. Surface as not-found, the same shape the up-front check
		// would have yielded.
		return SiteResult{}, ErrNotFound
	case errors.Is(err, storage.ErrServiceFull):
		return SiteResult{}, ErrServiceFull
	case errors.Is(err, storage.ErrOverUserQuota):
		return SiteResult{}, ErrOverQuota
	default:
		return SiteResult{}, err
	}
}

// blobSink implements domain.FileSink: it buffers one file's bytes,
// hashes them (content-addressing is by the file's uncompressed bytes,
// matching the rest of the store), stages them through the blob seam, and
// returns the SHA the manifest references. Identical files across
// deploys/sites dedupe for free at the blob layer.
//
// The bytes flow through the seam's StageStream (one file at a time), which
// on the standalone path is the compressing BlobStore.Put, so a site's
// files are zstd-compressed at rest exactly like a paste's bytes. The sink
// collects each staged handle so the deploy can Commit the manifest + binds
// as one unit after the untar.
type blobSink struct {
	blob    BlobUnit
	slug    string
	handles []BlobHandle
}

func (s *blobSink) Store(p string, r io.Reader, _ int64) (string, int, error) {
	// Read the whole (already-cap-bounded) file into memory: the untar's
	// decompression-bomb guard has admitted these bytes against the
	// running total, so this buffer is at most one file and the whole
	// site is at most the per-identity quota.
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
	// Compress with the SAME encoder the blob store uses and store the exact
	// bytes via the precompressed Stage path, so the returned compressed size
	// is precisely the on-disk footprint. That is what the deploy charges
	// against quota - matching how pastes charge their post-zstd size, so a
	// site no longer over-counts vs its real storage. The 4-byte magic prefix
	// is excluded to match the paste basis (compress.go).
	encoded, err := storage.EncodeCompressedBody(bytes.NewReader(bodyBytes))
	if err != nil {
		return "", 0, fmt.Errorf("compress file %q: %w", p, err)
	}
	compressedSize := len(encoded) - len(blobMagicV1)
	handle, err := s.blob.Stage(context.Background(), s.slug, sha, encoded)
	if err != nil {
		return "", 0, fmt.Errorf("blob put %q: %w", p, err)
	}
	s.handles = append(s.handles, handle)
	return sha, compressedSize, nil
}
