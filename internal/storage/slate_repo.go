// Package storage's SlateDB-backed metadata implementation.
//
// This file is the SlateDB-backed twin of paste_repo.go (sqlite). Both
// implement the same service-layer interfaces (PasteAdmin, PasteRepo,
// SweepRepo, PasteReader, KeyGateRepo), so cmd/hostthisd picks one
// via the HOSTTHIS_METADATA_BACKEND env var and the rest of the app
// is unaware of the choice. See docs/SPEC.md "Metadata storage
// backends" for the canonical description.
//
// Build/runtime requirement: cgo + the slatedb_uniffi shared library
// on the platform loader path. See README + Dockerfile.
//
// KEY LAYOUT (every key is UTF-8 string converted to []byte):
//
//   pastes/<slug>                      JSON of paste row (the head)
//   versions/<slug>/<ver_num>          JSON of version row (including deleted=bool)
//   slug_owner/<slug>                  identity string (small; for visitor-side lookup)
//   identity_quota/<identity>          int64 big-endian (active compressed bytes)
//   identity_pastes/<identity>/<slug>  empty (for "list by identity" prefix scan)
//   identity_first_seen/<identity>     RFC3339 timestamp (for whoami)
//   expiry/<rfc3339>/<slug>            empty (for sweep prefix scan)
//   keygate/<subnet>/<identity>        RFC3339 first-seen timestamp
//
// ATOMICITY: every multi-key write uses a SlateDB WriteBatch (via
// Db.Write) OR a snapshot-isolation transaction (via Db.Begin) so the
// invariants documented in SPEC.md "Atomicity contract" hold. See
// each method's doc-comment for which mechanism it uses and why.
//
// IMPLEMENTATION STATUS: SCAFFOLDING ONLY. Methods return ErrNotImplemented
// until the corresponding logic is filled in. See infra/TODO.md for the
// implementation checklist.

//go:build slatedb

package storage

import (
	"errors"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrSlateNotImplemented is returned by every SlateRepo method until
// the implementation lands. Lets us validate interface conformance
// + wire the env-var selector before writing the bulk of the code.
var ErrSlateNotImplemented = errors.New("slatedb backend: method not yet implemented")

// SlateConfig captures the connection parameters for the SlateDB
// metadata store. Mirrors the shape used by S3BlobStore so both can
// share the same operator config surface (HOSTTHIS_S3_*).
type SlateConfig struct {
	// ObjectStoreURL is the OpenDAL-style URL identifying the bucket
	// + region + endpoint. Examples:
	//   "s3://my-bucket?endpoint=http://minio:9000&access_key=…&secret_key=…"
	//   "s3://my-bucket?region=us-east-1"  (uses standard AWS creds)
	// See slatedb-go's ObjectStoreResolve docs for the URL grammar.
	ObjectStoreURL string
	// DbName is the logical database name within the bucket (becomes
	// the key prefix for all of SlateDB's internal SST + manifest
	// files). Lets one bucket host multiple SlateDB instances.
	DbName string
}

// SlateRepo is the SlateDB-backed metadata store. Satisfies the
// same service-layer interfaces as PasteRepo (sqlite). Concurrent
// access from a single Go process is safe (Db is thread-safe);
// multi-process writers are fenced via SlateDB's writer_epoch
// protocol — a second writer that opens the same DB instance halts
// the first one. That's why our deploy story is single-replica
// rolling: one writer at a time, by design.
type SlateRepo struct {
	db    *slatedb.Db
	store *slatedb.ObjectStore
}

// NewSlateRepo opens a SlateDB instance backed by the configured
// object store. Caller must call Close() to flush + shut down.
func NewSlateRepo(cfg SlateConfig) (*SlateRepo, error) {
	store, err := slatedb.ObjectStoreResolve(cfg.ObjectStoreURL)
	if err != nil {
		return nil, err
	}
	builder := slatedb.NewDbBuilder(cfg.DbName, store)
	db, err := builder.Build()
	if err != nil {
		store.Destroy()
		return nil, err
	}
	return &SlateRepo{db: db, store: store}, nil
}

// Close flushes pending writes + shuts down the underlying SlateDB.
func (r *SlateRepo) Close() error {
	if r.db != nil {
		_ = r.db.Shutdown()
	}
	if r.store != nil {
		r.store.Destroy()
	}
	return nil
}

// -- PasteRepo / PasteAdmin / PasteReader -----------------------------------

func (r *SlateRepo) Get(slug domain.Slug) (domain.Paste, error) {
	return domain.Paste{}, ErrSlateNotImplemented
}

func (r *SlateRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	return nil, ErrSlateNotImplemented
}

func (r *SlateRepo) InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) Delete(slug domain.Slug) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) SetName(slug domain.Slug, name string) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) SetPinnedVersion(slug domain.Slug, v domain.Version) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) Unpin(slug domain.Slug) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) AppendVersionWithQuotaCheck(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, serviceCap, userCap int64, now time.Time) (AppendResult, error) {
	return AppendResult{}, ErrSlateNotImplemented
}

func (r *SlateRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	return nil, ErrSlateNotImplemented
}

func (r *SlateRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	return domain.Version{}, ErrSlateNotImplemented
}

func (r *SlateRepo) DeleteVersion(slug domain.Slug, ver int) error {
	return ErrSlateNotImplemented
}

func (r *SlateRepo) CountByOwner(owner string) (int, error) {
	return 0, ErrSlateNotImplemented
}

func (r *SlateRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	return 0, ErrSlateNotImplemented
}

func (r *SlateRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	return time.Time{}, ErrSlateNotImplemented
}

// -- SweepRepo --------------------------------------------------------------

func (r *SlateRepo) ExpiredSlugs(now time.Time) ([]string, error) {
	return nil, ErrSlateNotImplemented
}

func (r *SlateRepo) UnreferencedBlobSHAs() ([]string, error) {
	return nil, ErrSlateNotImplemented
}

// -- KeyGateRepo (Sybil rate limit) -----------------------------------------

func (r *SlateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	return false, ErrSlateNotImplemented
}

func (r *SlateRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	return 0, ErrSlateNotImplemented
}
