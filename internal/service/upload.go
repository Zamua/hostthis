// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteRepo is the persistence interface the upload service needs.
// internal/storage.PasteRepo satisfies it.
type PasteRepo interface {
	InsertWithQuotaCheck(p domain.Paste, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
	// MarkReady flips a pending paste to ready once its blob has landed.
	// MarkFailed flips a pending paste to failed and releases its
	// reservation. Both are guarded to advance only a still-pending paste
	// (so a late finalizer cannot resurrect a reconciler-failed paste) and
	// are no-ops on a missing or non-pending paste. Used by the async
	// finalizer; see docs/SPEC.md "Paste lifecycle status".
	MarkReady(domain.Slug) error
	MarkFailed(domain.Slug) error
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
	// Logger records background-finalize outcomes. The blob write runs
	// after Create has returned the URL, so a failure there cannot be
	// returned to the caller; it is logged + reflected in the paste
	// status. Optional - nil discards.
	Logger *log.Logger
	// onFinalizeDone, when non-nil, is called after each background
	// finalize completes (success or failure). Test seam so a test can
	// wait for the async half deterministically; nil in production.
	onFinalizeDone func()
}

// NewUpload wires defaults.
func NewUpload(repo PasteRepo, blobs BlobStore) *Upload {
	return &Upload{Repo: repo, Blobs: blobs, Now: time.Now}
}

func (u *Upload) logf(format string, args ...any) {
	if u.Logger != nil {
		u.Logger.Printf(format, args...)
	}
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
	// The blob write (the ~250 ms bottleneck) is deferred to a background
	// finalizer. Synchronously we only reserve quota + write the
	// authoritative paste row as PENDING, then return the URL. The held
	// bytes (staged.Body) live in this pod's memory until the finalizer
	// flushes them; a crash in that window loses them and the reconciler
	// ages the stuck pending to failed (docs/SPEC.md "Durability trade").
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Status:        domain.PasteStatusPending,
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
	// uploads can't both pass and both insert. Quota is enforced HERE,
	// synchronously, before any URL is handed out (docs/SPEC.md "Create:
	// the synchronous half").
	const maxRetries = 5
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.InsertWithQuotaCheck(p, int64(domain.UserQuotaBytes), now)
		switch {
		case err == nil:
			// Metadata committed as pending. Hand the URL back immediately
			// and finish the blob write in the background.
			u.startFinalize(p.Slug, staged.SHA, staged.Body)
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

// finalizeWG lets a graceful shutdown (or a test) wait for in-flight
// background finalizers to drain. Production wiring may Wait on it before
// exiting so a clean shutdown doesn't strand pending pastes the
// reconciler would otherwise have to age out.
var finalizeWG sync.WaitGroup

// WaitFinalize blocks until every background finalize started so far has
// completed. Intended for graceful shutdown + deterministic tests.
func (u *Upload) WaitFinalize() { finalizeWG.Wait() }

// startFinalize launches the background half of Create: write the blob,
// then flip the paste's status. It owns the goroutine so the SSH/HTTP
// caller never blocks on the ~250 ms blob write.
func (u *Upload) startFinalize(slug domain.Slug, sha string, body []byte) {
	finalizeWG.Go(func() {
		u.finalize(slug, sha, body)
		if u.onFinalizeDone != nil {
			u.onFinalizeDone()
		}
	})
}

// finalize writes the held bytes to the blob store and transitions the
// paste status: ready on success, failed (reservation released) on
// failure. The transitions are guarded in the repo so a finalize that
// races the reconciler's age-out cannot resurrect a failed paste. Errors
// are logged, not returned: the caller already has its URL.
func (u *Upload) finalize(slug domain.Slug, sha string, body []byte) {
	if err := u.Blobs.PutPrecompressed(sha, body); err != nil {
		// Blob write failed (object-store error, bucket quota, etc.). Flip
		// the paste to failed + release its reservation so it stops charging
		// quota and a read serves the error page.
		u.logf("upload: finalize %s: blob write failed: %v", slug, err)
		if ferr := u.Repo.MarkFailed(slug); ferr != nil {
			u.logf("upload: finalize %s: mark failed: %v", slug, ferr)
		}
		return
	}
	if err := u.Repo.MarkReady(slug); err != nil {
		// The bytes ARE durable; only the status flip failed. The reconciler
		// will not age this out (it is no longer pending only if the flip
		// landed) - a retry-on-read or the next finalize attempt is out of
		// scope here, so surface it loudly. A stuck-pending-with-durable-blob
		// is still served as a loading page until something flips it.
		u.logf("upload: finalize %s: mark ready: %v", slug, err)
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
