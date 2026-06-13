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
// internal/storage.PasteRepo satisfies it. Delete is used only to roll
// back a metadata insert that committed while its concurrent blob Put
// failed (see Create): without it a failed blob would leave a usable
// paste whose read 500s on the missing blob. All three backends already
// expose Delete (it backs the manage / sweep paths).
type PasteRepo interface {
	InsertWithQuotaCheck(p domain.Paste, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
	Delete(domain.Slug) error
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
	Repo  PasteRepo
	Blobs BlobStore
	Now   func() time.Time
}

// NewUpload wires defaults.
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

// ErrServiceFull is returned when the durable total-bytes ceiling is hit:
// the object store rejects a blob Put because the bucket is at its
// configured hard quota (see SPEC "Limits -> Durable total-bytes ceiling:
// an object-store quota"). The blob store surfaces storage.ErrServiceFull,
// and the upload / deploy services translate it into this graceful
// "service is at capacity" response.
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

	// The blob Put (the ~250 ms object-store bottleneck) and the metadata
	// writes (~10-15 ms) are independent: the content SHA is known here,
	// before any metadata key is touched, and the blob is addressed by it.
	// Run them CONCURRENTLY so the wall-clock cost is the slower of the
	// two, not their sum (SPEC "Write-path concurrency"). The blob is
	// content-addressed and slug-independent, so it is Put exactly once
	// even when the metadata path re-rolls the slug on a collision.
	blobErrCh := make(chan error, 1)
	go func() {
		blobErrCh <- u.Blobs.PutPrecompressed(staged.SHA, staged.Body)
	}()

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
	// uploads can't both pass and both insert. This loop runs in the
	// foreground, overlapping the in-flight blob Put above.
	const maxRetries = 5
	var metaErr error
	inserted := false
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.InsertWithQuotaCheck(p, int64(domain.UserQuotaBytes), now)
		switch {
		case err == nil:
			inserted = true
		case isSlugTaken(err):
			continue
		default:
			metaErr = err
		}
		break
	}
	if !inserted && metaErr == nil {
		// All retries collided on a slug.
		metaErr = SlugTakenErr
	}

	// Join the blob Put. Both writes must succeed before the URL is
	// returned: the returned URL means "durably saved" exactly as before.
	blobErr := <-blobErrCh

	switch {
	case metaErr == nil && blobErr == nil:
		// Both durable: the paste is readable and its blob exists.
		return Result{Paste: p}, nil

	case metaErr == nil && blobErr != nil:
		// The metadata committed but the blob did not. A read would 500 on
		// the missing blob, so the paste must not survive: roll back the
		// insert (Delete removes the paste + versions + indexes and
		// releases the reserved bytes). Best-effort - a failed rollback
		// leaves a blob-less row the sweep removes at expiry, the same
		// fail-safe direction the rest of the write path takes.
		_ = u.Repo.Delete(p.Slug)
		return Result{}, translateBlobErr(blobErr)

	default:
		// metaErr != nil. Any blob written is now an orphan keyed by a SHA
		// no paste references; the blob-GC ref-set reclaims it on the next
		// sweep, exactly as before. Surface the metadata error.
		return Result{}, translateMetaErr(metaErr)
	}
}

// translateBlobErr maps a blob Put failure to the service-level sentinel.
// A bucket-quota rejection (storage.ErrServiceFull) becomes the graceful
// "service is at capacity" response; anything else is wrapped.
func translateBlobErr(err error) error {
	if errors.Is(err, storage.ErrServiceFull) {
		return ErrServiceFull
	}
	return fmt.Errorf("blob write: %w", err)
}

// translateMetaErr maps an InsertWithQuotaCheck failure to the
// service-level sentinel, preserving the sequential path's mapping.
func translateMetaErr(err error) error {
	switch {
	case errors.Is(err, storage.ErrServiceFull):
		return ErrServiceFull
	case errors.Is(err, storage.ErrOverUserQuota):
		return ErrOverQuota
	default:
		return err
	}
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
