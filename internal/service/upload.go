// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"context"
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
	InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error
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
	Repo PasteRepo
	// Blob is the per-record blob lifecycle seam (Stage/Commit/Read/
	// UnbindOnDelete). Upload uses Stage to write the paste's bytes; the
	// metadata row is committed via Repo (the pending model is preserved on
	// the standalone path: the row commits first, the finalizer Stages the
	// bytes in the background).
	Blob BlobUnit
	Now  func() time.Time
	// Logger records background-finalize outcomes. The blob write runs
	// after Create has returned the URL, so a failure there cannot be
	// returned to the caller; it is logged + reflected in the paste
	// status. Optional - nil discards.
	Logger *log.Logger
	// onFinalizeDone, when non-nil, is called after each background
	// finalize completes (success or failure). Test seam so a test can
	// wait for the async half deterministically; nil in production.
	onFinalizeDone func()
	// SyncBlob, when true, restores the pre-async behavior: Create writes
	// the paste row as ready + writes the blob INLINE (on the ack path) and
	// skips the pending/MarkReady status flip - so one metadata write + one
	// blob write, both synchronous. This is a BENCHMARK TOGGLE (env
	// HOSTTHIS_BLOB_SYNC) to A/B sync vs async on one binary; not a normal
	// production mode. Default false = the async path.
	SyncBlob bool
}

// NewUpload wires defaults.
func NewUpload(repo PasteRepo, blob BlobUnit) *Upload {
	return &Upload{Repo: repo, Blob: blob, Now: time.Now}
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
	if u.Blob.IsTransactional() {
		// Shale-collocated path: stage the bytes durably, then co-commit a
		// READY paste row with the blob bind in ONE transaction. No pending
		// window, no background finalizer - the row and its bytes become
		// visible together (docs/SPEC.md "Pending-collapse: a shale-collocated
		// paste commits READY directly"). On a staging failure the row never
		// commits and the SSH client gets the error.
		return u.createTransactional(staged, owner, name, kind, now)
	}
	// Detached-store path (sqlite / slatedb / shale-without-a-blob-bucket):
	// the blob write (the ~250 ms bottleneck) is deferred to a background
	// finalizer. Synchronously we only reserve quota + write the
	// authoritative paste row as PENDING, then return the URL. The held
	// bytes (staged.Body) live in this pod's memory until the finalizer
	// flushes them; a crash in that window loses them and the reconciler
	// ages the stuck pending to failed (docs/SPEC.md "Durability trade").
	// SyncBlob (benchmark toggle) commits the row as ready and writes the
	// blob inline below; the async path commits pending and finalizes in
	// the background.
	initialStatus := domain.PasteStatusPending
	if u.SyncBlob {
		initialStatus = domain.PasteStatusReady
	}
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Status:        initialStatus,
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
		err := u.Repo.InsertWithQuotaCheck(context.Background(), p, int64(domain.UserQuotaBytes), now)
		switch {
		case err == nil:
			if u.SyncBlob {
				// Benchmark sync path: row already committed as ready; write
				// the blob INLINE (on the ack path), no pending/MarkReady flip.
				// One metadata write + one blob write, both synchronous - the
				// pre-async shape.
				if _, berr := u.Blob.Stage(context.Background(), string(p.Slug), staged.SHA, staged.Body); berr != nil {
					u.logf("upload: sync blob write %s: %v", p.Slug, berr)
				}
				return Result{Paste: p}, nil
			}
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

// createTransactional is the shale-collocated Create: stage the (already
// magic+zstd) body, then commit the paste row as READY with the blob bound
// in the SAME transaction (Stage->Commit). There is no pending status, no
// async finalizer, and no SyncBlob toggle on this path - the staged bytes are
// durable BEFORE the metadata commit and the bind makes them visible together,
// so a reader never sees a row without its blob.
//
// The blob is staged INSIDE the retry loop with the chosen slug: the staged
// ref's route shard is captured from the slug at stage time, so the bind
// co-commits with the row only when both use the SAME slug. A (vanishingly
// rare) slug collision re-mints the slug and re-stages; the first staged-but-
// unbound object is reclaimed by the orphan sweep.
func (u *Upload) createTransactional(staged stagedUpload, owner, name string, kind domain.ContentKind, now time.Time) (Result, error) {
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Status:        domain.PasteStatusReady, // co-committed with the bind; no pending window
		Kind:          kind,
		ContentSHA:    staged.SHA,
		Size:          staged.CompressedSize,
		Name:          name,
		PinnedVersion: 0, // unpinned by default - public URL follows the latest version
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	ctx := context.Background()
	const maxRetries = 5
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		// Stage with the chosen slug so the ref co-routes to its shard, then
		// Commit binds it in the authoritative {slug} transaction. Staging
		// before the insert means a staging failure aborts WITHOUT a metadata
		// row (no quota charged), and the insert's quota reserve still runs
		// inside InsertWithQuotaCheck so an over-quota upload is rejected.
		handle, err := u.Blob.Stage(ctx, string(p.Slug), staged.SHA, staged.Body)
		if err != nil {
			// A blob Put rejected by the object store's bucket quota surfaces
			// storage.ErrServiceFull (the durable total-bytes ceiling).
			if errors.Is(err, storage.ErrServiceFull) {
				return Result{}, ErrServiceFull
			}
			return Result{}, fmt.Errorf("blob write: %w", err)
		}
		err = u.Blob.Commit(ctx, []BlobHandle{handle}, func(ctx context.Context) error {
			return u.Repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
		})
		switch {
		case err == nil:
			return Result{Paste: p}, nil
		case errors.Is(err, storage.ErrServiceFull):
			return Result{}, ErrServiceFull
		case errors.Is(err, storage.ErrOverUserQuota):
			return Result{}, ErrOverQuota
		case isSlugTaken(err):
			// Re-mint the slug and re-stage so the new ref co-routes to the
			// new slug's shard; the prior staged object ages out via the
			// orphan sweep.
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
	if _, err := u.Blob.Stage(context.Background(), string(slug), sha, body); err != nil {
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
