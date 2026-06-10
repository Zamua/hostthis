// Package storage's shale-backed metadata implementation.
//
// ShaleRepo is the third metadata backend, satisfying the same four
// service-layer interfaces (PasteRepo, PasteAdmin, SweepRepo,
// KeyGateRepo) as the sqlite PasteRepo and the slatedb SlateRepo. Where
// SlateRepo talks to one SlateDB instance directly, ShaleRepo talks to a
// shale cluster.Cluster, which routes each key to the node that owns its
// shard and so scales horizontally. The slate backend is wrapped as the
// per-node KV engine, so a single-node ShaleRepo and the direct SlateRepo
// sit on the same storage primitive; the difference is the cluster layer
// (sharded routing + per-shard CAS) above it.
//
// Canonical design: docs/SPEC.md "Shale-backed metadata storage".
//
// Build/runtime requirement: cgo + libslatedb_uniffi on the platform
// loader path, exactly like SlateRepo (the slate backend ShaleRepo wraps
// pulls in slatedb). Built only under -tags slatedb.
//
// # Key layout (unchanged from SlateRepo)
//
// The key names are identical to SlateRepo's so the row schemas and the
// observable contract carry over without a rename. Co-location across
// shards is achieved by a custom ShardKeyFn (shaleShardKey below) that
// extracts a shard key per key family, NOT by renaming keys:
//
//	pastes/<slug>                      -> shard key <slug>
//	versions/<slug>/<NNNN>             -> shard key <slug>
//	slug_owner/<slug>                  -> shard key <slug>
//	expiry/<rfc3339>/<slug>            -> shard key <slug>  (slug is the LAST segment)
//	identity_pastes/<id>/<slug>        -> shard key <id>
//	identity_first_seen/<id>           -> shard key <id>
//	identity_bytes/<id>                -> shard key <id>    (the reservation counter)
//	keygate/<subnet>/<identity>        -> shard key <subnet>
//
// Routing every key of a family to the same shard makes a transaction
// that touches one family for one subject a single-shard CAS, committed
// through cluster.Transact(pinKey, fn).

//go:build slatedb

package storage

import (
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleConfig captures the parameters needed to open a single-node
// shale cluster over the slate backend. The S3 connection fields mirror
// SlateConfig (they configure the same underlying SlateDB-on-object-
// storage engine); NodeID + the consistency knobs are the cluster-layer
// additions.
//
// This phase builds at ReplicationFactor=1 (one in-process node) for the
// conformance suite. The ShardKeyFn and the reservation pattern are
// present and correct so the same code shards correctly at N>1; shale's
// own tests cover multi-node routing.
type ShaleConfig struct {
	NodeID    string // stable node identity; required by cluster.Open
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false -> slate sets AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files

	// ReplicationFactor is forwarded to cluster.Config. Zero is
	// normalized to 1 by cluster.Open (single owner per key, no
	// replicas). This phase exercises the single-node path.
	ReplicationFactor int
}

// ShaleRepo is the shale-cluster-backed metadata store. It satisfies the
// same service-layer interfaces as SlateRepo. Every operation goes
// through the cluster handle, which routes per-shard via shaleShardKey
// and commits single-shard writes through per-shard CAS.
type ShaleRepo struct {
	cluster *cluster.Cluster
}

// NewShaleRepo opens a single-node shale cluster over a fresh slate
// backend and returns a ShaleRepo over it. Caller must Close() to flush
// and shut down the cluster (which shuts down the slate backend in turn).
//
// The cluster is opened with shaleShardKey as its ShardKeyFn so that
// every key family co-locates on the shard keyed by its subject
// (<slug> / <id> / <subnet>), which is what makes the reservation
// counter and the per-slug authoritative rows each single-shard.
func NewShaleRepo(cfg ShaleConfig) (*ShaleRepo, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("ShaleConfig.Bucket required")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ShaleConfig.NodeID required")
	}

	be, err := slate.New(slate.Config{
		Bucket:    cfg.Bucket,
		DbName:    cfg.DbName,
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseSSL:    cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("shale: open slate backend: %w", err)
	}

	cl, err := cluster.Open(cluster.Config{
		NodeID:            cfg.NodeID,
		Backend:           be,
		ReplicationFactor: cfg.ReplicationFactor,
		ReadConsistency:   cluster.ReadNearest,
		ShardKeyFn:        shaleShardKey,
	})
	if err != nil {
		_ = be.Close()
		return nil, fmt.Errorf("shale: open cluster: %w", err)
	}

	return &ShaleRepo{cluster: cl}, nil
}

// Close shuts down the cluster (and the underlying slate backend).
func (r *ShaleRepo) Close() error {
	if r.cluster != nil {
		return r.cluster.Close()
	}
	return nil
}

// --- PasteRepo / PasteAdmin reads ------------------------------------------

func (r *ShaleRepo) Get(slug domain.Slug) (domain.Paste, error) {
	// TODO(phaseB-impl)
	return domain.Paste{}, errors.New("shale: Get not yet implemented")
}

func (r *ShaleRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	// TODO(phaseB-impl)
	return nil, errors.New("shale: ListByOwner not yet implemented")
}

func (r *ShaleRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	// TODO(phaseB-impl)
	return nil, errors.New("shale: ListVersions not yet implemented")
}

func (r *ShaleRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	// TODO(phaseB-impl)
	return domain.Version{}, errors.New("shale: GetVersion not yet implemented")
}

func (r *ShaleRepo) CountByOwner(owner string) (int, error) {
	// TODO(phaseB-impl)
	return 0, errors.New("shale: CountByOwner not yet implemented")
}

func (r *ShaleRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	// TODO(phaseB-impl)
	return 0, errors.New("shale: SumActiveBytesByOwner not yet implemented")
}

func (r *ShaleRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	// TODO(phaseB-impl)
	return time.Time{}, errors.New("shale: OwnerFirstSeen not yet implemented")
}

// --- PasteRepo / PasteAdmin writes -----------------------------------------

func (r *ShaleRepo) InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error {
	// TODO(phaseB-impl)
	return errors.New("shale: InsertWithQuotaCheck not yet implemented")
}

func (r *ShaleRepo) AppendVersionWithQuotaCheck(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, serviceCap, userCap int64, now time.Time) (AppendResult, error) {
	// TODO(phaseB-impl)
	return AppendResult{}, errors.New("shale: AppendVersionWithQuotaCheck not yet implemented")
}

func (r *ShaleRepo) Delete(slug domain.Slug) error {
	// TODO(phaseB-impl)
	return errors.New("shale: Delete not yet implemented")
}

func (r *ShaleRepo) DeleteVersion(slug domain.Slug, ver int) error {
	// TODO(phaseB-impl)
	return errors.New("shale: DeleteVersion not yet implemented")
}

func (r *ShaleRepo) SetName(slug domain.Slug, name string) error {
	// TODO(phaseB-impl)
	return errors.New("shale: SetName not yet implemented")
}

func (r *ShaleRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	// TODO(phaseB-impl)
	return errors.New("shale: SetPinnedVersion not yet implemented")
}

func (r *ShaleRepo) Unpin(slug domain.Slug) error {
	// TODO(phaseB-impl)
	return errors.New("shale: Unpin not yet implemented")
}

// --- SweepRepo -------------------------------------------------------------

func (r *ShaleRepo) ExpiredSlugs(now time.Time) ([]string, error) {
	// TODO(phaseB-impl)
	return nil, errors.New("shale: ExpiredSlugs not yet implemented")
}

// ReferencedBlobSHAs MUST NOT return a nil slice with a nil error. The
// sweep treats the returned slice as the allow-list of blobs to keep; a
// nil/empty allow-list returned while blobs exist would tell the sweep
// every blob is orphaned and delete the bucket. Guard against a repo that
// returns 0 referenced shas, which would make the sweep delete every
// blob: this stub fails loudly so a half-wired backend can never silently
// produce an empty allow-list. The real implementation returns the
// content-shas of LIVE (non-deleted) versions plus paste heads.
func (r *ShaleRepo) ReferencedBlobSHAs() ([]string, error) {
	// TODO(phaseB-impl)
	return nil, errors.New("shale: ReferencedBlobSHAs not yet implemented")
}

// --- KeyGateRepo -----------------------------------------------------------

func (r *ShaleRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	// TODO(phaseB-impl)
	return false, errors.New("shale: AdmitNewKey not yet implemented")
}

func (r *ShaleRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	// TODO(phaseB-impl)
	return 0, errors.New("shale: DeleteFirstSeenOlderThan not yet implemented")
}

func (r *ShaleRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (freshCount int, oldestFirstSeen time.Time, err error) {
	// TODO(phaseB-impl)
	return 0, time.Time{}, errors.New("shale: SubnetSnapshot not yet implemented")
}

func (r *ShaleRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	// TODO(phaseB-impl)
	return 0, errors.New("shale: SubnetsForIdentity not yet implemented")
}
