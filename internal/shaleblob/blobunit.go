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
	"io"

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
	return service.BlobHandle{Slug: slug, SHA: sha, Ref: ref}, nil
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
	return metaWrite(storage.WithPendingBinds(ctx, refs))
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
func (u *Unit) StageEncoding(ctx context.Context, slug, sha string, r io.Reader) (service.BlobHandle, int, error) {
	body, err := storage.EncodeCompressedBody(r)
	if err != nil {
		return service.BlobHandle{}, 0, err
	}
	h, err := u.Stage(ctx, slug, sha, body)
	if err != nil {
		return service.BlobHandle{}, 0, err
	}
	return h, len(body) - storage.CompressedBodyPrefixLen, nil
}
