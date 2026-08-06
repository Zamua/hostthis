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
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/mime"
)

// PasteRepo is the persistence interface the upload service needs.
// internal/storage.PasteRepo satisfies it.
type PasteRepo interface {
	InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
	// MarkReady / MarkFailed advance a still-PENDING paste only (MarkFailed
	// also releases its reservation), so a late finalizer cannot resurrect a
	// reconciler-failed paste; both no-op on a missing or non-pending paste.
	// docs/SPEC.md "Paste lifecycle status".
	MarkReady(domain.Slug) error
	MarkFailed(domain.Slug) error
}

// BlobStore writes and reads content-addressed bytes. Put's size is required by
// S3-shaped backends to set Content-Length; the disk impl accepts it for
// interface uniformity. PutPrecompressed takes a body that is already
// magic-prefixed + zstd-encoded, so the streaming upload path does not re-encode
// its staging buffer. Get returns the full UNCOMPRESSED bytes in memory.
type BlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	PutPrecompressed(sha string, body []byte) error
	Get(sha string) ([]byte, error)

	// EncodeBody encodes UNCOMPRESSED bytes into the store's at-rest format and
	// reports the payload size EXCLUDING the framing prefix: the basis quota
	// charges. Declared here so a caller needing the on-disk footprint asks the
	// component that owns the format instead of redoing the framing arithmetic.
	EncodeBody(r io.Reader) (body []byte, payloadSize int, err error)
}

// SlugTakenErr is returned when the internal slug re-mint retry budget is
// exhausted.
var SlugTakenErr = errors.New("service: slug taken (after retries)")

// Upload is the application service for new paste creation.
type Upload struct {
	Repo PasteRepo
	// Blob is the per-record blob lifecycle seam (Stage/Commit/Read/
	// UnbindOnDelete). On the standalone path the metadata row commits first
	// and the finalizer Stages the bytes in the background.
	Blob BlobUnit
	Now  func() time.Time
	// Sniff is the port DetectKind's text-only rule calls: the domain owns the
	// rule, an adapter supplies the algorithm. Overridable so a test can drive
	// a branch without hunting for bytes that sniff a particular way.
	Sniff domain.MIMESniffer
	// finalizeWG tracks this instance's in-flight background finalizers.
	finalizeWG sync.WaitGroup
	// Logger records background-finalize outcomes: the blob write runs after
	// Create returned the URL, so a failure there cannot reach the caller.
	// nil discards.
	Logger *log.Logger
	// onFinalizeDone is called after each background finalize completes,
	// success or failure. Test seam for waiting on the async half; nil in
	// production.
	onFinalizeDone func()
	// SyncBlob makes Create commit the row READY and write the blob INLINE on
	// the ack path, skipping the pending/MarkReady flip. A BENCHMARK TOGGLE
	// (env HOSTTHIS_BLOB_SYNC) for A/B-ing sync against async on one binary,
	// not a production mode.
	SyncBlob bool
}

// NewUpload wires defaults.
func NewUpload(repo PasteRepo, blob BlobUnit) *Upload {
	return &Upload{Repo: repo, Blob: blob, Now: time.Now, Sniff: mime.Detect}
}

func (u *Upload) logf(format string, args ...any) {
	if u.Logger != nil {
		u.Logger.Printf(format, args...)
	}
}

// Result is what Create produced, for the SSH and HTTP layers to format.
type Result struct {
	Paste domain.Paste
}

// ErrOverQuota is returned when accepting the upload would push the identity's
// total active COMPRESSED bytes above UserQuotaBytes. The number is derived
// from the constant so the message cannot drift from the enforced limit.
var ErrOverQuota = fmt.Errorf("service: would exceed your %d MiB total quota; delete a paste to free space", domain.UserQuotaBytes>>20)

// ErrRawTooLarge is returned when the raw input exceeded the fast-fail cap.
// The server stopped reading before any compression check could run.
var ErrRawTooLarge = errors.New("service: upload too large to consider (raw input exceeded 100 MiB cap)")

// ErrCompressedTooLarge is returned when the input compressed under zstd to
// more than MaxPasteBytes.
var ErrCompressedTooLarge = errors.New("service: upload exceeds 10 MiB compressed cap")

// ErrServiceFull is the graceful translation of storage.ErrServiceFull: the
// object store rejected a blob Put because the bucket is at its configured
// hard quota (SPEC "Limits -> Durable total-bytes ceiling: an object-store
// quota").
var ErrServiceFull = errors.New("service: service is at capacity, try again later")

// Create persists a new paste owned by owner, a "key:<fp>" identity built from
// the uploader's ssh public key fingerprint. That identity gates quota: its
// active pastes plus this body cannot exceed UserQuotaBytes.
//
// body is consumed in a single streaming pass (hash + compress + count, peak
// memory ~MaxPasteBytes), and type detection runs on the captured 512-byte
// prefix so the source is never re-read. Unsupported types return
// domain.ErrUnsupportedKind for the caller to surface verbatim.
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
	kind, err := domain.DetectKind(staged.Prefix, typeHint, u.Sniff)
	if err != nil {
		return Result{}, err
	}
	// KindSite (a gzip-tar archive) must go through the deploy pipeline and its
	// untar guards. Reaching the single-file paste path with it would persist
	// the raw gzip as a paste and skip every one of them.
	if kind == domain.KindSite {
		return Result{}, domain.ErrUnsupportedKind
	}
	now := u.Now().UTC()
	if u.Blob.IsTransactional() {
		// Shale-collocated: the row and its bytes become visible together, so
		// there is no pending window and no finalizer (docs/SPEC.md
		// "Pending-collapse: a shale-collocated paste commits READY directly").
		// A staging failure means the row never commits.
		return u.createTransactional(staged, owner, name, kind, now)
	}
	// Detached-store path (sqlite / slatedb / shale-without-a-blob-bucket): the
	// blob write is deferred to a background finalizer, so only the quota
	// reserve and the PENDING row happen before the URL is returned. staged.Body
	// lives in this pod's memory until the finalizer flushes it; a crash in that
	// window loses the bytes and the reconciler ages the stuck pending to failed
	// (docs/SPEC.md "Durability trade").
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
	}
	// Retry on slug collision; SlugAlphabet's 32^8 slugs make a collision
	// inside 5 retries vanishingly unlikely. The quota check lives INSIDE
	// InsertWithQuotaCheck so concurrent uploads cannot both pass and both
	// insert, and it runs before any URL is handed out (docs/SPEC.md "Create:
	// the synchronous half").
	const maxRetries = 5
	for attempt := 1; attempt <= maxRetries; attempt++ {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.InsertWithQuotaCheck(context.Background(), p, int64(domain.UserQuotaBytes), now)
		switch class, terr := classifyCommitErr(err); class {
		case commitOK:
			if u.SyncBlob {
				// Benchmark path: the row is already committed ready, so write
				// the blob inline with no MarkReady flip.
				if _, berr := u.Blob.Stage(context.Background(), string(p.Slug), staged.SHA, staged.Body); berr != nil {
					u.logf("upload: sync blob write %s: %v", p.Slug, berr)
				}
				return Result{Paste: p}, nil
			}
			// Metadata committed as pending: hand the URL back and finish the
			// blob write in the background.
			u.startFinalize(p.Slug, staged.SHA, staged.Body)
			return Result{Paste: p}, nil
		case commitSlugTaken:
			// Logged so a remint burst shows up in the service log instead of
			// being silent.
			u.logf("upload: slug %s taken, re-minting (attempt %d/%d)", p.Slug, attempt, maxRetries)
			continue
		default:
			// The translated triad sentinel, or the raw error verbatim.
			return Result{}, terr
		}
	}
	return Result{}, SlugTakenErr
}

// createTransactional is the shale-collocated Create: stage the (already
// magic+zstd) body, then commit the row READY with the blob bound in the SAME
// transaction, so the bytes are durable before the metadata commit and a reader
// never sees a row without its blob. No pending status, no finalizer, no
// SyncBlob toggle on this path.
//
// Staging happens INSIDE the retry loop because the staged ref's route shard is
// captured from the slug: the bind co-commits with the row only when both use
// the SAME slug. A collision re-mints and re-stages, and the orphan sweep
// reclaims the first staged-but-unbound object.
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
	}
	ctx := context.Background()
	const maxRetries = 5
	for attempt := 1; attempt <= maxRetries; attempt++ {
		p.Slug = domain.NewRandomSlug()
		// Staging before the insert means a staging failure aborts WITHOUT a
		// metadata row and with no quota charged; the reserve still runs inside
		// InsertWithQuotaCheck, so an over-quota upload is still rejected.
		handle, err := u.Blob.Stage(ctx, string(p.Slug), staged.SHA, staged.Body)
		if err != nil {
			// A Put rejected by the bucket quota surfaces
			// storage.ErrServiceFull (the durable total-bytes ceiling).
			if class, terr := classifyCommitErr(err); class != commitOther {
				return Result{}, terr
			}
			return Result{}, fmt.Errorf("blob write: %w", err)
		}
		err = u.Blob.Commit(ctx, []BlobHandle{handle}, func(ctx context.Context) error {
			return u.Repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
		})
		switch class, terr := classifyCommitErr(err); class {
		case commitOK:
			return Result{Paste: p}, nil
		case commitSlugTaken:
			// Re-mint and re-stage so the new ref co-routes to the new slug's
			// shard; the prior staged object ages out via the orphan sweep.
			// Logged so a remint burst, which strands those objects, is
			// visible.
			u.logf("upload: slug %s taken, re-minting + re-staging (attempt %d/%d)", p.Slug, attempt, maxRetries)
			continue
		default:
			// The translated triad sentinel, or the raw error verbatim.
			return Result{}, terr
		}
	}
	return Result{}, SlugTakenErr
}

// WaitFinalize blocks until every background finalize this Upload started has
// completed, so a graceful shutdown does not strand pending pastes for the
// reconciler to age out.
//
// Per-instance. A package-level WaitGroup would make one Upload's WaitFinalize
// block on every other instance's in-flight work, coupling unrelated servers
// and any tests that run in parallel.
func (u *Upload) WaitFinalize() { u.finalizeWG.Wait() }

// startFinalize runs the background half of Create (write the blob, then flip
// the paste's status) so the SSH/HTTP caller never blocks on the blob write.
func (u *Upload) startFinalize(slug domain.Slug, sha string, body []byte) {
	u.finalizeWG.Go(func() {
		u.finalize(slug, sha, body)
		if u.onFinalizeDone != nil {
			u.onFinalizeDone()
		}
	})
}

// finalize writes the held bytes and transitions the paste: ready on success,
// failed (reservation released) otherwise. The repo guards the transitions so a
// finalize racing the reconciler's age-out cannot resurrect a failed paste.
// Errors are logged, not returned: the caller already has its URL.
func (u *Upload) finalize(slug domain.Slug, sha string, body []byte) {
	if _, err := u.Blob.Stage(context.Background(), string(slug), sha, body); err != nil {
		// Flip to failed and release the reservation so the paste stops
		// charging quota and a read serves the error page.
		u.logf("upload: finalize %s: blob write failed: %v", slug, err)
		if ferr := u.Repo.MarkFailed(slug); ferr != nil {
			u.logf("upload: finalize %s: mark failed: %v", slug, ferr)
		}
		return
	}
	if err := u.Repo.MarkReady(slug); err != nil {
		// The bytes ARE durable and only the status flip failed, so the paste
		// stays pending and is served as a loading page until something flips
		// it. Nothing retries the flip, so surface it loudly.
		u.logf("upload: finalize %s: mark ready: %v", slug, err)
	}
}

// isSlugTaken matches the slug-collision sentinel (domain.ErrSlugTaken, aliased
// as storage.ErrSlugTaken) by identity through any wrapping, never by message
// text: an unrelated error mentioning "slug" must surface verbatim rather than
// burn the remint budget.
func isSlugTaken(err error) bool {
	return errors.Is(err, domain.ErrSlugTaken)
}
