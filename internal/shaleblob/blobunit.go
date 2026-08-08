// Package shaleblob adapts a blob-capable storage.ShaleRepo to the
// service.BlobUnit seam, routing a record's blob lifecycle through the shale
// cluster's transactional blob plane so a staged blob's pointer co-commits with
// the metadata on the owning {slug} shard.
//
// Staged bytes stay reader-invisible until Commit binds the pointer, so there
// is no pending window on this path: the paste commits READY directly and the
// pending/finalizer model stays on the standalone path.
//
// Its own package because it depends on BOTH service and storage: service
// imports storage for error sentinels, so storage cannot import service.
//
// Design: docs/design/shale-blobs-phase3.md, docs/SPEC.md "Shale-collocated
// blobs".

//go:build slatedb

package shaleblob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Unit adapts a blob-capable ShaleRepo to the service.BlobUnit seam.
type Unit struct {
	repo *storage.ShaleRepo
	// Logf reports a best-effort failure the caller must not be failed for.
	// nil uses the standard logger; a deployment that threads its own logger
	// sets it at the composition root.
	Logf func(format string, args ...any)
}

// logf reports through Logf when set, and the standard logger otherwise, so a
// best-effort failure is never silent.
func (u *Unit) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// New builds the transactional shale-blob seam over repo, which MUST have a
// blob store configured. Erroring here surfaces the wiring mistake at
// construction rather than at the first Stage.
func New(repo *storage.ShaleRepo) (*Unit, error) {
	if repo == nil || !repo.HasBlobPlane() {
		return nil, errors.New("shaleblob: New requires a ShaleRepo with a configured blob store (cfg.BlobStore)")
	}
	return &Unit{repo: repo}, nil
}

// Stage stages an already magic+zstd-encoded paste/version body. The content
// sha rides the ref so the integrity hash lands in the persisted blob.Pointer
// and the site path can rebuild its sha -> blob-id side-table.
func (u *Unit) Stage(ctx context.Context, slug, sha string, body []byte) (service.BlobHandle, error) {
	ref, err := u.repo.StageBlobStream(ctx, u.repo.RouteKeyForSlug(slug), bytesReader(body), int64(len(body)), sha)
	if err != nil {
		return service.BlobHandle{}, err
	}
	if rerr := u.recordStaged(ctx, slug, ref); rerr != nil {
		return service.BlobHandle{}, rerr
	}
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, nil
}

// recordStaged remembers a ref the instant its bytes land, so a death before
// the commit leaves an exact list to reclaim rather than bytes nothing recorded.
//
// Per object rather than batched at the end: a batch loses precisely the blobs a
// mid-upload crash strands, which is the case this exists for.
//
// A failed record FAILS THE STAGE. Continuing would put bytes on disk that
// nothing remembers - the leak this is closing - and the upload can be retried,
// so refusing costs a retry where proceeding costs a permanent orphan.
func (u *Unit) recordStaged(ctx context.Context, slug string, ref cluster.BlobRef) error {
	if err := u.repo.RecordStagedRef(ctx, domain.Slug(slug), ref); err != nil {
		return fmt.Errorf("shaleblob: record staged ref for %s: %w", slug, err)
	}
	return nil
}

// StageStream stages a site-deploy file. The sink hands UNCOMPRESSED bytes but
// BlobKV.StageBlob streams verbatim, so the body is encoded into the
// `magic + zstd(bytes)` at-rest format here, keeping the read decode uniform
// with the paste and standalone paths. size is ignored (the staged length is
// the encoded one) and accepted only to satisfy the seam.
func (u *Unit) StageStream(ctx context.Context, slug, sha string, r io.Reader, _ int64) (service.BlobHandle, error) {
	body, err := storage.EncodeCompressedBody(r)
	if err != nil {
		return service.BlobHandle{}, err
	}
	ref, err := u.repo.StageBlobStream(ctx, u.repo.RouteKeyForSlug(slug), bytesReader(body), int64(len(body)), sha)
	if err != nil {
		return service.BlobHandle{}, err
	}
	if rerr := u.recordStaged(ctx, slug, ref); rerr != nil {
		return service.BlobHandle{}, rerr
	}
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, nil
}

// Commit carries the staged refs on a PER-CALL child context and runs metaWrite
// under it; the authoritative {slug} write reads them off that context and
// binds them in the transaction the row commits in. Riding the context rather
// than a per-repo stash keyed by slug is what lets two concurrent same-slug
// Commits each bind their OWN blobs. metaWrite's error surfaces verbatim, and
// on error nothing is bound (row and binds commit together or not at all) so
// staged-but-unbound objects age out via SweepOrphans.
//
// The metaWrite closure MUST thread the context it is handed into the repo's
// metadata-write method; passing a different one silently drops the binds.
func (u *Unit) Commit(ctx context.Context, handles []service.BlobHandle, metaWrite func(context.Context) error) error {
	if len(handles) == 0 {
		return metaWrite(ctx)
	}
	refs := make([]cluster.BlobRef, 0, len(handles))
	for _, h := range handles {
		ref, ok := h.Ref.(cluster.BlobRef)
		if !ok {
			return errors.New("shaleblob: Commit got a handle without a cluster.BlobRef (was it staged by this unit?)")
		}
		refs = append(refs, ref)
	}
	if err := metaWrite(storage.WithPendingBinds(ctx, refs)); err != nil {
		return err
	}
	// The write committed, so these bytes are bound. A staged record that
	// outlives its commit is a standing instruction to delete live data, so
	// clearing is part of committing, not housekeeping.
	//
	// Best-effort: the metadata is already durable and reporting failure here
	// would fail a write that succeeded. A record left behind is re-cleared by
	// the next resolution, which finds the blob bound and skips it.
	slug := handles[0].Slug
	if cerr := u.repo.ClearStagedRefs(ctx, domain.Slug(slug)); cerr != nil {
		u.logf("shaleblob: clearing staged records for %s: %v (the next resolution re-clears them)", slug, cerr)
	}
	return nil
}

// BeginUpload claims the slug's blob-ownership epoch and returns a context
// carrying it, so the commit can verify it still holds when it binds.
func (u *Unit) BeginUpload(ctx context.Context, slug string) (context.Context, error) {
	epoch, err := u.repo.ClaimBlobOwnership(ctx, domain.Slug(slug))
	if err != nil {
		return ctx, fmt.Errorf("shaleblob: claim blob ownership for %s: %w", slug, err)
	}
	return storage.WithBlobOwnerEpoch(ctx, epoch), nil
}

// Read streams the stored object for (slug, sha) through the magic-peek + zstd
// decode, so the caller sees decompressed bytes (same contract as the
// standalone GetReader). ctx is tied to the returned reader's Close because
// GetBlob streams lazily and its ctx must outlive the reader. The int64 is the
// inner stored length, not a Content-Length.
func (u *Unit) Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error) {
	blobID, err := u.repo.ResolveBlobID(domain.Slug(slug), sha)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, 0, blob.ErrNotFound
		}
		return nil, 0, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	rc, size, err := u.repo.GetBlobStream(streamCtx, u.repo.RouteKeyForSlug(slug), blobID)
	if err != nil {
		cancel()
		if errors.Is(err, blob.ErrNotFound) {
			return nil, 0, blob.ErrNotFound
		}
		return nil, 0, err
	}
	dec, derr := storage.DecodeCompressedStream(rc, blobID)
	if derr != nil {
		cancel() // DecodeCompressedStream already closed rc
		return nil, 0, derr
	}
	return &ctxCancelReadCloser{rc: dec, cancel: cancel}, size, nil
}

// ReadAll buffers the full decompressed blob, for the paths that need the
// whole document at once.
func (u *Unit) ReadAll(ctx context.Context, slug, sha string) ([]byte, error) {
	rc, _, err := u.Read(ctx, slug, sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

// UnbindOnDelete is a no-op: the unbind is folded into the metadata-delete
// transaction (ShaleRepo.Delete / DeleteVersion / DeleteSite), so the bytes go
// unreferenced atomically with the row removal.
func (u *Unit) UnbindOnDelete(_ context.Context, _ string, _ []string) error {
	return nil
}

// IsTransactional is true: BindBlob runs inside the authoritative {slug} write,
// so a Stage->Commit makes the row and its bytes visible together. Upload.Create
// keys off this to commit a paste READY directly, with no pending row and no
// finalizer (docs/SPEC.md "Pending-collapse: a shale-collocated paste commits
// READY directly").
func (u *Unit) IsTransactional() bool { return true }

// ctxCancelReadCloser cancels the stream's ctx on Close, satisfying the
// lifetime contract on BlobKV.GetBlob: the bound ctx must outlive the reader.
type ctxCancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *ctxCancelReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *ctxCancelReadCloser) Close() error {
	err := c.rc.Close()
	c.cancel()
	return err
}

var _ service.BlobUnit = (*Unit)(nil)

// StageEncoding encodes to the at-rest format, stages those exact bytes, and
// returns the compressed size EXCLUDING the framing prefix: the basis quota
// charges, matching how a paste charges its post-zstd size.
func (u *Unit) StageEncoding(ctx context.Context, slug string, r io.Reader) (service.BlobHandle, string, int, error) {
	// Encoded to a temp file first, NOT streamed with an unknown length.
	//
	// A pipe with blob.SizeUnknown looks like the streaming answer and is worse:
	// an object store that does not know the total allocates a full multipart
	// part buffer, which is fixed-size and can exceed the payload. Measured on a
	// live deployment, that cost 128 MiB of RSS growth for a 32 MiB upload
	// against 61 MiB for the buffering it was meant to replace.
	//
	// Spilling to disk keeps memory to the compressor window while still giving
	// the store an exact size, so it can pick a sane part size.
	f, err := os.CreateTemp(stagingDir(), "hostthis-stage-*")
	if err != nil {
		return service.BlobHandle{}, "", 0, fmt.Errorf("shaleblob: staging temp: %w", err)
	}
	defer os.Remove(f.Name()) //nolint:errcheck
	defer f.Close()           //nolint:errcheck

	size, sha, err := storage.EncodeCompressedTo(f, r)
	if err != nil {
		return service.BlobHandle{}, "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return service.BlobHandle{}, "", 0, fmt.Errorf("shaleblob: rewind staged temp: %w", err)
	}
	ref, err := u.repo.StageBlobStream(ctx, u.repo.RouteKeyForSlug(slug), f, size, sha)
	if err != nil {
		return service.BlobHandle{}, "", 0, err
	}
	if rerr := u.recordStaged(ctx, slug, ref); rerr != nil {
		return service.BlobHandle{}, "", 0, rerr
	}
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, sha, int(size) - storage.CompressedBodyPrefixLen, nil
}

// stagingDir is where an in-flight upload spills while it is encoded. Defaults
// to the OS temp dir; HOSTTHIS_STAGING_DIR points it at a volume with room when
// the container's writable layer is small.
func stagingDir() string { return os.Getenv("HOSTTHIS_STAGING_DIR") }
