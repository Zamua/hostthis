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
	InsertWithQuotaCheck(s domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error
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
	Sites           SiteRepo
	Pastes          PasteByteSummer
	Blobs           BlobStore
	ServiceCapBytes int64 // 0 = no service-wide cap
	Now             func() time.Time
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
		err := d.Sites.InsertWithQuotaCheck(site, deduped, d.ServiceCapBytes, int64(domain.UserQuotaBytes), now)
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
