package service

import (
	"context"
	"io"
)

// BlobUnit is the per-record blob lifecycle seam: stage the bytes, commit them
// alongside the record's metadata, read them back, unbind them on delete,
// WITHOUT exposing the storage mechanism. Both the standalone detached-store
// path and the transactional shale path satisfy it, so upload / manage /
// deploy_site / http-read run against ONE shape regardless of backend.
//
// The "record" is a paste or a site, identified by its slug. A blob is
// identified within a record by its content sha. The seam maps (slug, sha) to
// an opaque BlobHandle the commit threads through; the services never inspect
// a handle's internals.
//
// The domain stays pure: BlobUnit and BlobHandle live in the service layer,
// the implementations in internal/storage, and the domain types never
// reference a handle.
type BlobUnit interface {
	// Stage durably writes a record's blob from an ALREADY magic+zstd-encoded
	// in-memory body. Used by the single-file paste/version paths, whose
	// streaming pipeline has already produced the encoded staging buffer.
	// Metadata-first ordering (for the pending model) is the caller's choice
	// of when to Stage relative to its metadata write.
	Stage(ctx context.Context, slug, sha string, body []byte) (BlobHandle, error)

	// StageEncoding encodes UNCOMPRESSED bytes into the adapter's at-rest
	// format, stages them, and reports the quota-relevant compressed size.
	//
	// It exists so a caller needing the on-disk footprint (the site deploy,
	// which charges the post-compression size against quota) does not have to
	// know the encoder or the framing. The adapter owns the format; the port
	// reports the number.
	StageEncoding(ctx context.Context, slug, sha string, r io.Reader) (BlobHandle, int, error)

	// StageStream durably writes a record's blob by streaming r
	// (UNCOMPRESSED; the storage layer compresses at rest). size is the
	// expected uncompressed length. Used by the site deploy path, which stores
	// one file at a time straight out of the safe-untar without buffering the
	// whole archive.
	StageStream(ctx context.Context, slug, sha string, r io.Reader, size int64) (BlobHandle, error)

	// Commit persists a record's metadata AND binds every staged handle as ONE
	// unit. On the standalone path Stage already wrote the bytes durably, so
	// the bind is a no-op and Commit just runs metaWrite. On the transactional
	// shale path Commit binds the handles in the SAME transaction metaWrite's
	// writes commit in.
	//
	// metaWrite receives a context.Context, NOT the ambient one: the shale
	// path derives a per-Commit child context carrying THIS call's staged
	// refs, so the metadata write binds exactly this call's blobs even when
	// two same-slug Commits run concurrently. The closure MUST thread the
	// context it is given into the repo's metadata-write method; passing a
	// different context silently drops the binds.
	//
	// metaWrite is the source of truth for slug-collision retries and quota
	// errors: Commit returns its error verbatim.
	Commit(ctx context.Context, handles []BlobHandle, metaWrite func(context.Context) error) error

	// Read streams a record's blob bytes, DECOMPRESSED, with the inner
	// (stored) byte length. The caller MUST Close the returned reader.
	Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error)

	// ReadAll returns a record's full DECOMPRESSED blob bytes buffered in
	// memory, for where the whole document is needed at once (markdown render,
	// owner-controlled Show).
	ReadAll(ctx context.Context, slug, sha string) ([]byte, error)

	// UnbindOnDelete removes a record's blob references as part of the
	// record's metadata delete. On the standalone path this is a NO-OP: the
	// bytes are content-addressed and reclaimed by the global sweep once no
	// live record references their sha, so a delete never removes bytes
	// directly. It exists so the services delete uniformly across backends.
	UnbindOnDelete(ctx context.Context, slug string, shas []string) error

	// IsTransactional reports whether this unit binds a record's blob inside
	// the SAME transaction the metadata commits in, so a Stage->Commit makes
	// the row and its bytes visible together.
	//
	// It is the one place a service path varies by backend capability:
	// Upload.Create uses it to choose between READY-direct (no pending window,
	// no finalizer) and the detached-store pending/finalizer model (PENDING
	// row first, bytes in the background, then flip to ready). A transactional
	// unit MUST honor the bind metaWrite threads off the Commit context. See
	// docs/SPEC.md "Pending-collapse: a shale-collocated paste commits READY
	// directly".
	IsTransactional() bool
}

// BlobHandle is the opaque token Stage / StageStream return and Commit
// consumes. Only handles a Stage call produced are valid to Commit; a zero
// BlobHandle is meaningless to the seam.
type BlobHandle struct {
	// Slug + SHA identify the staged blob on the standalone path. The field
	// set is internal to the seam and may grow without affecting callers,
	// which only ever pass handles back into Commit.
	Slug string
	SHA  string

	// Ref is the opaque, implementation-private staged-blob reference the
	// transactional shale path threads from Stage/StageStream into Commit. It
	// holds a cluster.BlobRef, typed as any so the service package stays free
	// of the shale/cluster dependency and its cgo/slatedb build tag. The
	// standalone path leaves it nil.
	Ref any
}
