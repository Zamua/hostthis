// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"bufio"
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
	MarkReady(domain.Paste) error
	MarkFailed(domain.Paste) error
}

// BlobStore owns the at-rest encoding and writes content-addressed bytes.
// Known lengths let S3-shaped backends avoid unknown-size multipart buffering.
type BlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	PutPrecompressed(sha string, r io.Reader, size int64) error
	EncodeTo(w io.Writer, r io.Reader) (sha string, payloadSize int, totalSize int64, err error)
}

// ErrSlugTaken is returned when the internal slug re-mint retry budget is
// exhausted.
var ErrSlugTaken = errors.New("service: slug taken (after retries)")

// Upload is the application service for new paste creation.
type Upload struct {
	Repo PasteRepo
	// Blob is the content-addressed byte plane.
	Blob BlobUnit
	Now  func() time.Time
	// Sniff is the domain.MIMESniffer port. Overridable so a test can drive a
	// branch without hunting for bytes that sniff a particular way.
	Sniff domain.MIMESniffer
	// Archive handles the multi-file shape. nil disables archive uploads, which
	// is what a deploy with no site support wants.
	Archive ArchiveDeployer
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

// ArchiveDeployer is the multi-file half of an upload, injected so Create can
// own the whole decision rather than leaving it to the transport.
//
// It is a PORT, not the concrete deploy service: upload does not import it, so
// the two can be merged without a circular dependency and the transport keeps
// one entry point either way (docs/SPEC.md "One paste, not two aggregates").
type ArchiveDeployer interface {
	Deploy(body io.Reader, owner string) (Result, error)
}

// archivePeek is the buffered-reader size for the gzip-magic peek. Only 2
// bytes are examined; the buffer just has to be large enough that the peek
// cannot short-read.
const archivePeek = 512

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
// body is consumed in a single streaming pass; type detection runs on the
// captured prefix so the source is never re-read. Unsupported types return
// domain.ErrUnsupportedKind for the caller to surface verbatim.
func (u *Upload) Create(body io.Reader, owner string, name string, typeHint string) (Result, error) {
	// A gzip-tar archive is the SAME paste at a different cardinality, so the
	// decision is made here, once, rather than by each transport (docs/SPEC.md
	// "One paste, not two aggregates"). It must precede staging, which consumes
	// the stream; the peek is non-destructive.
	if u.Archive != nil && typeHint == "" {
		peeked := bufio.NewReaderSize(body, archivePeek)
		head, _ := peeked.Peek(2)
		if domain.HasGzipMagic(head) {
			return u.Archive.Deploy(peeked, owner)
		}
		body = peeked
	}

	staged, err := streamUpload(body)
	// Freed here EXCEPT where ownership transfers to the finalize goroutine,
	// which discards it itself once the background write is done.
	transferred := false
	defer func() {
		if !transferred {
			staged.discard()
		}
	}()
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
	// Reaching here with an archive means the peek above did not catch it -
	// only possible when no ArchiveDeployer is wired. Persisting the raw gzip
	// as a single document would skip every untar guard.
	if kind == domain.KindSite {
		return Result{}, domain.ErrUnsupportedKind
	}
	now := u.Now().UTC()
	status := domain.PasteStatusPending
	if u.SyncBlob {
		status = domain.PasteStatusReady
	}
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Generation:    domain.NewPasteGeneration(),
		Status:        status,
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
		ctx := context.Background()
		if status == domain.PasteStatusReady {
			// The benchmark path writes bytes before metadata. A failed write
			// therefore cannot leave a ready row pointing at absent content.
			if err := u.Blob.StagePrecompressed(ctx, staged.SHA, staged.File, staged.encodedSize()); err != nil {
				if class, terr := classifyCommitErr(err); class != commitOther {
					return Result{}, terr
				}
				return Result{}, fmt.Errorf("blob write: %w", err)
			}
		}
		err := u.Repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
		switch class, terr := classifyCommitErr(err); class {
		case commitOK:
			if status == domain.PasteStatusPending {
				// The bytes land after the row, so the caller gets its URL now
				// and the finalizer flips the status when they are durable.
				// Ownership of the spill file transfers to the goroutine.
				transferred = true
				u.startFinalize(p, staged)
			}
			return Result{Paste: p}, nil
		case commitSlugTaken:
			u.logf("upload: slug %s taken, re-minting (attempt %d/%d)", p.Slug, attempt, maxRetries)
			continue
		default:
			// The translated triad sentinel, or the raw error verbatim.
			return Result{}, terr
		}
	}
	return Result{}, ErrSlugTaken
}

// WaitFinalize blocks until every background finalize this Upload started has
// completed, so a graceful shutdown does not strand pending pastes for the
// reconciler to age out. Per-instance: a package-level WaitGroup would couple
// unrelated servers and parallel tests.
func (u *Upload) WaitFinalize() { u.finalizeWG.Wait() }

// startFinalize runs the background half of Create (write the blob, then flip
// the paste's status) so the SSH/HTTP caller never blocks on the blob write.
func (u *Upload) startFinalize(paste domain.Paste, staged stagedUpload) {
	u.finalizeWG.Go(func() {
		// The goroutine owns the spill file: the request has already returned.
		defer staged.discard()
		u.finalize(paste, staged)
		if u.onFinalizeDone != nil {
			u.onFinalizeDone()
		}
	})
}

// finalize writes the staged bytes and transitions the paste: ready on success,
// failed (reservation released) otherwise. The repo guards the transitions so a
// finalize racing the reconciler's age-out cannot resurrect a failed paste.
// Errors are logged, not returned: the caller already has its URL.
//
// Streams from the spill file: buffering would make resident memory scale with
// concurrent uploads times payload size (docs/SPEC.md "Writes are
// constant-memory").
func (u *Upload) finalize(paste domain.Paste, staged stagedUpload) {
	if err := u.Blob.StagePrecompressed(context.Background(), staged.SHA, staged.File, staged.encodedSize()); err != nil {
		// Flip to failed and release the reservation so the paste stops
		// charging quota and a read serves the error page.
		u.logf("upload: finalize %s: blob write failed: %v", paste.Slug, err)
		if ferr := u.Repo.MarkFailed(paste); ferr != nil {
			u.logf("upload: finalize %s: mark failed: %v", paste.Slug, ferr)
		}
		return
	}
	if err := u.Repo.MarkReady(paste); err != nil {
		// The bytes ARE durable and only the status flip failed, so the paste
		// stays pending and is served as a loading page until something flips
		// it. Nothing retries the flip, so surface it loudly.
		u.logf("upload: finalize %s: mark ready: %v", paste.Slug, err)
	}
}
