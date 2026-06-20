// Package shaleblob adapts a blob-capable storage.ShaleRepo to the
// service.BlobUnit seam, routing a record's blob lifecycle through the shale
// cluster's transactional blob plane (*cluster.BlobKV) so a staged blob's
// pointer co-commits with the metadata on the owning {slug} shard.
//
// It is the second service.BlobUnit implementation (the first is
// service.StandaloneBlobUnit, the detached content-addressed store). It lives
// in its OWN package because it depends on BOTH service (the seam) AND storage
// (the repo): service imports storage for its error sentinels, so storage
// cannot import service - this adapter package breaks the cycle. The cmd wiring
// picks it for a shale backend whose repo has a blob store configured
// (ShaleRepo.HasBlobPlane()).
//
// Canonical design: docs/design/shale-blobs-phase3.md + docs/SPEC.md
// "Shale-collocated blobs".
//
// The seam mapping (vs the standalone path):
//
//   - Stage / StageStream -> ShaleRepo.StageBlobStream -> BlobKV.StageBlob: the
//     bytes stream to the FINAL unit-keyed object OUTSIDE any transaction and
//     are reader-INVISIBLE until Commit binds the pointer. The returned
//     BlobHandle carries the opaque cluster.BlobRef in its Ref field.
//   - Commit -> stash the staged refs by slug, run the repo's existing metadata
//     write (which pops + BindBlobs them inside the authoritative {slug}
//     transaction), then clear the stash. The pointer co-commits with the row
//     in ONE single-shard CAS, so there is no pending window - the paste
//     commits READY directly (the pending/finalizer model collapses here; it
//     stays on the standalone path, see SPEC).
//   - Read / ReadAll -> ResolveBlobID(slug, sha) -> BlobKV.GetBlob, wrapped in
//     the SAME magic-peek + zstd decode the standalone GetReader uses, so the
//     read path sees decompressed bytes. ctx is tied to the reader's Close
//     (GetBlob streams lazily; ctx MUST outlive the reader).
//   - UnbindOnDelete -> a NO-OP: the delete-side unbind is folded INTO the
//     metadata-delete transaction (ShaleRepo.Delete / DeleteVersion /
//     DeleteSite already UnbindBlob the row's blobs atomically), so the seam's
//     post-delete hook has nothing to do.
//
// Build/runtime: -tags slatedb (it reaches *cluster.BlobKV through *ShaleRepo).

//go:build slatedb

package shaleblob

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// bytesReader is a tiny alias for bytes.NewReader to keep the Stage call sites
// readable.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Unit adapts a blob-capable ShaleRepo to the service.BlobUnit seam.
type Unit struct {
	repo *storage.ShaleRepo
}

// New builds the transactional shale-blob seam over repo. repo MUST have a blob
// store configured (repo.HasBlobPlane()); New returns an error otherwise,
// surfacing the wiring mistake at construction rather than at the first Stage.
func New(repo *storage.ShaleRepo) (*Unit, error) {
	if repo == nil || !repo.HasBlobPlane() {
		return nil, errors.New("shaleblob: New requires a ShaleRepo with a configured blob store (cfg.BlobStore)")
	}
	return &Unit{repo: repo}, nil
}

// Stage stages the already magic+zstd-encoded paste/version body and returns a
// handle carrying the staged cluster.BlobRef. The content sha is carried on the
// ref so the integrity hash lands in the persisted blob.Pointer (and the site
// path can rebuild its sha -> blob-id side-table).
func (u *Unit) Stage(ctx context.Context, slug, sha string, body []byte) (service.BlobHandle, error) {
	ref, err := u.repo.StageBlobStream(ctx, u.repo.RouteKeyForSlug(slug), bytesReader(body), int64(len(body)), sha)
	if err != nil {
		return service.BlobHandle{}, err
	}
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, nil
}

// StageStream stages a site-deploy file. The sink hands UNCOMPRESSED bytes
// (matching the standalone BlobStore.Put contract), but BlobKV.StageBlob streams
// verbatim and does NOT compress, so the repo's stage path encodes the body
// into the `magic + zstd(bytes)` at-rest format first (the SAME format the
// paste path and the standalone path produce), keeping the read decode uniform.
// The provided size (uncompressed) is unused; the staged size is the encoded
// length, computed inside StageBlobStream after encoding. size is accepted to
// satisfy the seam.
func (u *Unit) StageStream(ctx context.Context, slug, sha string, r io.Reader, _ int64) (service.BlobHandle, error) {
	body, err := storage.EncodeCompressedBody(r)
	if err != nil {
		return service.BlobHandle{}, err
	}
	ref, err := u.repo.StageBlobStream(ctx, u.repo.RouteKeyForSlug(slug), bytesReader(body), int64(len(body)), sha)
	if err != nil {
		return service.BlobHandle{}, err
	}
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, nil
}

// Commit carries the staged refs on a PER-CALL child context and runs the
// repo's metadata write under it; the authoritative {slug} write reads the refs
// off that context and binds them in the same transaction the row commits in.
// Because the refs ride the context (not a shared per-repo stash keyed by slug),
// two concurrent same-slug Commits each bind their OWN blobs - there is no
// window where one call's refs clobber or are clobbered by another's. metaWrite
// returns the slug-collision / quota error verbatim, so the caller's retry
// switch is unchanged. On a metaWrite error nothing is bound (the authoritative
// transaction committed both the row and the binds or neither), and any
// staged-but-unbound objects age out via SweepOrphans.
//
// The caller's metaWrite closure MUST thread the context it is handed into the
// repo's metadata-write method (the seam contract); passing a different context
// drops the binds.
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
	return metaWrite(storage.WithPendingBinds(ctx, refs))
}

// Read resolves the blob id for (slug, sha) from the metadata, streams the
// stored (compressed) object via GetBlob, and wraps it in the magic-peek + zstd
// decode so the caller sees decompressed bytes (identical contract to the
// standalone GetReader). ctx is tied to the returned reader's Close: GetBlob
// streams lazily and its ctx must outlive the reader, so Close cancels it after
// the body has been piped downstream. The int64 is the inner (stored) length,
// which the serve path does not use for Content-Length (it matches GetReader).
func (u *Unit) Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error) {
	blobID, err := u.repo.ResolveBlobID(domain.Slug(slug), sha)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, 0, blob.ErrNotFound
		}
		return nil, 0, err
	}
	// Scope a child ctx to the reader's lifetime: GetBlob's stream is lazy, so
	// the ctx must stay live until Close (not just until Read returns).
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

// ReadAll buffers the full decompressed blob for (slug, sha). Used by the
// markdown render + owner Show paths that need the whole document at once.
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
// unreferenced atomically with the row removal and there is nothing for the
// seam's post-delete hook to do.
func (u *Unit) UnbindOnDelete(_ context.Context, _ string, _ []string) error {
	return nil
}

// IsTransactional is true: a staged blob's pointer co-commits with the metadata
// in one {slug}-shard transaction (BindBlob inside the authoritative write), so
// a Stage->Commit makes the row and its bytes visible together. Upload.Create
// uses this to commit a paste READY directly (no pending row, no finalizer); the
// pending/finalizer model is collapsed on this path (docs/SPEC.md
// "Pending-collapse: a shale-collocated paste commits READY directly").
func (u *Unit) IsTransactional() bool { return true }

// ctxCancelReadCloser ties a context cancel to the reader's Close, so the
// GetBlob stream's bound ctx is canceled exactly when the caller is done piping
// the bytes (the LIFETIME contract on BlobKV.GetBlob). Close closes the inner
// reader first, then cancels.
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

// Ensure Unit satisfies the seam.
var _ service.BlobUnit = (*Unit)(nil)
