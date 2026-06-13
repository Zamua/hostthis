package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// SiteRepo is the persistence interface the site-deploy + site-read
// services need. internal/storage.SiteRepo satisfies it.
type SiteRepo interface {
	InsertWithQuotaCheck(s domain.Site, dedupedSize int, userCap int64, now time.Time) error
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
	//     expires_at are all replaced from s (the 7-day clock restarts),
	//     and the expiry index is re-keyed. The slug and created_at are
	//     unchanged. The swap is a single transaction: the URL serves the
	//     OLD manifest until it lands, the new one immediately after.
	ReplaceWithQuotaCheck(s domain.Site, dedupedSize int, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Site, error)
	// SumActiveBytesByOwner returns the identity's active SITE bytes.
	// The deploy path adds the paste-side sum to compute the remaining
	// quota budget the untar may fill before the persistence-time check.
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
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
	Blobs  BlobStore
	Now    func() time.Time
}

// NewDeploySite wires defaults.
func NewDeploySite(sites SiteRepo, pastes PasteByteSummer, blobs BlobStore) *DeploySite {
	return &DeploySite{Sites: sites, Pastes: pastes, Blobs: blobs, Now: time.Now}
}

// SiteResult is what Deploy produced for the SSH layer to format.
type SiteResult struct {
	Site domain.Site
}

// ErrEmptySite is returned when an archive safe-untars to zero files.
var ErrEmptySite = errors.New("service: archive contains no files")

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
	budget := int64(domain.UserQuotaBytes) - int64(usedPaste) - usedSite
	if budget < 0 {
		budget = 0
	}

	sink := &blobSink{blobs: d.Blobs}
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
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
	deduped := man.DedupedSize()

	// Persist with the atomic quota check. The mid-untar guard already
	// bounded the bytes, but the persistence-time check closes the race
	// where two concurrent deploys from the same identity each pass the
	// pre-untar budget read and then both insert.
	const maxRetries = 5
	for range maxRetries {
		site.Slug = domain.NewRandomSlug()
		err := d.Sites.InsertWithQuotaCheck(site, deduped, int64(domain.UserQuotaBytes), now)
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
	budget := int64(domain.UserQuotaBytes) - int64(usedPaste) - (usedSite - creditOld)
	if budget < 0 {
		budget = 0
	}

	sink := &blobSink{blobs: d.Blobs}
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
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
	deduped := man.DedupedSize()

	err = d.Sites.ReplaceWithQuotaCheck(site, deduped, int64(domain.UserQuotaBytes), now)
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
// matching the rest of the store), writes them to the content-addressed
// BlobStore, and returns the SHA the manifest references. Identical
// files across deploys/sites dedupe for free at the blob layer.
//
// The bytes flow through the compressing BlobStore.Put (one file at a
// time), so a site's files are zstd-compressed at rest exactly like a
// paste's bytes.
type blobSink struct {
	blobs BlobStore
}

func (s *blobSink) Store(p string, r io.Reader, _ int64) (string, error) {
	// Read the whole (already-cap-bounded) file into memory: the untar's
	// decompression-bomb guard has admitted these bytes against the
	// running total, so this buffer is at most one file and the whole
	// site is at most the per-identity quota.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		// Propagate the cap-exceeded sentinel unchanged so SafeUntar can
		// distinguish it from a real I/O error.
		if errors.Is(err, domain.ErrArchiveTooLarge) {
			return "", domain.ErrArchiveTooLarge
		}
		return "", fmt.Errorf("read file %q: %w", p, err)
	}
	bodyBytes := buf.Bytes()
	sum := sha256.Sum256(bodyBytes)
	sha := hex.EncodeToString(sum[:])
	if err := s.blobs.Put(sha, bytes.NewReader(bodyBytes), int64(len(bodyBytes))); err != nil {
		return "", fmt.Errorf("blob put %q: %w", p, err)
	}
	return sha, nil
}
