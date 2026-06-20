package service

import (
	"context"
	"io"
)

// BlobUnit is the per-record blob lifecycle seam. It names the OPERATION
// the application services perform on a record's content bytes (stage the
// bytes, commit them alongside the record's metadata, read them back,
// unbind them on delete) WITHOUT exposing the storage mechanism. Both the
// standalone detached-store path and (in a later phase) the transactional
// shale path satisfy it, so upload / manage / deploy_site / http-read run
// against ONE shape regardless of backend.
//
// The "record" is a paste or a site, identified by its slug (the route
// key). A blob is identified within a record by its content sha (the app's
// existing content-addressing). The seam maps (slug, sha) -> an opaque
// BlobHandle the commit threads through; the services never inspect a
// handle's internals.
//
// The domain stays pure: BlobUnit and BlobHandle live in the service
// layer, the concrete implementations live in internal/storage, and the
// domain types (Paste, Site, Manifest, Version) never reference a handle.
type BlobUnit interface {
	// Stage durably writes a record's blob from an ALREADY magic+zstd-
	// encoded in-memory body and returns an opaque handle to commit. Used
	// by the single-file paste/version paths whose streaming pipeline has
	// already produced the encoded staging buffer. On the standalone path
	// this is the existing precompressed write to the detached store; the
	// bytes are immediately content-addressable, and metadata-first
	// ordering (for the pending model) is preserved by the caller choosing
	// when to Stage relative to its metadata write.
	Stage(ctx context.Context, slug, sha string, body []byte) (BlobHandle, error)

	// StageStream durably writes a record's blob by streaming r (UNCOMPRESSED
	// bytes; the storage layer compresses them at rest) and returns a handle
	// to commit. size is the expected uncompressed length. Used by the site
	// deploy path, which stores one file at a time straight out of the
	// safe-untar without buffering the whole archive.
	StageStream(ctx context.Context, slug, sha string, r io.Reader, size int64) (BlobHandle, error)

	// Commit persists a record's metadata AND binds every staged handle as
	// ONE unit. metaWrite runs the repo's existing per-record metadata write
	// (InsertWithQuotaCheck / AppendVersionWithQuotaCheck / ...). On the
	// standalone path the bytes were already durably written by Stage /
	// StageStream, so a bind is a no-op and Commit simply runs metaWrite -
	// byte-identical to the service calling the repo directly after the blob
	// write, which is exactly the pre-seam blob-first ordering for updates
	// and deploys. On the transactional shale path Commit binds the handles
	// in the SAME transaction metaWrite's writes commit in.
	//
	// metaWrite receives a context.Context, NOT the ambient one: the shale
	// path derives a per-Commit child context carrying THIS call's staged
	// refs and hands it to metaWrite, so the metadata write it triggers binds
	// exactly this call's blobs even when two same-slug Commits run
	// concurrently. The closure MUST thread the context it is given into the
	// repo's metadata-write method (so the refs reach the authoritative
	// transaction); passing a different context drops the binds. On the
	// standalone path the context is the ambient one and carries no refs.
	//
	// metaWrite is the source of truth for slug-collision retries and quota
	// errors: Commit returns metaWrite's error verbatim so the caller's
	// existing retry/translate switch is unchanged.
	Commit(ctx context.Context, handles []BlobHandle, metaWrite func(context.Context) error) error

	// Read streams a record's blob bytes, DECOMPRESSED, with the inner
	// (stored) byte length. The caller MUST Close the returned reader. This
	// is the streaming read path (HTML paste serve, site-file serve).
	Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error)

	// ReadAll returns a record's full DECOMPRESSED blob bytes buffered in
	// memory. Used where the whole document is needed at once (markdown
	// render, owner-controlled Show).
	ReadAll(ctx context.Context, slug, sha string) ([]byte, error)

	// UnbindOnDelete removes a record's blob references as part of the
	// record's metadata delete. On the standalone path this is a NO-OP: the
	// bytes are content-addressed and reclaimed by the global sweep when no
	// live record references their sha, so a delete never removes bytes
	// directly. Exposed so the services delete uniformly across backends;
	// the transactional shale path (a later phase) unbinds the pointers in
	// the metadata-delete transaction.
	UnbindOnDelete(ctx context.Context, slug string, shas []string) error
}

// BlobHandle is the opaque token Stage / StageStream return and Commit
// consumes. The service threads it from one to the other without inspecting
// it. The standalone path carries (slug, sha); the shale path carries a
// cluster blob reference in Ref. A zero BlobHandle is meaningless to the
// seam - only handles a Stage call produced are valid to Commit.
type BlobHandle struct {
	// Slug + SHA identify the staged blob on the standalone path. Other
	// implementations populate their own fields; the field set is internal
	// to the seam and may grow without affecting callers, which only ever
	// pass handles back into Commit.
	Slug string
	SHA  string

	// Ref is the OPAQUE, implementation-private staged-blob reference the
	// transactional shale path threads from Stage/StageStream into Commit.
	// It holds a cluster.BlobRef (typed as any so the service package stays
	// free of the shale/cluster dependency and its cgo/slatedb build tag).
	// The standalone path leaves it nil; the services never inspect it. The
	// ShaleBlobUnit type-asserts it back to a cluster.BlobRef in Commit.
	Ref any
}
