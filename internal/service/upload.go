// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteRepo is the persistence interface the upload service needs.
// internal/storage.PasteRepo satisfies it.
type PasteRepo interface {
	InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
}

// BlobStore writes and reads content-addressed bytes. Put streams r
// to the backing store; size is the expected byte length (required by
// S3-shaped backends to set Content-Length, accepted by the disk impl
// for interface uniformity). PutPrecompressed writes a body that's
// already magic-prefixed + zstd-encoded - used by the streaming upload
// path so the staging buffer doesn't get re-encoded on its way down.
// Get returns the full UNCOMPRESSED bytes in memory.
type BlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	PutPrecompressed(sha string, body []byte) error
	Get(sha string) ([]byte, error)
}

// SlugTakenErr signals that the chosen slug already exists. The
// upload service retries internally on Insert; this is exported so
// tests can assert on it if the retry budget is exhausted.
var SlugTakenErr = errors.New("service: slug taken (after retries)")

// Upload is the application service for new paste creation.
type Upload struct {
	Repo            PasteRepo
	Blobs           BlobStore
	ServiceCapBytes int64 // 0 = no service-wide cap
	Now             func() time.Time
}

// NewUpload wires defaults. ServiceCap defaults to 0 (no cap); main
// flips it to the operator value.
func NewUpload(repo PasteRepo, blobs BlobStore) *Upload {
	return &Upload{Repo: repo, Blobs: blobs, Now: time.Now}
}

// Result captures what Create produced for the caller (SSH / HTTP)
// to format into a response.
type Result struct {
	Paste domain.Paste
}

// ErrOverQuota is returned when accepting the upload would push the
// identity's total active COMPRESSED bytes above UserQuotaBytes.
var ErrOverQuota = errors.New("service: would exceed your 10 MiB total quota; delete a paste or wait for one to expire")

// ErrRawTooLarge is returned when the raw input exceeded the
// 100 MiB hard fast-fail cap. The server stopped reading before any
// compression check could run.
var ErrRawTooLarge = errors.New("service: upload too large to consider (raw input exceeded 100 MiB cap)")

// ErrCompressedTooLarge is returned when the input compressed under
// zstd to more than MaxPasteBytes. Caller may want to surface the
// compressed-size number to the user; see Upload.Create.
var ErrCompressedTooLarge = errors.New("service: upload exceeds 10 MiB compressed cap")

// ErrServiceFull is returned when the service-wide disk cap is hit.
var ErrServiceFull = errors.New("service: service is at capacity, try again after the next expiry")

// Create persists a new paste owned by the given identity.
// The identity is a "key:<fp>" string built from the uploader's ssh
// public key fingerprint. The identity gates quota - sum of the
// identity's active pastes plus this body cannot exceed UserQuotaBytes.
//
// Reads body via the streaming pipeline (single pass: hash + compress
// + count, peak memory ~MaxPasteBytes). Type detection runs on the
// captured 512-byte prefix so we don't have to re-read the source.
//
// Unsupported types return domain.ErrUnsupportedKind so the caller
// can surface the right message verbatim.
func (u *Upload) Create(body io.Reader, owner string, name string, typeHint string) (Result, error) {
	staged, err := streamUpload(body)
	switch {
	case errors.Is(err, errRawCapExceeded):
		return Result{}, ErrRawTooLarge
	case errors.Is(err, errCompressedCapExceeded):
		return Result{}, ErrCompressedTooLarge
	case err != nil:
		return Result{}, fmt.Errorf("staging: %w", err)
	}
	if staged.RawSize == 0 {
		return Result{}, errors.New("empty upload")
	}
	kind, err := domain.DetectKind(staged.Prefix, typeHint)
	if err != nil {
		return Result{}, err
	}
	// KindSite (a gzip-tar archive) is NOT a paste: it must go through the
	// deploy pipeline (which runs the safe-untar guards). Reaching the
	// single-file paste path with KindSite would persist the raw gzip as a
	// paste and skip every untar guard, so reject it here.
	if kind == domain.KindSite {
		return Result{}, domain.ErrUnsupportedKind
	}
	now := u.Now().UTC()
	if err := u.Blobs.PutPrecompressed(staged.SHA, staged.Body); err != nil {
		return Result{}, fmt.Errorf("blob write: %w", err)
	}
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Kind:          kind,
		ContentSHA:    staged.SHA,
		Size:          staged.CompressedSize,
		Name:          name,
		PinnedVersion: 0, // unpinned by default - public URL follows the latest version
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	// Retry on slug collision. SlugAlphabet has 32^8 ≈ 1.1e12 distinct
	// slugs; collisions inside 5 retries are vanishingly unlikely.
	// The quota checks live inside InsertWithQuotaCheck so concurrent
	// uploads can't both pass and both insert.
	const maxRetries = 5
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.InsertWithQuotaCheck(p, u.ServiceCapBytes, int64(domain.UserQuotaBytes), now)
		switch {
		case err == nil:
			return Result{Paste: p}, nil
		case errors.Is(err, storage.ErrServiceFull):
			return Result{}, ErrServiceFull
		case errors.Is(err, storage.ErrOverUserQuota):
			return Result{}, ErrOverQuota
		case isSlugTaken(err):
			continue
		default:
			return Result{}, err
		}
	}
	return Result{}, SlugTakenErr
}

// isSlugTaken returns true if err is any flavor of "slug already
// exists in the repo." We sniff the error message to avoid the
// service layer importing the storage package directly - that would
// invert the dependency direction (service shouldn't know which
// concrete repo it's talking to).
func isSlugTaken(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "slug")
}
