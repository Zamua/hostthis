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
// shards is achieved by a custom ShardKeyFn (shaleShardKey in
// shale_shardkey.go) that extracts a shard key per key family, NOT by
// renaming keys:
//
//	pastes/<slug>                      -> shard key <slug>
//	versions/<slug>/<NNNN>             -> shard key <slug>
//	slug_owner/<slug>                  -> shard key <slug>
//	expiry/<rfc3339>/<slug>            -> shard key <slug>  (slug is the LAST segment)
//	identity_pastes/<id>/<slug>        -> shard key <id>    (value-bearing projection)
//	identity_first_seen/<id>           -> shard key <id>
//	identity_bytes/<id>                -> shard key <id>    (the reservation counter)
//	identity_reserve/<id>/<slug>       -> shard key <id>    (reservation marker)
//	keygate/<subnet>/<identity>        -> shard key <subnet>
//
// Routing every key of a family to the same shard makes a transaction
// that touches one family for one subject a single-shard CAS, committed
// through cluster.Transact(pinKey, fn).
//
// # Transaction model
//
// shale's CAS transaction (cluster.Transact) is read-modify-write under
// optimistic concurrency: tx.Get records a read-check, tx.Put / tx.Delete
// buffer writes, and Commit validates the read-set on the owner shard and
// applies atomically (retrying the closure on conflict). Two constraints
// shape the code:
//
//   - ScanPrefix is NOT supported inside a CAS transaction. Every scan
//     (a paste's versions, an owner's index entries, a subnet's keygate
//     rows) runs OUTSIDE the transaction via cluster.ScanPrefix (single
//     shard) or cluster.Aggregate (cross-shard). Where the result of a
//     scan must be made race-safe (e.g. the next version number must not
//     be reused), the specific key the decision hinges on is re-read
//     INSIDE the transaction as a read-check, so a racing writer that
//     grabbed it conflicts and the closure retries.
//   - Put rejects empty values (cluster.ErrEmptyValue); the empty-payload
//     shape is reserved for Delete tombstones. Index-marker families that
//     SlateRepo stored as empty values carry a one-byte / JSON value here.
//
// # Cross-family writes: the reservation pattern
//
// Insert / AppendVersion / Delete / DeleteVersion span the {id} counter
// shard and the {slug} authoritative shard, which cannot be one
// transaction. They are a sequence of single-shard CAS transactions:
// reserve on {id}, authoritative write on {slug}, confirm on {id}. The
// reserve step increments identity_bytes BEFORE the authoritative write,
// so quota is a hard ceiling under concurrency (docs/SPEC.md
// "Reservation-pattern quota"). A failure after reserve leaves an
// orphaned reservation that over-counts (fail-safe) until the reconciler
// releases it.

//go:build slatedb

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/cluster"
	"github.com/Zamua/shale/pkg/rpc"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleConfig captures the parameters needed to open a shale cluster over
// the slate backend. The S3 connection fields mirror SlateConfig (they
// configure the same underlying SlateDB-on-object-storage engine); NodeID,
// the peer-discovery fields, and the consistency knobs are the
// cluster-layer additions.
//
// Single-node vs multi-node is selected by BindAddr: empty keeps the
// single-node path (every op local, no gossip, no ring routing - today's
// behavior, the back-compat guarantee), non-empty brings up memberlist +
// gRPC forwarding and joins the ring. The ShardKeyFn and the reservation
// pattern are unchanged across node counts: the same code shards correctly
// at N>1, and shale's own tests cover multi-node routing + rebalance.
type ShaleConfig struct {
	NodeID    string // stable node identity; required by cluster.Open
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false -> slate sets AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files

	// UnitCount selects the backend SHAPE. Zero (the default) opens the
	// single-backend path: one slatedb database per node, today's behavior
	// byte-for-byte. A value >= 1 (which MUST be a power of two) selects
	// MULTI-BACKEND (sharded) mode: the keyspace is partitioned into UnitCount
	// units, each its own slatedb database, distributed across the cluster's
	// nodes by the ring and routed per key by ShardKeyFn -> UnitForHash. Each
	// unit is a full slatedb instance (memtable, WAL, compaction) per owning
	// replica, so keep this small on small deployments. The multi-backend layout
	// is NOT on-disk-compatible with single-backend, so switching modes on an
	// existing deployment is a one-time operator data copy, not in-place (see
	// docs/SPEC.md "Sharded metadata (multi-backend mode)"). Operator env:
	// HOSTTHIS_SHALE_UNIT_COUNT.
	UnitCount int

	// BindAddr is the host:port memberlist listens on. A NON-EMPTY value
	// enables multi-node mode (gossip membership + ring routing + gRPC
	// forwarding + rebalance on membership change). Empty keeps the
	// single-node path: every op resolves to the local backend, no ring,
	// no gossip. This is the back-compat switch.
	BindAddr string

	// GRPCAddr is this node's gRPC forwarding address, broadcast to peers
	// as their forwarding target. Required whenever BindAddr is set
	// (cluster.Open errors if BindAddr is non-empty and GRPCAddr is
	// empty). Ignored in single-node mode.
	GRPCAddr string

	// Seeds are other nodes' BindAddrs, used to bootstrap gossip
	// discovery. Empty means this node is the seed/founder. Ignored in
	// single-node mode.
	Seeds []string

	// ReplicationFactor is forwarded to cluster.Config. Zero is
	// normalized to 1 by cluster.Open (single owner per key, no
	// replicas). R=1 is the horizontal-write-scaling shape; R>1 trades
	// write throughput for availability (docs/SPEC.md "R=1 vs R=2").
	ReplicationFactor int

	// RelaxedDurability selects the slate backend's write-durability mode.
	// The zero value (false) is the SAFE DEFAULT: durable writes, acked only
	// after the object-store flush, the byte-exact path slate took before this
	// knob existed - so a ShaleConfig built without setting this field (tests,
	// the migration tool) stays durable. Setting it true enables relaxed
	// durability (fast-ack at the memtable, background flush) - the largest
	// write-throughput win, removing the per-commit flush round-trip from the
	// hot path. ONLY safe at ReplicationFactor >= 2 across anti-affinity-
	// separated nodes (a replica holds the write through the flush window);
	// relaxed durability at R=1 loses un-flushed writes on a single crash. See
	// docs/SPEC.md "Relaxed durability: fast-ack at the memtable". Threaded to
	// the slate backend's WriteOptions; false leaves WriteOptions nil.
	//
	// NB the operator-facing env var is HOSTTHIS_METADATA_AWAIT_DURABLE (the
	// inverse, default true), matching slatedb's own AwaitDurable terminology;
	// the field is inverted here so the struct's zero value is the safe one.
	RelaxedDurability bool

	// CacheBytes sizes the slatedb SST block + metadata cache (a Moka
	// in-memory cache) the slate backend is given. Zero disables it (slatedb
	// default: no block cache, every read re-fetches SST blocks from the
	// object store - pathological on a distributed-MinIO backend). A few
	// hundred MiB holds the hot SST working set in RAM. Operator-facing env
	// var: HOSTTHIS_METADATA_CACHE_BYTES.
	CacheBytes uint64

	// ReapFenceWALs enables slatedb's fence-WAL garbage collector. slatedb
	// writes a small "fence" WAL object every time a unit DB is opened (to
	// claim the single-writer epoch), and slatedb's GC ships that category in
	// DRY-RUN by default (garbage_collector_options.wal_fence_options.dry_run =
	// true): fence WALs are therefore NEVER deleted and accumulate one-per-open
	// forever, bloating a unit's object-store prefix until a cold-start open
	// crawls (the open re-reads every WAL). Set true (the long-lived serving
	// deploys do) to flip that flag off via the slate backend's Settings, so
	// the GC reaps fence WALs older than its min_age (the conservative slatedb
	// default, 300s) and a unit's WAL count stays bounded - keeping cold-start
	// mounts fast. The zero value (false) leaves slatedb's defaults untouched,
	// the unchanged path for short-lived callers (tests, the migration tool)
	// that open briefly and never accumulate. Operator-facing env var:
	// HOSTTHIS_SLATEDB_FENCE_GC (default on; "false" is the kill-switch).
	ReapFenceWALs bool

	// WriteTimeout / ReadTimeout, when > 0, override the cluster's per-dispatch
	// write/read deadline (cluster.Config defaults each to 5s). Zero keeps the
	// shale default, so a ShaleConfig built without setting these (serving, tests)
	// is unchanged. The bulk migration tool raises these well above 5s because a
	// single CAS commit on an un-GC'd (bloated) unit can stall past 5s under a
	// migrate burst, and a 5s DeadlineExceeded there aborts the whole run.
	WriteTimeout time.Duration
	ReadTimeout  time.Duration

	// Logger receives the skip lines from the tolerant background scans
	// (expiry / reconcile) when they hit an undecodable record. Optional:
	// nil falls back to log.Default(). It never affects the blob-GC ref-set
	// scans, which fail closed rather than skip+log.
	Logger *log.Logger

	// BlobStore is the OPTIONAL streaming blob byte plane. When set, the repo
	// opens its cluster via cluster.NewBlobKV (the blob-capable surface), so a
	// staged blob's pointer co-commits with the metadata on the owning shard
	// (the transactional shale-blob path, docs/SPEC.md "Shale-collocated
	// blobs"). Prod wires the MinIO blob.Store adapter here (a DISTINCT blob
	// bucket from the metadata's object store); tests inject blobmem.New(). A
	// nil BlobStore keeps the metadata-only path (cluster.Open, no *BlobKV) -
	// the back-compat shape every shale test that does not exercise blobs
	// uses, and the shape ShaleBlobUnit is simply not wired for.
	BlobStore blob.Store

	// ConditionalStore is the OPTIONAL shared CAS arbiter (create-if-absent /
	// compare-and-set over the metadata object store) that enables the
	// HOMOGENEOUS bootstrap: when set, cluster.Open decides form-vs-join at
	// runtime against the __cluster/init marker instead of using the founder/
	// joiner seed asymmetry, and AllowSoloStart lets the first pod up come up
	// solo and contend to form. Every pod must wire the SAME store (same bucket,
	// same key prefix) so the marker is one shared object. nil (the default)
	// keeps the seed-based bootstrap unchanged - existing seed/joiner
	// deployments are unaffected. Only meaningful in multi-backend (sharded)
	// mode. See docs/SPEC.md "Homogeneous bootstrap (optional)".
	ConditionalStore storageunit.ConditionalStore
}

// ShaleRepo is the shale-cluster-backed metadata store. It satisfies the
// same service-layer interfaces as SlateRepo. Every operation goes
// through the cluster handle, which routes per-shard via shaleShardKey
// and commits single-shard writes through per-shard CAS.
//
// In multi-node mode (cfg.BindAddr != "") the repo ALSO owns the
// process-level gRPC peer-forwarding server: cluster.Open advertises the
// node's GRPCAddr via gossip but does not stand up the listener that peers
// forward routed reads/writes/migrations to (docs/SPEC.md "Peer
// forwarding"). NewShaleRepo binds that listener + serves the cluster's
// rpc handlers on it; grpcSrv + grpcLis are nil in single-node mode and
// Close gracefully stops/closes them when set.
type ShaleRepo struct {
	cluster *cluster.Cluster

	// kv is the blob-capable cluster surface, set ONLY when cfg.BlobStore is
	// non-nil (the transactional shale-blob path). It wraps the SAME *Cluster
	// as `cluster` (kv.Cluster() == cluster), so every existing r.cluster.*
	// call site is unchanged; only the step-2 authoritative writes that bind a
	// staged blob switch to r.kv.Transact(slug, func(*cluster.BlobTx)) so the
	// BindBlob co-commits with the metadata. nil on the metadata-only path
	// (cfg.BlobStore nil) - the back-compat shape.
	kv *cluster.BlobKV

	// logger records skipped records in the tolerant background scans
	// (the expiry/reconcile family). A nil logger falls back to
	// log.Default() via repoLog, so a repo built without one still emits
	// the skip lines an operator needs to spot a persistently-bad row.
	// The blob-GC ref-set scans never use this: they fail closed (abort +
	// return the decode error) rather than skip, so there is nothing to
	// log-and-continue there.
	logger *log.Logger

	// grpcAddr is the ACTUAL bound forwarding address advertised to peers
	// (lis.Addr().String()), or "" in single-node mode. bindAddr mirrors
	// the memberlist bind address a second node seeds off. Both are exposed
	// via accessors so a peer can reference this node.
	grpcAddr string
	bindAddr string

	// grpcSrv + grpcLis are the peer-forwarding server and its listener,
	// set only in multi-node mode. nil single-node (the back-compat path
	// stands up neither). Close GracefulStops the server, which drains
	// in-flight RPCs and closes the listener.
	grpcSrv *grpc.Server
	grpcLis net.Listener

	// confirmWG tracks in-flight deferred confirm-insert goroutines (the
	// step-3 index write moved off the upload response path, see
	// InsertWithQuotaCheck). Close waits on it so a shutdown does not drop a
	// pending confirm (the reconciler would otherwise have to heal it), and
	// WaitPendingConfirms lets a test or operator block until every deferred
	// confirm has run so a subsequent ListByOwner is deterministic.
	confirmWG sync.WaitGroup

	// cache is the slatedb SST block + metadata cache shared by the slate
	// backend (nil when CacheBytes==0). Operator-owned uniffi handle; Close
	// Destroys it after the cluster (and its backend) have shut down.
	cache *slatedb.DbCache

	// fenceGCSettings is the slatedb Settings object that enables the fence-WAL
	// garbage collector (see ShaleConfig.ReapFenceWALs). Built once and
	// forwarded verbatim to every unit's DbBuilder by the slate backend; an
	// operator-owned uniffi handle that Close Destroys after the cluster (and
	// its backends) have shut down. Nil when ReapFenceWALs is false.
	fenceGCSettings *slatedb.Settings

	// closeFactory releases the multi-backend slate Backing after the cluster
	// shuts down (cluster.Close closes the mounted unit databases; this is the
	// backing-level safety net that flushes/closes anything left, mirroring
	// shaled-slate's CloseFactory). Nil in single-backend mode, where
	// cluster.Close owns closing the single backend.
	closeFactory func() error
}

// WaitPendingConfirms blocks until every deferred confirm-insert goroutine
// launched by InsertWithQuotaCheck has finished. The confirm step (writing
// the derived identity_pastes index entry + first-seen) runs in the
// background so it stays off the upload response path; a freshly-inserted
// paste is Get-readable immediately but may take a beat to appear in
// ListByOwner. Callers that need the list to reflect a just-inserted paste
// synchronously (tests, an operator draining before a snapshot) call this.
// It does not stop new confirms from being launched; it drains the ones
// outstanding at the moment of the call's WaitGroup snapshot.
func (r *ShaleRepo) WaitPendingConfirms() { r.confirmWG.Wait() }

// repoLog returns the repo's logger, falling back to the process default
// when none was wired. The tolerant background scans (expiry / reconcile)
// log each skipped undecodable record through this so a persistently-bad
// row is visible to an operator instead of being silently swallowed.
func (r *ShaleRepo) repoLog() *log.Logger {
	if r.logger != nil {
		return r.logger
	}
	return log.Default()
}

// slateConfigFromShale maps a ShaleConfig to the slate.Config used to open
// the per-node backend. Pure (no I/O), so the WriteOptions wiring is unit-
// testable without a live object store. The S3 fields are a straight
// copy-through; the only logic is the durability knob: RelaxedDurability=true
// sets slate's per-write WriteOptions to fast-ack, while the default (false)
// leaves WriteOptions nil - the byte-exact durable path slate took before the
// knob existed (slate treats nil WriteOptions as AwaitDurable=true). See
// docs/SPEC.md "Relaxed durability: fast-ack at the memtable".
func slateConfigFromShale(cfg ShaleConfig) slate.Config {
	sc := slate.Config{
		Bucket:    cfg.Bucket,
		DbName:    cfg.DbName,
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseSSL:    cfg.UseSSL,
	}
	if cfg.RelaxedDurability {
		sc.WriteOptions = &slatedb.WriteOptions{AwaitDurable: false}
	}
	return sc
}

// newFenceWALGCSettings builds a slatedb Settings with the fence-WAL garbage
// collector enabled: it flips garbage_collector_options.wal_fence_options.dry_run
// from slatedb's default (true) to false. ONLY that one flag is changed - every
// other GC category and the conservative min_age (slatedb's 300s default) are
// left untouched, so the GC reaps only superseded fence WAL objects older than
// min_age and never touches data WALs or a recently-written (still-live) fence.
// Without this, fence WALs (one written per unit open) are never collected and a
// unit's object-store prefix grows without bound, making cold-start opens crawl.
// See ShaleConfig.ReapFenceWALs. The returned handle is operator-owned: the
// caller forwards it to the slate backend and Destroys it on shutdown.
func newFenceWALGCSettings() (*slatedb.Settings, error) {
	s := slatedb.SettingsDefault()
	if err := s.Set("garbage_collector_options.wal_fence_options.dry_run", "false"); err != nil {
		s.Destroy()
		return nil, err
	}
	return s, nil
}

// NewShaleRepo opens a shale cluster over a fresh slate backend and
// returns a ShaleRepo over it. Caller must Close() to flush and shut down
// the cluster (which shuts down the slate backend in turn).
//
// When cfg.BindAddr is empty the cluster opens single-node (today's
// behavior: every op local, no gossip, no ring routing). When BindAddr is
// non-empty it opens multi-node: cluster.Open brings up memberlist on
// BindAddr, advertises GRPCAddr to peers, joins via Seeds, and rebalances
// the ring on membership change. The peer-discovery fields are passed
// straight through to cluster.Config; everything else (ShardKeyFn,
// ReadConsistency, ReplicationFactor) is identical across modes.
//
// The cluster is opened with shaleShardKey as its ShardKeyFn so that
// every key family co-locates on the shard keyed by its subject
// (<slug> / <id> / <subnet>), which is what makes the reservation
// counter and the per-slug authoritative rows each single-shard.
//
// Multi-node wiring (cfg.BindAddr != ""): NewShaleRepo binds the gRPC
// peer-forwarding listener on cfg.GRPCAddr BEFORE opening the cluster, then
// advertises the listener's ACTUAL bound address (lis.Addr().String()) as
// the cluster's GRPCAddr. This matters when cfg.GRPCAddr is ":0" / an
// OS-assigned port: the real port is only known after Listen, and a peer
// that forwards to the advertised address must reach the address actually
// served. After the cluster is open it registers the cluster's rpc
// handlers on a grpc.Server and serves in a background goroutine. Close
// gracefully stops that server + closes the listener. Single-node mode
// (cfg.BindAddr == "") binds no listener and starts no server: the
// back-compat path is byte-for-byte today's behavior.
func NewShaleRepo(cfg ShaleConfig) (*ShaleRepo, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("ShaleConfig.Bucket required")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ShaleConfig.NodeID required")
	}

	// Multi-node only: bind the peer-forwarding listener BEFORE opening the
	// cluster so the advertised GRPCAddr is the address actually served
	// (resolves the ":0" / OS-assigned-port case). Single-node leaves lis
	// nil and the cluster opens with an empty GRPCAddr, exactly as before.
	var lis net.Listener
	advertiseGRPCAddr := cfg.GRPCAddr
	if cfg.BindAddr != "" {
		if cfg.GRPCAddr == "" {
			return nil, fmt.Errorf("shale: GRPCAddr required in multi-node mode (BindAddr set)")
		}
		l, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			return nil, fmt.Errorf("shale: bind gRPC listener on %q: %w", cfg.GRPCAddr, err)
		}
		lis = l
		advertiseGRPCAddr = l.Addr().String()
	}

	sc := slateConfigFromShale(cfg)
	// Block cache: without one, slatedb re-fetches SST blocks from the
	// object store on every read, which on a distributed-MinIO backend is a
	// steady self-inflicted read storm (the same hot SSTs fetched hundreds
	// of times a second). A modest in-memory Moka cache holds the hot SST
	// working set in RAM and eliminates ~all of those object reads. Operator
	// owns the handle; Close Destroys it after the cluster shuts down.
	var cache *slatedb.DbCache
	if cfg.CacheBytes > 0 {
		c, cerr := slatedb.DbCacheNewMokaCache(slatedb.MokaCacheOptions{MaxCapacity: cfg.CacheBytes})
		if cerr != nil {
			if lis != nil {
				_ = lis.Close()
			}
			return nil, fmt.Errorf("shale: build slatedb block cache: %w", cerr)
		}
		sc.Cache = c
		cache = c
	}

	// Fence-WAL GC: build the slatedb Settings that turns OFF the fence-WAL
	// collector's dry-run default, so the per-open fence WAL objects get reaped
	// instead of accumulating unboundedly (see ShaleConfig.ReapFenceWALs). The
	// slate backend forwards this verbatim to every unit's DbBuilder (both the
	// single-backend slate.Config and the multi-backend BackingConfig below).
	// Built before cleanup so the closure Destroys it on any later open error.
	var fenceGCSettings *slatedb.Settings
	if cfg.ReapFenceWALs {
		s, sErr := newFenceWALGCSettings()
		if sErr != nil {
			if cache != nil {
				cache.Destroy()
			}
			if lis != nil {
				_ = lis.Close()
			}
			return nil, fmt.Errorf("shale: build fence-WAL GC settings: %w", sErr)
		}
		sc.Settings = s
		fenceGCSettings = s
	}

	cleanup := func() {
		if cache != nil {
			cache.Destroy()
		}
		if fenceGCSettings != nil {
			fenceGCSettings.Destroy()
		}
		if lis != nil {
			_ = lis.Close()
		}
	}

	// Backend SHAPE: single-backend (UnitCount 0, today's path) or multi-backend
	// sharded (UnitCount a power of two). The cluster.Config differs only in
	// Backend vs BackendFactory+UnitCount; the peer-discovery + read-quorum +
	// shard-key fields are identical.
	clusterCfg := cluster.Config{
		NodeID:            cfg.NodeID,
		BindAddr:          cfg.BindAddr,
		GRPCAddr:          advertiseGRPCAddr,
		Seeds:             cfg.Seeds,
		ReplicationFactor: cfg.ReplicationFactor,
		// Per-dispatch write/read deadlines. Zero leaves cluster.Open's 5s default
		// (serving keeps it); the bulk migrate raises them so a slow CAS on a
		// bloated unit does not trip a 5s DeadlineExceeded mid-run.
		WriteTimeout: cfg.WriteTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		// ReadQuorum, not ReadNearest: at R>1 ReadNearest decides on the first
		// replica to answer and treats a NotFound as usable, so a read served by a
		// still-backfilling replica (a freshly joined node) could return NotFound
		// for a key that exists on the other replica. ReadQuorum reads a quorum and
		// the present value wins on LWW. At R=1 a quorum is the single replica, so
		// this is behavior-identical to ReadNearest there (one read, no extra hop).
		// See docs/SPEC.md "Deploy arc: replication factor 1, then scale out".
		ReadConsistency: cluster.ReadQuorum,
		ShardKeyFn:      shaleShardKey,
		// Optional streaming blob plane. When set, cluster.NewBlobKV opens the
		// blob-capable surface so a staged blob's pointer co-commits with the
		// metadata; when nil, cluster.Open opens the metadata-only path.
		BlobStore: cfg.BlobStore,
		// Optional homogeneous bootstrap. When set, cluster.Open uses the
		// __cluster/init marker (try-join-else-form) + derives AllowSoloStart,
		// retiring the founder/joiner seed asymmetry; nil keeps seed-based
		// bootstrap. Every pod wires the SAME store. See docs/SPEC.md.
		ConditionalStore: cfg.ConditionalStore,
	}
	// Cold-start patience: a joiner re-sweeps its seeds for the cluster generation
	// up to GenLearnBudget (shale default 180s) so a still-mounting seed is waited
	// for instead of crash-looping the joiner. These envs override the default (a
	// longer budget for a slow backend, or a SHORT one + SHALE_DEBUG_MOUNT_DELAY to
	// reproduce the pre-fix crash-loop on a real cluster); unset keeps the default.
	if v := strings.TrimSpace(os.Getenv("SHALE_GEN_LEARN_BUDGET")); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil {
			clusterCfg.GenLearnBudget = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("SHALE_DEBUG_MOUNT_DELAY")); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil {
			clusterCfg.TestingMountDelay = d
		}
	}

	var closeFactory func() error
	if cfg.UnitCount > 0 {
		// MULTI-BACKEND (sharded): UnitCount independent slatedb unit databases
		// under cfg.DbName as the shared key-prefix, distributed across the ring
		// and routed per key by ShardKeyFn. See docs/SPEC.md "Sharded metadata".
		uc, ucErr := storageunit.NewUnitCount(cfg.UnitCount)
		if ucErr != nil {
			cleanup()
			return nil, fmt.Errorf("shale: invalid UnitCount %d (must be a power of two): %w", cfg.UnitCount, ucErr)
		}
		backing, bErr := slate.NewBacking(slate.BackingConfig{
			Bucket:                   cfg.Bucket,
			Endpoint:                 cfg.Endpoint,
			Region:                   cfg.Region,
			AccessKey:                cfg.AccessKey,
			SecretKey:                cfg.SecretKey,
			UseSSL:                   cfg.UseSSL,
			KeyPrefix:                cfg.DbName,
			Cache:                    cache,
			Settings:                 fenceGCSettings,
			RelaxedReplicaDurability: cfg.RelaxedDurability,
		})
		if bErr != nil {
			cleanup()
			return nil, fmt.Errorf("shale: open slate backing: %w", bErr)
		}
		handle := backing.Handle()
		clusterCfg.BackendFactory = handle
		clusterCfg.UnitCount = uc
		closeFactory = handle.Close
	} else {
		// SINGLE-BACKEND (default): one slatedb database, today's behavior.
		be, beErr := slate.New(sc)
		if beErr != nil {
			cleanup()
			return nil, fmt.Errorf("shale: open slate backend: %w", beErr)
		}
		clusterCfg.Backend = be
	}

	// Open the cluster. With a blob store configured, open the blob-capable
	// surface (cluster.NewBlobKV) so staged-blob pointers can co-commit with
	// the metadata; without one, the metadata-only path (cluster.Open). Both
	// hold the SAME underlying *Cluster, so r.cluster is set either way and
	// every existing routed op is unchanged.
	var (
		cl *cluster.Cluster
		kv *cluster.BlobKV
	)
	if cfg.BlobStore != nil {
		bkv, berr := cluster.NewBlobKV(clusterCfg)
		if berr != nil {
			if closeFactory != nil {
				_ = closeFactory()
			} else if clusterCfg.Backend != nil {
				_ = clusterCfg.Backend.Close()
			}
			cleanup()
			return nil, fmt.Errorf("shale: open blob cluster: %w", berr)
		}
		kv = bkv
		cl = bkv.Cluster()
	} else {
		c, oerr := cluster.Open(clusterCfg)
		if oerr != nil {
			if closeFactory != nil {
				_ = closeFactory()
			} else if clusterCfg.Backend != nil {
				_ = clusterCfg.Backend.Close()
			}
			cleanup()
			return nil, fmt.Errorf("shale: open cluster: %w", oerr)
		}
		cl = c
	}

	r := &ShaleRepo{
		cluster:         cl,
		kv:              kv,
		logger:          cfg.Logger,
		bindAddr:        cfg.BindAddr,
		grpcAddr:        advertiseGRPCAddr,
		cache:           cache,
		fenceGCSettings: fenceGCSettings,
		closeFactory:    closeFactory,
	}

	// Multi-node: stand up the peer-forwarding server the cluster advertised
	// but does not serve itself. cluster.Open advertises GRPCAddr via gossip;
	// this is the listener that answers it.
	if lis != nil {
		// The cluster's inter-node client (shale pkg/cluster) keepalives every
		// 30s WITH PermitWithoutStream. A default server enforcement policy
		// (MinTime 5min, pings-without-streams disallowed) GOAWAYs those pings as
		// too_many_pings and closes the connection mid-preface - which presents
		// identically to a network drop ("error reading server preface: use of
		// closed network connection") and stalls the cross-shard scan. Permit the
		// client's keepalive cadence so peer forwarding stays up.
		g := grpc.NewServer(
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
		)
		rpc.NewServer(cl).Register(g)
		r.grpcSrv = g
		r.grpcLis = lis
		go func() {
			// Serve returns when GracefulStop (in Close) closes the
			// listener; that is a clean shutdown, not an error to surface.
			_ = g.Serve(lis)
		}()
	}

	return r, nil
}

// Close shuts down the peer-forwarding gRPC server (multi-node only, a
// no-op when nil), then the cluster (and the underlying slate backend).
// GracefulStop drains in-flight RPCs and closes the listener, so the
// forwarding port is released with no leaked goroutine.
func (r *ShaleRepo) Close() error {
	// Drain in-flight deferred confirms before tearing down the cluster, so
	// a shutdown does not strand a confirm mid-flight (which would leave the
	// reconciler to heal an index entry we could have written cleanly).
	r.confirmWG.Wait()
	if r.grpcSrv != nil {
		r.grpcSrv.GracefulStop() // also closes r.grpcLis
	}
	var err error
	if r.cluster != nil {
		err = r.cluster.Close()
	}
	// Multi-backend: release the slate Backing after the cluster closed its
	// mounted units (the backing-level flush/close safety net). No-op in
	// single-backend mode, where cluster.Close owns the single backend.
	if r.closeFactory != nil {
		if ferr := r.closeFactory(); ferr != nil && err == nil {
			err = ferr
		}
	}
	// Destroy the cache AFTER the cluster (and its slate backend) have shut
	// down, so no in-flight read still references it.
	if r.cache != nil {
		r.cache.Destroy()
		r.cache = nil
	}
	// Destroy the fence-WAL GC Settings handle on the same beat (after the
	// cluster + backends closed). The opened units captured their GC config at
	// Build time, so this handle is no longer referenced once they have shut
	// down.
	if r.fenceGCSettings != nil {
		r.fenceGCSettings.Destroy()
		r.fenceGCSettings = nil
	}
	return err
}

// GRPCAddr returns this node's ACTUAL bound gRPC forwarding address (the
// one advertised to peers), or "" in single-node mode. A second node seeds
// off the cluster via BindAddr, but a deploy/test that needs to reference
// the served forwarding endpoint reads it here (the configured GRPCAddr may
// have been ":0", so the actual port is only known post-bind).
func (r *ShaleRepo) GRPCAddr() string { return r.grpcAddr }

// BindAddr returns this node's memberlist bind address, or "" in
// single-node mode. A second node passes this as its Seeds entry to join
// the ring this node founded.
func (r *ShaleRepo) BindAddr() string { return r.bindAddr }

// --- key builders (mirror SlateRepo's layout) ------------------------------

func shaleKeyPaste(slug domain.Slug) []byte { return []byte("pastes/" + slug.String()) }

func shaleKeyVersion(slug domain.Slug, verNum int) []byte {
	return fmt.Appendf(nil, "versions/%s/%04d", slug.String(), verNum)
}

func shalePrefixVersions(slug domain.Slug) []byte {
	return []byte("versions/" + slug.String() + "/")
}

func shaleKeySlugOwner(slug domain.Slug) []byte { return []byte("slug_owner/" + slug.String()) }

func shaleKeyIdentityPaste(identity, slug string) []byte {
	return []byte("identity_pastes/" + identity + "/" + slug)
}

func shalePrefixIdentityPastes(identity string) []byte {
	return []byte("identity_pastes/" + identity + "/")
}

func shaleKeyIdentityFirstSeen(identity string) []byte {
	return []byte("identity_first_seen/" + identity)
}

func shaleKeyIdentityBytes(identity string) []byte {
	return []byte("identity_bytes/" + identity)
}

func shaleKeyIdentityReserve(identity, slug string) []byte {
	return []byte("identity_reserve/" + identity + "/" + slug)
}

func shalePrefixIdentityReserve(identity string) []byte {
	return []byte("identity_reserve/" + identity + "/")
}

func shaleKeyExpiry(t time.Time, slug domain.Slug) []byte {
	return []byte("expiry/" + t.UTC().Format(time.RFC3339Nano) + "/" + slug.String())
}

func shaleKeyKeygate(subnet, identity string) []byte {
	return []byte("keygate/" + subnet + "/" + identity)
}

func shalePrefixKeygateSubnet(subnet string) []byte {
	return []byte("keygate/" + subnet + "/")
}

// markerValue is the non-empty placeholder for index families that
// carry no data of their own (the expiry index). shale's Put rejects
// empty values, so a one-byte marker stands in for SlateRepo's []byte{}.
var markerValue = []byte{'1'}

// defaultReserveGrace is the safety interval the reconciler uses to tell
// an in-flight reservation (younger than the grace) from an abandoned or
// leaked one (older than the grace). It is a var, not a const, so tests
// can shrink it; callers pass their own value into Reconcile.
var defaultReserveGrace = 10 * time.Minute

// DefaultReserveGrace is the recommended reserveGrace to pass to
// Reconcile: comfortably longer than the reserve->authoritative-write
// window of a healthy insert, so the reconciler never races a live
// upload, while short enough that an abandoned reservation's over-count
// is reclaimed promptly. Operators scheduling Reconcile use this unless
// they have a reason to override it.
func DefaultReserveGrace() time.Duration { return defaultReserveGrace }

// PendingPasteTimeout is how old a status=pending paste may be before the
// reconciler ages it to failed (the pod-death backstop: its in-memory
// bytes never reached the blob store). It is comfortably longer than a
// healthy blob write (~250 ms) plus retries, so the reconciler never
// races a live finalizer, while short enough that a lost-bytes paste
// self-heals out of the loading screen within a sweep tick or two. A var,
// not a const, so tests can shrink it.
var PendingPasteTimeout = 2 * time.Minute

// reservationMarker is the value stored at identity_reserve/<id>/<slug>.
// Bytes is the reserved size; CreatedAt is the `now` the reserve was
// stamped with (RFC3339Nano via time.Time JSON), so the reconciler can
// compute the marker's age (now - created_at) and apply the grace window
// without inferring age from the paste's absence (docs/SPEC.md
// "Reservation-pattern quota").
type reservationMarker struct {
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// encodeReservationMarker serializes a marker to its JSON value.
func encodeReservationMarker(bytes int64, createdAt time.Time) ([]byte, error) {
	return json.Marshal(reservationMarker{Bytes: bytes, CreatedAt: createdAt.UTC()})
}

// parseReservationMarker decodes a reservation marker value. It tolerates
// a legacy bare-number marker (a plain decimal `body` with no timestamp,
// as an earlier layout wrote): a legacy marker decodes to its byte count
// with a zero CreatedAt, which reads as "very old" so it is always past
// any grace window. New markers always carry the JSON shape.
func parseReservationMarker(raw []byte) (reservationMarker, error) {
	// Strip the LWW envelope first: at R>1 the raw CAS tx.Get marker is
	// wrapped, so the '{' JSON sniff below would miss and misroute it to the
	// legacy bare-number path. Idempotent for R=1 / already-stripped values.
	raw, err := stripEnvelope(raw)
	if err != nil {
		return reservationMarker{}, fmt.Errorf("strip reservation marker envelope: %w", err)
	}
	if len(raw) == 0 {
		return reservationMarker{}, nil
	}
	if raw[0] == '{' {
		var m reservationMarker
		if err := json.Unmarshal(raw, &m); err != nil {
			return reservationMarker{}, fmt.Errorf("decode reservation marker: %w", err)
		}
		return m, nil
	}
	// Legacy bare-number marker: bytes only, zero (very old) created_at.
	n, err := parseCounter(raw)
	if err != nil {
		return reservationMarker{}, fmt.Errorf("decode legacy reservation marker: %w", err)
	}
	return reservationMarker{Bytes: n}, nil
}

// --- JSON projections ------------------------------------------------------

// identityPasteRow is the value-bearing projection stored at
// identity_pastes/<id>/<slug>. It denormalizes the fields ListByOwner
// needs so the list is a single-shard scan that does not fan out to the
// {slug} shards. It is derived (eventually consistent); repair-on-read +
// the reconciler keep it converged with the authoritative pastes/* rows.
type identityPasteRow struct {
	Name      string    `json:"name"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// --- generic helpers -------------------------------------------------------

// getJSON reads + JSON-decodes a value at key via routed Get. Returns
// storage.ErrNotFound when the key doesn't exist (translating shale's
// backend.ErrNotFound to the storage sentinel the callers + conformance
// suite expect).
func (r *ShaleRepo) getJSON(key []byte, out any) error {
	raw, err := r.cluster.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("get %s: %w", key, err)
	}
	// cluster.Get only unwraps the LWW envelope on the getReplicated (R>1,
	// non-multi) path; the single-node R=1 backend read and the multi-backend
	// read hand back the RAW stored bytes. A record left enveloped by an R>1
	// write therefore reaches here wrapped, so strip before decoding.
	// Idempotent for R=1 raw / already-unwrapped values.
	payload, serr := stripEnvelope(raw)
	if serr != nil {
		return fmt.Errorf("strip %s: %w", key, serr)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
}

// getRaw reads a value via routed Get, returning (nil, nil) when absent. Like
// getJSON it strips the LWW envelope cluster.Get leaves on at R=1 / multi
// (idempotent for raw values) so callers see the payload, not the wrapper.
func (r *ShaleRepo) getRaw(key []byte) ([]byte, error) {
	raw, err := r.cluster.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	payload, serr := stripEnvelope(raw)
	if serr != nil {
		return nil, fmt.Errorf("strip %s: %w", key, serr)
	}
	return payload, nil
}

// scanPrefix collects all (key, value) pairs whose key starts with prefix
// from the OWNING shard, via the routed cluster.ScanPrefix. Used for
// single-shard list/prefix queries (a paste's versions, an owner's
// pastes, a subnet's keygate rows). For cross-shard scans use aggregate.
func (r *ShaleRepo) scanPrefix(prefix []byte) ([]scanItem, error) {
	it, err := r.cluster.ScanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("scan prefix %s: %w", prefix, err)
	}
	defer it.Close() //nolint:errcheck
	var out []scanItem
	for {
		k, v, err := it.Next()
		if err != nil {
			return nil, fmt.Errorf("scan next %s: %w", prefix, err)
		}
		if k == nil && v == nil {
			break
		}
		// cluster.ScanPrefix, like the backend scan under aggregatePrefix,
		// returns the RAW stored value - an LWW envelope at R>1. Strip it so
		// consumers (keygate timestamps, version rows, owner pastes) decode
		// the payload exactly as cluster.Get hands it to them. Idempotent for
		// R=1 / pre-envelope values; a truncated envelope surfaces as an error.
		payload, derr := stripEnvelope(v)
		if derr != nil {
			return nil, fmt.Errorf("scan strip %s: %w", prefix, derr)
		}
		out = append(out, scanItem{
			Key:   append([]byte(nil), k...),
			Value: append([]byte(nil), payload...),
		})
	}
	return out, nil
}

// aggregatePrefix fans out across all shards (cluster.Aggregate) and
// collects every (key, value) whose key starts with prefix, deduplicating
// keys (a key may surface from more than one replica at R>1; the value is
// identical so last-writer-wins on the map is fine). Used for the three
// inherently cross-shard background operations: the expiry scan, the
// referenced-blob set, and keygate prune / counting.
func (r *ShaleRepo) aggregatePrefix(prefix []byte) ([]scanItem, error) {
	results := r.cluster.Aggregate(func(b backend.Backend) any {
		it, err := b.ScanPrefix(prefix)
		if err != nil {
			return err
		}
		defer it.Close() //nolint:errcheck
		var local []scanItem
		for {
			k, v, err := it.Next()
			if err != nil {
				return err
			}
			if k == nil && v == nil {
				break
			}
			// ScanPrefix returns the RAW backend value, which for an
			// R>1 write is an LWW Envelope (the cluster layer wraps on
			// Put and unwraps on Get, but the Backend - and therefore
			// this raw scan - sees the envelope). The aggregate
			// consumers (quota sum, blob-ref set, expiry sweep) expect
			// the decoded payload, exactly as cluster.Get hands them.
			// cluster.Decode is the universal strip: a magic-prefixed
			// envelope yields its payload; a pre-envelope / R=1 raw
			// value passes through unchanged (zero-Stamp payload). A
			// decode error means a truncated envelope (corruption) and
			// is surfaced, not swallowed.
			env, derr := cluster.Decode(v)
			if derr != nil {
				return fmt.Errorf("decode envelope for %q: %w", k, derr)
			}
			local = append(local, scanItem{
				Key:   append([]byte(nil), k...),
				Value: append([]byte(nil), env.Payload...),
			})
		}
		return local
	})

	seen := make(map[string]scanItem)
	for _, res := range results {
		if res.Err != nil {
			return nil, fmt.Errorf("aggregate %s: %w", prefix, res.Err)
		}
		switch v := res.Value.(type) {
		case error:
			return nil, fmt.Errorf("aggregate %s: %w", prefix, v)
		case []scanItem:
			for _, item := range v {
				seen[string(item.Key)] = item
			}
		case nil:
			// peer with no matching keys
		}
	}
	out := make([]scanItem, 0, len(seen))
	for _, item := range seen {
		out = append(out, item)
	}
	return out, nil
}

// --- version-set helpers ---------------------------------------------------

// scanVersions returns the version rows for a slug (decoded), scanned
// from the {slug} shard OUTSIDE any transaction.
func (r *ShaleRepo) scanVersions(slug domain.Slug) ([]versionRow, error) {
	items, err := r.scanPrefix(shalePrefixVersions(slug))
	if err != nil {
		return nil, err
	}
	out := make([]versionRow, 0, len(items))
	for _, item := range items {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// maxVerNum returns the highest version number across the supplied rows
// INCLUDING tombstones (version numbers are never reused). 0 for none.
func maxVerNum(versions []versionRow) int {
	max := 0
	for _, v := range versions {
		if v.VerNum > max {
			max = v.VerNum
		}
	}
	return max
}

// latestActiveVerNum returns the highest NON-deleted version number, or 1
// when none are live (matches sqlite COALESCE(..., 1)).
func latestActiveVerNum(versions []versionRow) int {
	latest := 0
	for _, v := range versions {
		if v.Deleted {
			continue
		}
		if v.VerNum > latest {
			latest = v.VerNum
		}
	}
	if latest == 0 {
		latest = 1
	}
	return latest
}

// --- counter helpers -------------------------------------------------------

// stripEnvelope returns the LWW-envelope payload of a RAW backend value.
// At R>1 every stored value is wrapped (magic + Stamp + payload); the cluster
// layer unwraps on cluster.Get/Delete, but the low-level paths - CAS tx.Get
// and the backend ScanPrefix used by aggregatePrefix - hand back the raw
// stored bytes. Any caller that DECODES such a raw read (parseCounter,
// json.Unmarshal, room-value length) must strip first or it will choke on the
// binary envelope header. Idempotent for R=1 / already-stripped values: bytes
// with no magic prefix pass through unchanged (cluster.Decode's v0.3-compat
// path). A truncated envelope is corruption and surfaces as an error.
func stripEnvelope(raw []byte) ([]byte, error) {
	env, err := cluster.Decode(raw)
	if err != nil {
		return nil, err
	}
	return env.Payload, nil
}

func parseCounter(raw []byte) (int64, error) {
	payload, err := stripEnvelope(raw)
	if err != nil {
		return 0, fmt.Errorf("decode counter envelope: %w", err)
	}
	if len(payload) == 0 {
		return 0, nil // absent or an empty-payload (tombstone) envelope
	}
	n, err := strconv.ParseInt(string(payload), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode counter: %w", err)
	}
	return n, nil
}

func formatCounter(n int64) []byte {
	if n < 0 {
		n = 0 // the counter never goes negative; clamp defensively
	}
	return []byte(strconv.FormatInt(n, 10))
}

// txGetCounter reads the identity_bytes counter inside a CAS tx, recording
// the read-check. Absent reads as 0 (and an ExpectAbsent check).
func txGetCounter(tx shaleKVTx, key []byte) (int64, error) {
	raw, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("tx get counter: %w", err)
	}
	return parseCounter(raw)
}

// shaleTxGetJSON reads + decodes inside a CAS tx, returning storage.ErrNotFound
// when absent so the closure can branch on existence.
func shaleTxGetJSON(tx shaleKVTx, key []byte, out any) error {
	raw, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("tx get %s: %w", key, err)
	}
	payload, err := stripEnvelope(raw)
	if err != nil {
		return fmt.Errorf("tx strip %s: %w", key, err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("tx decode %s: %w", key, err)
	}
	return nil
}

// shaleTxPutJSON encodes + buffers a Put inside a CAS tx.
func shaleTxPutJSON(tx shaleKVTx, key []byte, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	return tx.Put(key, body)
}

// --- PasteRepo / PasteAdmin reads ------------------------------------------

func (r *ShaleRepo) Get(slug domain.Slug) (domain.Paste, error) {
	var row pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &row); err != nil {
		return domain.Paste{}, err
	}
	return row.toDomain(slug), nil
}

// ListByOwner scans the owner's value-bearing identity_pastes index on
// the {id} shard and repairs on read (docs/SPEC.md "Derived indexes and
// repair-on-read"):
//
//   - a stale index entry (paste row gone) is skipped + queued for removal,
//   - the live pastes/* rows are the source of truth, so the LatestVersion
//     and the head fields are read from the {slug} shard per result.
//
// The "missing index entry" half of repair-on-read (an authoritative
// paste with no index entry) is the reconciler's job: ListByOwner cannot
// discover a paste it has no index pointer to without a cross-shard scan,
// which the single-shard list path deliberately avoids. The confirm step
// writes the index entry on every insert, so a missing entry only arises
// from a crash between authoritative write and confirm, which the
// reconciler heals.
func (r *ShaleRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Paste, 0, len(idx))
	var staleKeys [][]byte
	for _, item := range idx {
		slugStr := extractSlug(item.Key)
		slug := domain.Slug(slugStr)
		var p pasteRow
		if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Stale: index points at a paste that no longer exists.
				staleKeys = append(staleKeys, append([]byte(nil), item.Key...))
				continue
			}
			return nil, err
		}
		paste := p.toDomain(slug)
		versions, err := r.scanVersions(slug)
		if err != nil {
			return nil, err
		}
		paste.LatestVersion = latestActiveVerNum(versions)
		out = append(out, paste)
	}
	// Repair: drop the stale index entries we found. Best-effort: a
	// failure here is non-fatal to the read (the list is already correct;
	// the entry is simply re-attempted next time).
	for _, k := range staleKeys {
		_ = r.cluster.Delete(k)
	}
	sortByExpiresAt(out)
	return out, nil
}

func (r *ShaleRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	versions, err := r.scanVersions(slug)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Version, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.toDomain(slug))
	}
	sortVersionsDesc(out)
	return out, nil
}

func (r *ShaleRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	var row versionRow
	if err := r.getJSON(shaleKeyVersion(slug, ver), &row); err != nil {
		return domain.Version{}, err
	}
	return row.toDomain(slug), nil
}

func (r *ShaleRepo) CountByOwner(owner string) (int, error) {
	if owner == "" {
		return 0, nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	return len(idx), nil
}

// SumActiveBytesByOwner serves from the identity_bytes counter, a single
// {id}-shard read. Per docs/SPEC.md "One intentional behavior change" the
// counter has no read-time expiry awareness (it sheds an expired paste's
// bytes at sweep time, not read time), so `now` is unused. The counter
// over-counts an expired-unswept paste transiently; it never under-counts.
func (r *ShaleRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	if owner == "" {
		return 0, nil
	}
	raw, err := r.getRaw(shaleKeyIdentityBytes(owner))
	if err != nil {
		return 0, err
	}
	n, err := parseCounter(raw)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (r *ShaleRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	if owner == "" {
		return time.Time{}, nil
	}
	raw, err := r.getRaw(shaleKeyIdentityFirstSeen(owner))
	if err != nil {
		return time.Time{}, err
	}
	if raw == nil {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode first seen: %w", err)
	}
	return t, nil
}

// --- reservation-pattern quota: reserve / release helpers ------------------

// reserveBytes is step 1 of the reservation pattern: a single-shard CAS on
// the {id} shard that atomically checks the per-owner cap and increments
// the identity_bytes (PASTE) counter by `body`, plus writes a reservation
// marker keyed by slug. Returns ErrOverUserQuota if the owner's COMBINED
// paste + site bytes plus `body` would exceed cap. The increment + the
// check are one atomic CAS, so two concurrent reservers serialize: exactly
// one reads the pre-increment counter, the other reads the post-increment
// value and is rejected if it no longer fits. Quota is therefore a hard
// ceiling (docs/SPEC.md "Why quota can never be exceeded").
//
// The cap check sums BOTH the paste counter AND the site counter, so a
// paste insert is rejected if the owner's combined paste+site bytes would
// exceed userCap, the SYMMETRIC twin of reserveSiteBytes (which reads the
// paste counter for the site direction) and matching the sqlite
// identityActiveBytes that spans both kinds. The site counter
// (identity_site_bytes/<id>) co-shards on {id} with the paste counter, so
// reading it inside this CAS is a same-shard read. Only the paste counter
// is incremented here; the site counter is read for the cap but never
// written.
//
// A zero userCap means "no per-owner cap"; the paste counter is still
// incremented (so SumActiveBytesByOwner stays accurate) but the check is
// skipped.
func (r *ShaleRepo) reserveBytes(identity, slug string, body, userCap int64, now time.Time) error {
	counterKey := shaleKeyIdentityBytes(identity)
	siteCounterKey := shaleKeyIdentitySiteBytes(identity)
	reserveKey := shaleKeyIdentityReserve(identity, slug)
	markerVal, err := encodeReservationMarker(body, now)
	if err != nil {
		return err
	}
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if userCap > 0 {
			siteCur, err := txGetCounter(tx, siteCounterKey)
			if err != nil {
				return err
			}
			if cur+siteCur+body > userCap {
				return ErrOverUserQuota
			}
		}
		if err := tx.Put(counterKey, formatCounter(cur+body)); err != nil {
			return err
		}
		return tx.Put(reserveKey, markerVal)
	})
}

// releaseBytes returns `body` bytes to the counter and drops the
// reservation marker, in one {id}-shard CAS. Used to roll back a failed
// reservation. Idempotent on a missing marker: if the marker is already
// gone (a confirm already consumed it, or a prior release ran), the
// counter is left untouched so the bytes are not double-credited.
func (r *ShaleRepo) releaseBytes(identity, slug string, body int64) error {
	counterKey := shaleKeyIdentityBytes(identity)
	reserveKey := shaleKeyIdentityReserve(identity, slug)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		marker, err := tx.Get(reserveKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed/released; do not double-credit
			}
			return err
		}
		m, err := parseReservationMarker(marker)
		if err != nil {
			return err
		}
		amt := m.Bytes
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(counterKey, formatCounter(cur-amt)); err != nil {
			return err
		}
		return tx.Delete(reserveKey)
	})
}

// decrementBytes subtracts `body` from the counter in one {id}-shard CAS,
// with no reservation marker involved. Used by Delete / DeleteVersion to
// shed freed bytes. Clamped at zero by formatCounter.
func (r *ShaleRepo) decrementBytes(identity string, body int64) error {
	if body <= 0 {
		return nil
	}
	counterKey := shaleKeyIdentityBytes(identity)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		return tx.Put(counterKey, formatCounter(cur-body))
	})
}

// --- Shale-blob pointer binding (the transactional shale-blob path) --------
//
// A ShaleBlobUnit.Commit threads a staged blob's BlobRef into the authoritative
// {slug} write through the PER-CALL context.Context, NOT a shared per-repo
// stash. Commit derives a child context carrying THIS call's refs
// (WithPendingBinds) and hands it to the metadata-write closure; the public
// WithQuotaCheck method forwards that context to the authoritative {slug}
// transaction, which reads the refs (pendingBindsFromContext) and BindBlobs
// each one, so the pointer co-commits with the row.
//
// The refs ride the context value, so two concurrent same-slug writes (two
// updates, an update vs a delete, two DeployToSlug) each carry their OWN refs
// and cannot observe or clobber each other's binds: there is no shared mutable
// state keyed by slug. A retry-on-collision loop reuses the same context, so
// every attempt of one call binds the same refs (BindBlob is an idempotent
// tx.Put of the bref key). On the metadata-only path the context carries no
// refs and the authoritative writes take the unchanged no-bind branch.

// pendingBindsKey is the unexported context key the staged refs ride under. A
// dedicated unexported type avoids collisions with any other package's context
// values (the standard context-key idiom).
type pendingBindsKey struct{}

// WithPendingBinds returns a child context carrying refs for the next
// authoritative {slug} write triggered under it. ShaleBlobUnit.Commit installs
// the staged refs here and passes the returned context to the metadata-write
// closure; the public WithQuotaCheck method forwards it to the authoritative
// transaction. nil/empty refs return the parent unchanged (the no-bind path).
func WithPendingBinds(ctx context.Context, refs []cluster.BlobRef) context.Context {
	if len(refs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, pendingBindsKey{}, refs)
}

// pendingBindsFromContext returns the staged refs carried by ctx (or nil). The
// authoritative {slug} writes read it INSIDE the public WithQuotaCheck method
// and pass the refs down to the private authoritative helper, which binds them
// in the same transaction the metadata commits in.
func pendingBindsFromContext(ctx context.Context) []cluster.BlobRef {
	if ctx == nil {
		return nil
	}
	refs, _ := ctx.Value(pendingBindsKey{}).([]cluster.BlobRef)
	return refs
}

// StageBlobStream streams an already-encoded body to the final object via the
// blob-capable cluster (BlobKV.StageBlob), returning the BlobRef the binding
// transaction consumes. It is the repo entry the ShaleBlobUnit.Stage path calls
// (the unit holds *ShaleRepo, not the raw *BlobKV, so the cluster handle stays
// encapsulated). routeKey is the metadata key whose shard the blob co-locates
// with (pastes/<slug> or sites/<slug> - both route to the same unit).
// contentHash is the file's content sha, carried on the ref (BlobRef.ContentHash
// -> the persisted blob.Pointer) and used by the site read path's sha -> blob-id
// side-table.
func (r *ShaleRepo) StageBlobStream(ctx context.Context, routeKey []byte, body io.Reader, size int64, contentHash string) (cluster.BlobRef, error) {
	if r.kv == nil {
		return cluster.BlobRef{}, errors.New("shale: StageBlobStream requires a blob-configured cluster (cfg.BlobStore was nil)")
	}
	ref, err := r.kv.StageBlob(ctx, routeKey, body, size)
	if err != nil {
		return cluster.BlobRef{}, err
	}
	ref.ContentHash = contentHash
	return ref, nil
}

// GetBlobStream streams the bytes for blobid under routeKey's shard (GetBlob).
// The returned stream is the stored (compressed) body; the ShaleBlobUnit.Read
// wraps it in the magic-peek + zstd decode so the read path sees decompressed
// bytes, exactly as the standalone GetReader does. ctx MUST outlive the
// returned reader (it drives the lazy object-store stream). A blob whose pointer
// is gone (unbound/deleted) yields blob.ErrNotFound.
func (r *ShaleRepo) GetBlobStream(ctx context.Context, routeKey []byte, blobid string) (io.ReadCloser, int64, error) {
	if r.kv == nil {
		return nil, 0, errors.New("shale: GetBlobStream requires a blob-configured cluster (cfg.BlobStore was nil)")
	}
	return r.kv.GetBlob(ctx, routeKey, blobid)
}

// SweepBlobOrphans reclaims staged-but-unbound blob objects under this node's
// mounted units, age-gated by grace (SweepOrphans). hostthis schedules it in
// the sweep loop on the shale-blob path. Returns nil (a no-op) on the
// metadata-only path; the caller gates on HasBlobPlane.
func (r *ShaleRepo) SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error {
	if r.kv == nil {
		return nil
	}
	return r.kv.SweepOrphans(ctx, now, grace)
}

// HasBlobPlane reports whether this repo runs the transactional shale-blob path
// (a blob store was configured). The cmd wiring uses it to pick ShaleBlobUnit
// over StandaloneBlobUnit and to schedule SweepOrphans.
func (r *ShaleRepo) HasBlobPlane() bool { return r.kv != nil }

// DebugClusterState returns the embedded shale cluster's per-position handoff
// dump (cluster.DebugState) for LIVE diagnosis, or "" in the single-node path
// where there is no cluster. It is the production-safe equivalent of the
// shaled-binary /debug/shale/state endpoint, exposed so the metadata adapter can
// serve it on an OPTIONAL debug port without leaking the cluster handle.
func (r *ShaleRepo) DebugClusterState() string {
	if r.cluster == nil {
		return ""
	}
	return r.cluster.DebugState()
}

// RouteKeyForSlug returns the canonical metadata route key a slug's blobs co-
// locate with: pastes/<slug>. pastes/<slug> and sites/<slug> both shard on
// <slug>, so they resolve to the SAME unit and the SAME bref key; the blob unit
// uses this one route key for staging, reading, and the unbind-key derivation
// whether the record is a paste or a site. Exposed because shaleKeyPaste is
// unexported and the ShaleBlobUnit (a separate adapter package) needs it.
func (r *ShaleRepo) RouteKeyForSlug(slug string) []byte {
	return shaleKeyPaste(domain.Slug(slug))
}

// ResolveBlobID maps a (slug, contentSHA) the read path holds back to the
// shale blob id GetBlob needs. The metadata carries the blob id (the row's
// BlobID for a paste/version, the site row's FileBlobs side-table for a site
// file); the http/manage read seam only has the content sha, so this routed
// lookup bridges the two. It checks, in order:
//
//  1. the paste head row: its served ContentSHA -> its BlobID (the common
//     path - an HTML/markdown paste read passes the head's ContentSHA),
//  2. the paste's version rows: a non-head (pinned or Show'd) version whose
//     ContentSHA matches -> that version's BlobID,
//  3. the site row's FileBlobs[sha] (a static-site file read).
//
// Returns ("", ErrNotFound) when no metadata references the sha (a deleted /
// unbound blob), which the seam maps to blob.ErrNotFound, the same not-found
// shape a missing object yields. Legacy rows with an empty BlobID return ""
// here; the seam treats "" as "sha-keyed" (no shale blob id), which during a
// migration window means the read falls back - but on a pure shale-blob
// deployment every row carries a BlobID.
func (r *ShaleRepo) ResolveBlobID(slug domain.Slug, contentSHA string) (string, error) {
	// Paste head first (one routed Get).
	var p pasteRow
	perr := r.getJSON(shaleKeyPaste(slug), &p)
	if perr == nil {
		if p.ContentSHA == contentSHA && p.BlobID != "" {
			return p.BlobID, nil
		}
		// Not the head's served sha - scan this slug's versions for a match
		// (a pinned version, or manage Show of a specific version).
		versions, verr := r.scanVersions(slug)
		if verr != nil {
			return "", verr
		}
		for _, v := range versions {
			if v.ContentSHA == contentSHA && v.BlobID != "" {
				return v.BlobID, nil
			}
		}
		return "", ErrNotFound
	}
	if !errors.Is(perr, ErrNotFound) {
		return "", perr
	}
	// Not a paste - try a site file.
	var sr siteRow
	serr := r.getJSON(shaleKeySite(slug), &sr)
	if serr != nil {
		if errors.Is(serr, ErrNotFound) {
			return "", ErrNotFound
		}
		return "", serr
	}
	if id, ok := sr.FileBlobs[contentSHA]; ok && id != "" {
		return id, nil
	}
	return "", ErrNotFound
}

// blobRefFor reconstructs the BlobRef that unbinds blobid's pointer under
// routeKey's shard. UnbindBlob (a tx.Delete of brefKey(ref)) needs only the
// route shard + unit + blob id, all derivable from routeKey purely (the same
// derivation StageBlob/GetBlob use): Unit = RoutedUnitToken(routeKey),
// RouteShard = shaleShardKey(routeKey). Size/ContentHash are irrelevant to the
// bref key, so they stay zero. routeKey is pastes/<slug> (or sites/<slug>); the
// pointer co-shards there, so the unbind tx.Delete lands in the same {slug}
// transaction as the metadata delete.
func (r *ShaleRepo) blobRefFor(routeKey []byte, blobID string) cluster.BlobRef {
	return cluster.BlobRef{
		Unit:       r.kv.Cluster().RoutedUnitToken(routeKey),
		RouteShard: shaleShardKey(routeKey),
		BlobID:     blobID,
	}
}

// --- PasteRepo / PasteAdmin writes -----------------------------------------

// InsertWithQuotaCheck creates a paste via the three-step reservation
// pattern: reserve on {id}, authoritative write on {slug}, confirm on
// {id}. The per-owner cap is enforced strictly by the reserve step's
// atomic CAS. The durable total-bytes ceiling is NOT checked here: it is
// the object-store bucket quota, enforced when the blob Put is rejected
// (see SPEC "Limits -> Durable total-bytes ceiling: an object-store
// quota"); the metadata backend runs no cross-shard byte scan.
func (r *ShaleRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	identity := p.Identity.String()
	slug := p.Slug.String()
	body := int64(p.Size)

	// The staged blob refs (if any) ride this call's context, isolated from
	// any concurrent same-slug write. Read once here and pass them down so the
	// authoritative {slug} transaction binds exactly this call's blobs.
	binds := pendingBindsFromContext(ctx)

	// Step 1: reserve (strict per-owner quota). The marker is stamped with
	// `now` so the reconciler can apply the grace window.
	if err := r.reserveBytes(identity, slug, body, userCap, now); err != nil {
		return err
	}

	// Step 2: authoritative write on the {slug} shard. On any failure
	// here, release the reservation so the bytes are returned (the
	// over-count is bounded to the failure window + the reconciler).
	if err := r.insertAuthoritative(p, binds); err != nil {
		_ = r.releaseBytes(identity, slug, body)
		return err
	}

	// Step 3: confirm on the {id} shard: drop the reservation marker,
	// write the value-bearing index entry, set first-seen if absent. The
	// counter is NOT touched here (the reserve already accounted the
	// bytes); confirm just consumes the marker so the reconciler doesn't
	// later mistake it for an orphan.
	//
	// Deferred off the response path (SPEC "Reservation-pattern quota",
	// step 3). The authoritative paste row already exists, so the paste is
	// Get-readable and its URL never 404s; the bytes were reserved in step
	// 1, so quota is already strict. Confirm only writes the eventually-
	// consistent derived index entry + first-seen, both of which the
	// reconciler heals if this goroutine is lost (confirm was already a
	// non-fatal step whose failure left the index to the reconciler). So
	// InsertWithQuotaCheck returns success now and runs confirm in the
	// background; a lost confirm leaves a "leaked marker" the grace-
	// windowed reconciler pass drops and a missing index entry the
	// reconciler rebuilds, exactly as a synchronous confirm failure did.
	r.confirmWG.Add(1)
	go r.deferredConfirmInsert(p)
	return nil
}

// deferredConfirmInsert runs the confirm CAS off the upload's response
// path. It is launched in a WaitGroup-tracked goroutine by
// InsertWithQuotaCheck after the authoritative write commits, so the
// response does not wait on the derived-index write. A failure is
// non-fatal: the reservation marker it would have dropped becomes a leaked
// marker the reconciler's grace-windowed pass cleans, and the index entry
// it would have written is one the reconciler rebuilds from the
// authoritative rows. It is logged (not returned) because no caller is
// waiting on it. Close / WaitPendingConfirms join on confirmWG.
func (r *ShaleRepo) deferredConfirmInsert(p domain.Paste) {
	defer r.confirmWG.Done()
	if err := r.confirmInsert(p); err != nil {
		r.repoLog().Printf("shale: deferred confirm insert for %s: %v (index lag; reconciler will heal)", p.Slug, err)
	}
}

// shaleKVTx is the minimal transaction surface insertAuthoritative (and the
// other authoritative writes) need: the buffered Get/Put/Delete every shale
// transaction type exposes. BOTH backend.Transaction (the metadata-only path)
// and *cluster.BlobTx (the blob-binding path) satisfy it, so one body serves
// both: the closure runs against backend.Transaction when no blob is bound, and
// against the *BlobTx when a staged blob must co-commit (see runAuthoritative).
type shaleKVTx interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error
}

// runAuthoritative runs body in a {slug}-pinned single-shard CAS transaction.
// When refs is empty (the metadata-only path, or no blob staged) it routes
// through the plain cluster transaction; when refs is non-empty (a staged blob
// must co-commit) it routes through the blob-capable r.kv.Transact so body can
// BindBlob each ref in the SAME transaction. body takes the tx AND a bind
// callback: on the no-bind path bind is a no-op; on the blob path bind issues
// the BindBlobs. This keeps the authoritative-write bodies identical across the
// two paths (the only divergence is whether the binds fire), so the carefully-
// reasoned collision/read-set logic is written once.
func (r *ShaleRepo) runAuthoritative(pinKey []byte, refs []cluster.BlobRef, body func(tx shaleKVTx, bind func() error) error) error {
	if len(refs) == 0 || r.kv == nil {
		return r.cluster.Transact(pinKey, func(tx backend.Transaction) error {
			return body(tx, func() error { return nil })
		})
	}
	return r.kv.Transact(pinKey, func(tx *cluster.BlobTx) error {
		return body(tx, func() error {
			for _, ref := range refs {
				if err := tx.BindBlob(ref); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// insertAuthoritative writes the {slug}-shard authoritative rows in one
// CAS transaction: the paste row, the v1 version row, slug_owner, and the
// expiry index. The slug-collision check (reject if pastes/<slug> OR
// sites/<slug> already exists) participates in the transaction's read-set so
// a racing insert of the same slug (as a paste OR a site) conflicts.
//
// On the transactional shale-blob path the paste's staged blob (passed in via
// refs, carried on the call's context by ShaleBlobUnit.Commit) is BOUND in this
// same transaction, so the pointer co-commits with the row, and the blob id
// lands on both the paste head row and the v1 version row. refs is nil on the
// metadata-only path (no blob plane) or when no blob was staged.
func (r *ShaleRepo) insertAuthoritative(p domain.Paste, refs []cluster.BlobRef) error {
	pasteKey := shaleKeyPaste(p.Slug)
	blobID := firstBlobID(refs)
	return r.runAuthoritative(pasteKey, refs, func(tx shaleKVTx, bind func() error) error {
		// Collision check: a found paste is ErrSlugTaken. The ExpectAbsent
		// read-check makes a concurrent insert of the same slug conflict.
		if _, err := tx.Get(pasteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("slug check: %w", err)
		}
		// A slug must be unique across sites too: reject if a site already
		// owns it. sites/<slug> co-shards with pastes/<slug> on {slug}, so
		// this is a same-shard read inside the CAS. Mirrors the site insert's
		// reciprocal paste-key check and the slatedb paste insert's site-key
		// guard, so a slug is EITHER a paste or a site in both directions.
		if _, err := tx.Get(shaleKeySite(p.Slug)); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("site slug check: %w", err)
		}
		pr := pasteFromDomain(p)
		pr.BlobID = blobID
		if err := shaleTxPutJSON(tx, pasteKey, pr); err != nil {
			return err
		}
		v1 := versionRow{
			VerNum:     1,
			contentRef: contentRef{Kind: string(p.Kind), ContentSHA: p.ContentSHA, BlobID: blobID, Size: p.Size},
			CreatedAt:  p.CreatedAt,
		}
		if err := shaleTxPutJSON(tx, shaleKeyVersion(p.Slug, 1), v1); err != nil {
			return err
		}
		if err := tx.Put(shaleKeySlugOwner(p.Slug), []byte(p.Identity.String())); err != nil {
			return err
		}
		if err := tx.Put(shaleKeyExpiry(p.ExpiresAt, p.Slug), markerValue); err != nil {
			return err
		}
		// Bind the staged blob pointer LAST so it co-commits with the rows.
		return bind()
	})
}

// firstBlobID returns the BlobID of the first ref, or "" when there are none.
// A paste/version has exactly one blob, so the single-blob authoritative writes
// take the head ref's id.
func firstBlobID(refs []cluster.BlobRef) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0].BlobID
}

// fileBlobsFromRefs builds the site row's sha -> blob-id side-table from the
// staged file refs. Each ref's ContentHash is the file's content sha (set by
// StageBlobStream) and BlobID is the staged blob id, so the read path resolves a
// manifest sha to the blob id GetBlob needs. A site dedups identical files, so
// two manifest paths sharing a sha map to one ref/one blob id - the map keys on
// sha, so duplicates collapse correctly. Returns nil for no refs (omitempty
// keeps the row clean on the no-blob path).
func fileBlobsFromRefs(refs []cluster.BlobRef) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if ref.ContentHash != "" {
			out[ref.ContentHash] = ref.BlobID
		}
	}
	return out
}

// confirmInsert is step 3: drop the reservation marker, write the
// value-bearing identity_pastes index entry, and set identity_first_seen
// if absent. All on the {id} shard, one CAS transaction.
func (r *ShaleRepo) confirmInsert(p domain.Paste) error {
	identity := p.Identity.String()
	slug := p.Slug.String()
	reserveKey := shaleKeyIdentityReserve(identity, slug)
	indexKey := shaleKeyIdentityPaste(identity, slug)
	firstSeenKey := shaleKeyIdentityFirstSeen(identity)
	return r.cluster.Transact(reserveKey, func(tx backend.Transaction) error {
		// Drop the reservation marker (it was consumed into the index).
		if _, err := tx.Get(reserveKey); err == nil {
			if err := tx.Delete(reserveKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		if err := shaleTxPutJSON(tx, indexKey, identityPasteRow{
			Name:      p.Name,
			Size:      p.Size,
			CreatedAt: p.CreatedAt,
			ExpiresAt: p.ExpiresAt,
		}); err != nil {
			return err
		}
		// First-seen: write only if absent (cache MIN(created_at)).
		if _, err := tx.Get(firstSeenKey); errors.Is(err, backend.ErrNotFound) {
			return tx.Put(firstSeenKey, []byte(p.CreatedAt.UTC().Format(time.RFC3339Nano)))
		} else if err != nil {
			return err
		}
		return nil
	})
}

// MarkReady flips a paste's status pending -> ready on the {slug} shard,
// the background finalizer's success transition once the blob landed.
// Guarded: it only advances a paste that is still pending, so a late
// finalizer cannot resurrect a paste the reconciler already failed. A
// paste that is already ready is a no-op (idempotent); a paste that is
// failed (or absent) returns without changing anything. See docs/SPEC.md
// "Paste lifecycle status (async blob write)".
func (r *ShaleRepo) MarkReady(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	return r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(pasteKey)
		if errors.Is(err, backend.ErrNotFound) {
			return nil // nothing to advance
		}
		if err != nil {
			return err
		}
		payload, err := stripEnvelope(raw)
		if err != nil {
			return err
		}
		var p pasteRow
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		if domain.NormalizeStatus(p.Status) != domain.PasteStatusPending {
			return nil // already ready, or failed; do not transition
		}
		p.Status = string(domain.PasteStatusReady)
		return shaleTxPutJSON(tx, pasteKey, p)
	})
}

// MarkFailed flips a paste's status pending -> failed and releases its
// reservation (sheds the charged bytes + drops the index entry), the
// background finalizer's failure transition AND the reconciler's
// age-out. The paste ROW stays (flipped to failed) so a read can serve
// an error page; only its byte accounting is undone. Guarded: only a
// still-pending paste transitions, so this never un-counts a ready
// paste. Idempotent on re-call (a failed paste no longer has its index
// entry, so the decrement does not double-apply). See docs/SPEC.md
// "Paste lifecycle status (async blob write)".
func (r *ShaleRepo) MarkFailed(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	// Step 1: flip the {slug}-shard status pending -> failed. Capture the
	// owner + size so step 2 can shed the bytes. If the paste isn't pending
	// (already failed/ready, or absent) there's nothing to release.
	var identity string
	var size int64
	var transitioned bool
	err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		transitioned = false // reset on CAS retry
		raw, err := tx.Get(pasteKey)
		if errors.Is(err, backend.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		payload, err := stripEnvelope(raw)
		if err != nil {
			return err
		}
		var p pasteRow
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		if domain.NormalizeStatus(p.Status) != domain.PasteStatusPending {
			return nil // only a pending paste transitions
		}
		identity = p.Identity
		size = int64(p.Size)
		p.Status = string(domain.PasteStatusFailed)
		if err := shaleTxPutJSON(tx, pasteKey, p); err != nil {
			return err
		}
		transitioned = true
		return nil
	})
	if err != nil || !transitioned {
		return err
	}
	// Step 2: shed the charged bytes on the {id} shard - drop the index
	// entry + decrement the counter. Mirrors Delete's {id}-shard cleanup.
	// The index-entry presence guards against a double-decrement: a second
	// MarkFailed finds the row already failed in step 1 and never reaches
	// here.
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	counterKey := shaleKeyIdentityBytes(identity)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			if err := tx.Delete(indexKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		return tx.Put(counterKey, formatCounter(cur-size))
	})
}

// AppendVersionWithQuotaCheck appends a new version via the reservation
// pattern. The new version's bytes are reserved on the {id} shard
// (strict per-owner quota), then the version row is written + the expiry
// clock reset on the {slug} shard, then the index projection is refreshed
// on the {id} shard.
func (r *ShaleRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	// The new version's staged blob ref (if any) rides this call's context,
	// isolated from any concurrent same-slug append. Read once and pass it down.
	binds := pendingBindsFromContext(ctx)

	// Resolve the owner identity + pin state from the authoritative paste.
	var existing pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	identity := existing.Identity
	body := int64(size)
	slugStr := slug.String()

	// Step 1: reserve. The reservation marker is keyed by a synthetic
	// "<slug>#append" so it never collides with an insert reservation for
	// the same slug.
	reserveSlug := slugStr + "#append"
	if err := r.reserveBytes(identity, reserveSlug, body, userCap, now); err != nil {
		return AppendResult{}, err
	}

	// Determine the next version number from a scan (outside the tx). The
	// authoritative tx re-reads the candidate version key as ExpectAbsent,
	// so a racing append that took the same number conflicts + retries.
	res, err := r.appendAuthoritative(slug, kind, contentSHA, size, now, binds)
	if err != nil {
		_ = r.releaseBytes(identity, reserveSlug, body)
		return AppendResult{}, err
	}

	// Step 3: refresh the index projection (size/expiry changed) + drop
	// the append reservation marker. Best-effort; reconciler heals a lag.
	if err := r.confirmAppend(identity, slug, reserveSlug, now.Add(domain.RetentionWindow)); err != nil {
		return res, fmt.Errorf("confirm append: %w", err)
	}
	return res, nil
}

// errVerTaken is the internal signal that the candidate version number was
// already present at read time inside the authoritative tx: the closure
// aborts with it (shale's Transact returns a non-conflict fn error
// verbatim), and the outer loop re-scans for a fresh number and retries.
// It never escapes appendAuthoritative.
var errVerTaken = errors.New("shale: candidate version number already taken")

// appendAuthoritative writes the new version row + resets the expiry clock
// on the {slug} shard. The next version number is computed from a
// pre-scan, then a single-shard CAS writes it. Two race outcomes are
// retried by re-scanning for a fresh number:
//
//   - the candidate version key is ALREADY present at read time inside the
//     tx (a concurrent append committed it before this scan): the closure
//     returns errVerTaken, which we catch + re-number,
//   - the candidate key is absent at read time but a concurrent append
//     commits it first: this commit's read-set (the ExpectAbsent check on
//     the version key) fails validation, Transact returns ErrCASConflict,
//     which we catch + re-number.
//
// MAX(ver_num) counts tombstones, so version numbers are never reused.
func (r *ShaleRepo) appendAuthoritative(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, now time.Time, refs []cluster.BlobRef) (AppendResult, error) {
	pasteKey := shaleKeyPaste(slug)
	newExpiry := now.Add(domain.RetentionWindow)
	// The new version's staged blob (if any) was passed in via refs (carried on
	// the call's context by Commit); it binds in the SAME {slug} transaction as
	// the version row. The blob id lands on the new version row and, when the
	// head is unpinned (so the public URL follows this version), on the paste
	// head row too.
	blobID := firstBlobID(refs)
	const maxRenumberAttempts = 16
	for range maxRenumberAttempts {
		versions, err := r.scanVersions(slug)
		if err != nil {
			return AppendResult{}, err
		}
		newVer := maxVerNum(versions) + 1
		verKey := shaleKeyVersion(slug, newVer)

		var wasPinned bool
		txErr := r.runAuthoritative(pasteKey, refs, func(tx shaleKVTx, bind func() error) error {
			var p pasteRow
			if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
				return err
			}
			// The candidate number must be free. Reading it records an
			// ExpectAbsent read-check so a concurrent commit of the same key
			// after this read conflicts at Commit time; a value already
			// present here means another append beat us, so re-number.
			if _, gerr := tx.Get(verKey); gerr == nil {
				return errVerTaken
			} else if !errors.Is(gerr, backend.ErrNotFound) {
				return gerr
			}
			wasPinned = p.PinnedVersion != 0

			newV := versionRow{
				VerNum:     newVer,
				contentRef: contentRef{Kind: string(kind), ContentSHA: contentSHA, BlobID: blobID, Size: size},
				CreatedAt:  now,
			}
			if err := shaleTxPutJSON(tx, verKey, newV); err != nil {
				return err
			}
			// Move the expiry index entry (its date segment changes).
			if err := tx.Delete(shaleKeyExpiry(p.ExpiresAt, slug)); err != nil {
				return err
			}
			p.UpdatedAt = now
			p.ExpiresAt = newExpiry
			if p.PinnedVersion == 0 {
				p.contentRef = newV.contentRef // unpinned head rolls to the new version, whole
			}
			if err := shaleTxPutJSON(tx, pasteKey, p); err != nil {
				return err
			}
			if err := tx.Put(shaleKeyExpiry(p.ExpiresAt, slug), markerValue); err != nil {
				return err
			}
			// Bind the new version's blob pointer, co-committed with the rows.
			return bind()
		})
		switch {
		case txErr == nil:
			return AppendResult{NewVer: newVer, WasPinned: wasPinned}, nil
		case errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict):
			continue // re-scan + re-number
		default:
			return AppendResult{}, txErr
		}
	}
	return AppendResult{}, fmt.Errorf("shale: append %q: could not allocate a free version number after %d attempts", slug, maxRenumberAttempts)
}

// confirmAppend refreshes the index projection's size/expiry for the
// paste's owner and drops the append reservation marker, on the {id}
// shard. The projection's Size mirrors the paste head size; ListByOwner's
// authoritative LatestVersion read does not depend on it, but keeping it
// fresh avoids a stale denormalized size in the list view.
func (r *ShaleRepo) confirmAppend(identity string, slug domain.Slug, reserveSlug string, newExpiry time.Time) error {
	reserveKey := shaleKeyIdentityReserve(identity, reserveSlug)
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	return r.cluster.Transact(reserveKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(reserveKey); err == nil {
			if err := tx.Delete(reserveKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		var row identityPasteRow
		if err := shaleTxGetJSON(tx, indexKey, &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil // index entry not present; reconciler rebuilds
			}
			return err
		}
		row.ExpiresAt = newExpiry
		return shaleTxPutJSON(tx, indexKey, row)
	})
}

// Delete removes a paste entirely (whole-paste delete is a full removal,
// not a tombstone): the authoritative {slug} rows go away and the freed
// bytes are decremented from the owner's {id} counter. Idempotent on a
// missing paste.
func (r *ShaleRepo) Delete(slug domain.Slug) error {
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	versions, err := r.scanVersions(slug)
	if err != nil {
		return err
	}
	identity := p.Identity

	// Authoritative removal on the {slug} shard, one CAS. The freed bytes
	// are computed INSIDE the transaction by re-reading each version's
	// tombstone state, so the count matches exactly what this Delete
	// removes. The re-read also puts every version key in the CAS read-set:
	// a concurrent DeleteVersion that tombstoned (and already decremented)
	// a version commits a change to that key, which conflicts this CAS and
	// forces a retry that re-reads the now-tombstoned version and excludes
	// it from `freed`. That closes the same-slug Delete-vs-DeleteVersion
	// double-decrement (an under-count) the pre-transaction scan was
	// exposed to.
	var freed int64
	pasteKey := shaleKeyPaste(slug)
	// On the transactional shale-blob path the paste's blobs are unbound in the
	// SAME {slug} transaction (atomic delete): each live version's pointer is
	// removed via unbind, so the bytes go unreferenced exactly when the rows
	// vanish, and SweepOrphans reclaims them after the grace. unbind is a no-op
	// on the metadata-only path (the global content-addressed sweep reclaims).
	delBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		freed = 0 // reset: the closure re-runs on a CAS conflict
		if err := tx.Delete(pasteKey); err != nil {
			return err
		}
		for _, v := range versions {
			vKey := shaleKeyVersion(slug, v.VerNum)
			raw, gerr := tx.Get(vKey) // read-set: detects a concurrent tombstone
			if errors.Is(gerr, backend.ErrNotFound) {
				continue // already gone
			}
			if gerr != nil {
				return gerr
			}
			payload, serr := stripEnvelope(raw)
			if serr != nil {
				return serr
			}
			var vr versionRow
			if jerr := json.Unmarshal(payload, &vr); jerr != nil {
				return jerr
			}
			if !vr.Deleted {
				freed += int64(vr.Size) // only bytes still live count toward the decrement
			}
			// Unbind this version's blob (the bref pointer) so its bytes go
			// unreferenced. A tombstoned version's pointer was already unbound
			// by DeleteVersion, so re-unbinding (an idempotent tx.Delete of a
			// missing key) is harmless. Only the BlobID-carrying rows (the
			// shale-blob path) have a pointer; legacy rows carry no BlobID and
			// unbind is skipped.
			if vr.BlobID != "" {
				if err := unbind(vr.BlobID); err != nil {
					return err
				}
			}
			if err := tx.Delete(vKey); err != nil {
				return err
			}
		}
		if err := tx.Delete(shaleKeySlugOwner(slug)); err != nil {
			return err
		}
		return tx.Delete(shaleKeyExpiry(p.ExpiresAt, slug))
	}
	if r.kv != nil {
		err = r.kv.Transact(pasteKey, func(tx *cluster.BlobTx) error {
			return delBody(tx, func(blobID string) error {
				return tx.UnbindBlob(r.blobRefFor(pasteKey, blobID))
			})
		})
	} else {
		err = r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
			return delBody(tx, func(string) error { return nil })
		})
	}
	if err != nil {
		return err
	}

	// Derived cleanup on the {id} shard: drop the index entry + decrement
	// the counter by the freed bytes. Two {id}-shard transactions (the
	// index delete and the counter decrement are independent; combine into
	// one CAS pinned on the {id} shard since both keys co-shard).
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	counterKey := shaleKeyIdentityBytes(identity)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			if err := tx.Delete(indexKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		return tx.Put(counterKey, formatCounter(cur-freed))
	})
}

// DeleteVersion tombstones a single version (Q1 = Option 2: the version
// stays visible in the list flagged deleted, but its content blob is no
// longer referenced so the GC reclaims it). The freed bytes are
// decremented from the owner's counter. A re-delete of an already-
// tombstoned version is a repo-level no-op (no double-decrement).
func (r *ShaleRepo) DeleteVersion(slug domain.Slug, ver int) error {
	// Resolve owner for the counter decrement.
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		return err
	}
	identity := p.Identity
	verKey := shaleKeyVersion(slug, ver)
	// The tombstone tx pins on verKey, but the blob pointer (bref) routes on the
	// {slug} shard - the SAME unit verKey routes to (versions/<slug>/<NNNN> and
	// pastes/<slug> both shard on <slug>), so the unbind co-commits with the
	// tombstone in one single-shard transaction. Each version's blob has a
	// unique stage-minted blob id (no within-record dedup in this phase), so a
	// tombstoned version's blob is referenced by no live sibling and is safe to
	// unbind unconditionally; the served (head) version cannot be deleted (the
	// service guards it), so the head row's blob id is never the one unbound here.
	pasteKey := shaleKeyPaste(slug)

	var freed int64
	verBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		var v versionRow
		if err := shaleTxGetJSON(tx, verKey, &v); err != nil {
			return err
		}
		if v.Deleted {
			freed = 0 // already tombstoned; no-op
			return nil
		}
		freed = int64(v.Size)
		v.Deleted = true
		if err := shaleTxPutJSON(tx, verKey, v); err != nil {
			return err
		}
		if v.BlobID != "" {
			return unbind(v.BlobID)
		}
		return nil
	}
	var err error
	if r.kv != nil {
		err = r.kv.Transact(verKey, func(tx *cluster.BlobTx) error {
			return verBody(tx, func(blobID string) error {
				return tx.UnbindBlob(r.blobRefFor(pasteKey, blobID))
			})
		})
	} else {
		err = r.cluster.Transact(verKey, func(tx backend.Transaction) error {
			return verBody(tx, func(string) error { return nil })
		})
	}
	if err != nil {
		return err
	}
	if freed == 0 {
		return nil
	}
	return r.decrementBytes(identity, freed)
}

func (r *ShaleRepo) SetName(slug domain.Slug, name string) error {
	pasteKey := shaleKeyPaste(slug)
	if err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		p.Name = name
		return shaleTxPutJSON(tx, pasteKey, p)
	}); err != nil {
		return err
	}
	// Best-effort: refresh the denormalized name in the index projection.
	indexKey := shaleKeyIdentityPaste(r.ownerOfSlug(slug), slug.String())
	_ = r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		var row identityPasteRow
		if err := shaleTxGetJSON(tx, indexKey, &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		row.Name = name
		return shaleTxPutJSON(tx, indexKey, row)
	})
	return nil
}

// ownerOfSlug resolves a slug's owner identity from slug_owner (a single
// {slug}-shard read). Returns "" if the paste is gone, in which case the
// best-effort index refresh it feeds is a harmless no-op.
func (r *ShaleRepo) ownerOfSlug(slug domain.Slug) string {
	raw, err := r.getRaw(shaleKeySlugOwner(slug))
	if err != nil || raw == nil {
		return ""
	}
	return string(raw)
}

func (r *ShaleRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	pasteKey := shaleKeyPaste(slug)
	return r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		// Repoint the head's served descriptor at the pinned version's, as ONE
		// value. The version row carries the full contentRef (incl. BlobID,
		// which domain.Version does not), so the head can't drift a field. The
		// version row co-shards on {slug}, so this read is inside the same CAS.
		var vr versionRow
		if err := shaleTxGetJSON(tx, shaleKeyVersion(slug, ver.VerNum), &vr); err != nil {
			return err
		}
		p.PinnedVersion = ver.VerNum
		p.contentRef = vr.contentRef
		return shaleTxPutJSON(tx, pasteKey, p)
	})
}

func (r *ShaleRepo) Unpin(slug domain.Slug) error {
	// Resolve the latest version's head fields from a scan (outside tx).
	versions, err := r.scanVersions(slug)
	if err != nil {
		return err
	}
	var latest *versionRow
	for i := range versions {
		if latest == nil || versions[i].VerNum > latest.VerNum {
			latest = &versions[i]
		}
	}
	if latest == nil {
		return ErrNotFound
	}
	pasteKey := shaleKeyPaste(slug)
	return r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		p.PinnedVersion = 0
		p.contentRef = latest.contentRef // whole served descriptor rolls to the latest version
		return shaleTxPutJSON(tx, pasteKey, p)
	})
}

// --- SweepRepo -------------------------------------------------------------

// ExpiredSlugs fans out across all {slug} shards (cluster.Aggregate) over
// the expiry/* index and returns the slugs whose expiry timestamp is <=
// now. The timestamp is the middle segment of expiry/<rfc3339>/<slug>;
// RFC3339Nano sorts lexicographically so a string compare is correct at
// whole-second granularity. NOTE: RFC3339Nano is variable-width (a
// fractional ".5Z" sorts before a bare "Z" within one whole second), so
// this paste index carries the same latent sub-second skew documented for
// the paste expiry path; fixing it is a key-format migration left as a
// follow-up. The site expiry index uses a fixed-width format
// (expirySiteTimeFormat) and has no such skew.
func (r *ShaleRepo) ExpiredSlugs(now time.Time) ([]string, error) {
	items, err := r.aggregatePrefix([]byte("expiry/"))
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(time.RFC3339Nano)
	var out []string
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), "expiry/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		ts := rest[:idx]
		slug := rest[idx+1:]
		if ts <= cutoff {
			out = append(out, slug)
		}
	}
	return out, nil
}

// ReferencedBlobSHAs returns the set of blob content-SHAs still referenced
// by a LIVE (non-deleted) version or paste head, aggregated across all
// {slug} shards. The sweep treats the returned slice as the allow-list:
// any blob whose sha is NOT in it is GC'd. A tombstoned version's sha is
// excluded (Q1 = Option 2: a deleted version is app-final and
// content-inaccessible, so its blob is GC-eligible). Dedup-safe: a sha
// shared by another live version stays referenced.
//
// Guard against a repo that returns 0 referenced shas while pastes exist,
// which would make the sweep delete every blob: this method never returns
// nil with a nil error, and the sweep's abort-on-zero-refs guard backstops
// it. An empty result is only legitimate for an empty keyspace.
func (r *ShaleRepo) ReferencedBlobSHAs() ([]string, error) {
	pastes, err := r.aggregatePrefix([]byte("pastes/"))
	if err != nil {
		return nil, err
	}
	versions, err := r.aggregatePrefix([]byte("versions/"))
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]struct{}, len(pastes)+len(versions))
	for _, item := range pastes {
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// FAIL CLOSED, never skip. This is the blob-GC keep-set: skipping
			// an undecodable row would UNDER-COUNT references, so a blob the
			// row still references would look orphaned and be deleted
			// (irreversible). Abort the whole pass with the error; the sweep
			// caller treats any error here as "delete nothing this pass". The
			// opposite of the Reconcile/expiry skip+log policy by design. See
			// docs/SPEC.md "Decode tolerance is per-scan-semantics", Policy 2.
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if p.ContentSHA != "" {
			referenced[p.ContentSHA] = struct{}{}
		}
	}
	for _, item := range versions {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			// FAIL CLOSED, never skip (same reasoning as the paste loop
			// above): an under-counted ref-set deletes a live blob. Policy 2.
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if v.ContentSHA != "" && !v.Deleted {
			referenced[v.ContentSHA] = struct{}{}
		}
	}
	out := make([]string, 0, len(referenced))
	for sha := range referenced {
		out = append(out, sha)
	}
	return out, nil
}

// --- KeyGateRepo (Sybil rate limit) ----------------------------------------

// AdmitNewKey atomically checks + admits a fresh (identity, subnet) pair
// on the {subnet} shard. The check (count in-window rows + the per-subnet
// cap) and the admit (write the row) are ONE CAS transaction, so two
// concurrent admissions for the same subnet serialize. The in-window count
// is computed from a pre-scan (scans are not allowed inside the tx); the
// candidate row's key is read ExpectAbsent inside the tx so the admit
// participates in conflict detection, and a racing admit of the same pair
// makes the second a known-already result.
func (r *ShaleRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, errors.New("identity + subnet required")
	}
	rowKey := shaleKeyKeygate(subnet, identity)

	// Fast path: already known? (single-shard read; no accounting.)
	if raw, err := r.getRaw(rowKey); err != nil {
		return false, err
	} else if raw != nil {
		return true, nil
	}

	// Count fresh in-window rows from a pre-scan (outside the tx).
	items, err := r.scanPrefix(shalePrefixKeygateSubnet(subnet))
	if err != nil {
		return false, err
	}
	cutoff := now.Add(-window)
	freshCount := 0
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if t.After(cutoff) {
			freshCount++
		}
	}
	if freshCount >= limitPerSubnet {
		return false, ErrTooManyNewKeys
	}

	known := false
	txErr := r.cluster.Transact(rowKey, func(tx backend.Transaction) error {
		// Re-read inside the tx: a concurrent admit of the same pair makes
		// this a known result; the ExpectAbsent read-check otherwise makes
		// a racing admit conflict + retry (re-checking the count).
		if _, gerr := tx.Get(rowKey); gerr == nil {
			known = true
			return nil
		} else if !errors.Is(gerr, backend.ErrNotFound) {
			return gerr
		}
		return tx.Put(rowKey, []byte(now.UTC().Format(time.RFC3339Nano)))
	})
	if txErr != nil {
		return false, txErr
	}
	return known, nil
}

// SubnetSnapshot counts in-window rows for a subnet + finds the oldest
// first_seen value among them. Single-shard scan on the {subnet} shard.
func (r *ShaleRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	items, err := r.scanPrefix(shalePrefixKeygateSubnet(subnet))
	if err != nil {
		return 0, time.Time{}, err
	}
	cutoff := now.Add(-window)
	count := 0
	var oldest time.Time
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if !t.After(cutoff) {
			continue
		}
		count++
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return count, oldest, nil
}

// SubnetsForIdentity counts distinct in-window subnets for an identity.
// Cross-shard (a keygate row could live on any {subnet} shard), so it
// fans out via aggregate over the global keygate prefix.
func (r *ShaleRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	items, err := r.aggregatePrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	seen := make(map[string]struct{})
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), "keygate/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		subnet := rest[:idx]
		id := rest[idx+1:]
		if id != identity {
			continue
		}
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if !t.After(cutoff) {
			continue
		}
		seen[subnet] = struct{}{}
	}
	return len(seen), nil
}

// DeleteFirstSeenOlderThan prunes keygate rows whose first-seen is before
// cutoff, across all {subnet} shards. The candidate set is gathered via a
// cross-shard aggregate; each delete is routed to its owning shard.
func (r *ShaleRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	items, err := r.aggregatePrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, item := range items {
		t, perr := time.Parse(time.RFC3339Nano, string(item.Value))
		if perr != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := r.cluster.Delete(item.Key); err != nil {
				return deleted, fmt.Errorf("delete %s: %w", item.Key, err)
			}
			deleted++
		}
	}
	return deleted, nil
}

// --- reconciler ------------------------------------------------------------

// Reconcile heals the derived per-identity family and completes the
// reservation markers the hot path left behind. It is the convergence
// mechanism the reservation pattern relies on (docs/SPEC.md "Running the
// reconciler against live traffic"). It has exactly two jobs, and
// NEITHER ever overwrites the identity_bytes counter with an absolute
// value derived from a scan:
//
//   - rebuild identity_pastes/<id>/<slug> projections from the
//     authoritative paste rows (adding missing entries, refreshing stale
//     ones, dropping entries whose paste is gone). This touches only the
//     derived index, never the counter (reconcileIndexes).
//   - complete identity_reserve/<id>/<slug> markers older than
//     reserveGrace: an ABANDONED reservation (paste absent) is released
//     with a targeted, read-checked CAS that does counter -= marker.bytes
//     AND deletes the marker atomically; a LEAKED confirm-failed marker
//     (paste exists) is simply deleted, leaving the counter untouched
//     because the bytes are already authoritatively counted. Markers
//     younger than the grace are in-flight inserts and left strictly
//     alone (releaseReservationMarkers).
//
// The counter is NEVER recomputed. It is maintained purely by the strict,
// read-checked CAS deltas on the hot path (reserve adds, delete /
// delete-version subtracts, orphan-release subtracts). An earlier
// recompute that rebuilt the counter from a cross-shard live-byte scan
// was structurally racy (it could under-count across the scan ->
// tx.Get(counter) window and let an owner over-admit past the cap) and is
// removed. See docs/SPEC.md "Why never recompute".
//
// It is NOT part of the SweepRepo contract: the sweep's public surface is
// unchanged. An operator (or a sweep hook that calls this directly) runs
// it periodically. Single-node, cross-shard via aggregate; safe to run
// concurrently with live traffic because every write it makes is either a
// derived-index repair or an idempotent, targeted, read-checked CAS delta
// on a single {id} shard, never a full-scan counter overwrite.
func (r *ShaleRepo) Reconcile(now time.Time, reserveGrace time.Duration) error {
	// Gather authoritative state across all shards.
	pasteItems, err := r.aggregatePrefix([]byte("pastes/"))
	if err != nil {
		return fmt.Errorf("reconcile: scan pastes: %w", err)
	}

	// The set of slugs that have an authoritative paste, used to decide
	// whether a past-grace marker is an abandoned reservation (paste
	// absent -> orphan-release, decrement) or a leaked confirm-failed
	// marker (paste present -> drop without touching the counter).
	livePasteSlugs := make(map[string]struct{}, len(pasteItems))
	pastesByOwner := make(map[string]map[string]identityPasteRow)
	// stalePending collects slugs whose status is pending and whose
	// created_at is older than the pending timeout: the pod-death backstop
	// (the in-memory bytes are gone, no finalizer will ever run). They are
	// aged to failed AFTER the scan + index rebuild so a too-old pending's
	// index entry does not get re-projected by reconcileIndexes on the same
	// pass. See docs/SPEC.md "Reconciler: age out stuck pendings".
	var stalePending []domain.Slug
	for _, item := range pasteItems {
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// Idempotent reconcile: skip + log the undecodable row and
			// continue. One poisoned paste must not stall the whole pass
			// (which would freeze index rebuild + reservation release for
			// every healthy owner); the next tick retries it. See docs/SPEC.md
			// "Decode tolerance is per-scan-semantics", Policy 1.
			r.repoLog().Printf("reconcile: skip undecodable paste %s: %v", item.Key, err)
			// The row PHYSICALLY exists (we just cannot decode it), so mark
			// its slug LIVE before continuing. Otherwise a past-grace
			// reservation marker for this slug would be classified as an
			// abandoned reservation (paste absent -> orphan-release ->
			// decrement the owner's byte counter) for a paste that still
			// exists, UNDER-COUNTING the owner's quota. Treating it as present
			// makes a lingering marker a leaked-confirm (dropped, counter
			// untouched). We still skip THIS slug's derived-index rebuild
			// (we cannot read the row), which the next tick retries.
			livePasteSlugs[strings.TrimPrefix(string(item.Key), "pastes/")] = struct{}{}
			continue
		}
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		livePasteSlugs[slug] = struct{}{}
		if pastesByOwner[p.Identity] == nil {
			pastesByOwner[p.Identity] = make(map[string]identityPasteRow)
		}
		pastesByOwner[p.Identity][slug] = identityPasteRow{
			Name:      p.Name,
			Size:      p.Size,
			CreatedAt: p.CreatedAt,
			ExpiresAt: p.ExpiresAt,
		}
		// Age-out check: a pending paste older than the timeout is a
		// pod-death casualty (its in-memory bytes never reached the blob
		// store). Defer the transition until after the index rebuild.
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusPending &&
			now.Sub(p.CreatedAt) > PendingPasteTimeout {
			stalePending = append(stalePending, domain.Slug(slug))
		}
	}

	// Gather the reservation markers (cross-shard) and group them by owner.
	markersByOwner, err := r.gatherReservationMarkers()
	if err != nil {
		return err
	}

	// Job 1: rebuild the derived index (never touches the counter).
	if err := r.reconcileIndexes(pastesByOwner); err != nil {
		return err
	}
	// Job 1b: age out stuck pending pastes (pod-death backstop). Each
	// MarkFailed is independent + idempotent; a failure on one slug is
	// logged and skipped so it cannot stall the rest of the pass (the next
	// tick retries it), the same per-row discipline the other jobs use.
	for _, slug := range stalePending {
		if err := r.MarkFailed(slug); err != nil {
			r.repoLog().Printf("reconcile: age-out pending %s: %v", slug, err)
		}
	}
	// Job 2: complete past-grace paste markers (orphan-release / leaked-drop).
	if err := r.releaseReservationMarkers(now, reserveGrace, markersByOwner, livePasteSlugs); err != nil {
		return err
	}
	// Job 3: complete past-grace SITE markers, the exact same orphan-release /
	// leaked-drop rule applied to identity_site_reserve/ against the SITE
	// counter (shale_site_repo.go). It is the backstop for a deploy that
	// crashed between the site reserve and the authoritative write (the hot
	// path's release covers a failed write; the reconciler covers a crash).
	return r.reconcileSiteReservations(now, reserveGrace)
}

// reservationMarkerEntry is a parsed reservation marker tagged with its
// full identity_reserve/<id>/<slug> key, used by the reconciler to
// complete past-grace markers (orphan-release or leaked-marker drop).
type reservationMarkerEntry struct {
	Key    []byte
	Marker reservationMarker
}

// gatherReservationMarkers scans every identity_reserve marker across all
// shards, parses each (tolerating legacy bare-number markers), and groups
// them by owner identity.
func (r *ShaleRepo) gatherReservationMarkers() (map[string][]reservationMarkerEntry, error) {
	items, err := r.aggregatePrefix([]byte("identity_reserve/"))
	if err != nil {
		return nil, fmt.Errorf("reconcile: scan reservations: %w", err)
	}
	byOwner := make(map[string][]reservationMarkerEntry)
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), "identity_reserve/")
		before, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		owner := before
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			// Idempotent reconcile: skip + log the undecodable marker and
			// continue. A single poisoned reservation marker must not stall
			// the reconciler pass for every other owner; the next tick
			// retries it. See docs/SPEC.md "Decode tolerance is
			// per-scan-semantics", Policy 1.
			r.repoLog().Printf("reconcile: skip undecodable reservation marker %s: %v", item.Key, err)
			continue
		}
		byOwner[owner] = append(byOwner[owner], reservationMarkerEntry{
			Key:    append([]byte(nil), item.Key...),
			Marker: m,
		})
	}
	return byOwner, nil
}

// markerInGrace reports whether a marker is still within the grace window
// (now - created_at <= reserveGrace), i.e. a genuinely in-flight insert
// whose bytes belong in the true counter value. A marker past grace is an
// abandoned-or-leaked reservation excluded from the recompute.
func markerInGrace(m reservationMarker, now time.Time, reserveGrace time.Duration) bool {
	return now.Sub(m.CreatedAt) <= reserveGrace
}

// reconcileIndexes rebuilds identity_pastes projections to match the
// authoritative paste set per owner: adds missing, refreshes stale, drops
// entries with no authoritative paste.
func (r *ShaleRepo) reconcileIndexes(pastesByOwner map[string]map[string]identityPasteRow) error {
	for owner, want := range pastesByOwner {
		have, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
		if err != nil {
			return fmt.Errorf("reconcile: scan index %s: %w", owner, err)
		}
		for _, item := range have {
			slug := extractSlug(item.Key)
			if _, ok := want[slug]; !ok {
				// Stale: the authoritative paste is gone; drop the index entry.
				_ = r.cluster.Delete(item.Key)
			}
		}
		for slug, row := range want {
			// Refresh every wanted entry (idempotent; covers add + update).
			if err := txPutIndex(r, owner, slug, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// txPutIndex writes a single identity_pastes projection via a {id}-shard
// CAS (idempotent overwrite).
func txPutIndex(r *ShaleRepo, owner, slug string, row identityPasteRow) error {
	key := shaleKeyIdentityPaste(owner, slug)
	return r.cluster.Transact(key, func(tx backend.Transaction) error {
		return shaleTxPutJSON(tx, key, row)
	})
}

// releaseReservationMarkers completes past-grace reservation markers,
// honoring the grace window so an in-flight insert's marker is never
// mistaken for an orphan (docs/SPEC.md "Grace window for reservation
// completion"). For each marker:
//
//   - YOUNGER than reserveGrace: in-flight (between the reserve step and
//     the authoritative write). Skip it. Its bytes are already in the
//     counter (the reserve added them) and the confirm step is expected
//     to land shortly. The counter is never recomputed, so the marker's
//     bytes stay correctly counted while it is in flight.
//   - OLDER than reserveGrace, paste ABSENT (orphan-reserve-release): the
//     insert was abandoned, so the reserved bytes never became live. This
//     is the ONLY reconciler write that touches the counter: a targeted,
//     read-checked CAS on the {id} shard that does counter -= marker.bytes
//     AND deletes the marker atomically (orphanReleaseMarker). Idempotent
//     (a concurrent confirm or a prior pass that already consumed the
//     marker leaves nothing to release). It only ever decrements abandoned
//     reservations whose bytes were never consumed by a live paste, so it
//     can never under-count a live paste.
//   - OLDER than reserveGrace, paste PRESENT (leaked-marker drop): a
//     confirm that failed after the authoritative write succeeded left the
//     marker behind even though the bytes are already authoritatively
//     counted. Delete the marker WITHOUT touching the counter
//     (decrementing here would under-count the live paste). This is what
//     bounds the marker family: without it a confirm-failed marker for a
//     still-live paste would leak unboundedly.
//
// An <slug>#append marker maps to the base <slug>'s paste: an append
// targets an existing paste, so a past-grace append marker is virtually
// always the leaked-marker case (the paste is present), which drops the
// marker without moving the counter.
func (r *ShaleRepo) releaseReservationMarkers(now time.Time, reserveGrace time.Duration, markersByOwner map[string][]reservationMarkerEntry, livePasteSlugs map[string]struct{}) error {
	for _, markers := range markersByOwner {
		for _, e := range markers {
			if markerInGrace(e.Marker, now, reserveGrace) {
				continue // in-flight; leave it for the confirm step to drop
			}
			baseSlug := strings.TrimSuffix(markerSlugFromKey(e.Key), "#append")
			if _, pasteExists := livePasteSlugs[baseSlug]; pasteExists {
				// Leaked confirm-failed marker: the bytes are already
				// authoritatively counted. Drop the marker, leave the
				// counter alone.
				_ = r.cluster.Delete(e.Key)
				continue
			}
			// Abandoned reservation: the paste never materialized. Release
			// the over-count with a targeted, read-checked CAS that consumes
			// the marker atomically.
			if err := r.orphanReleaseMarker(e.Key); err != nil {
				return fmt.Errorf("reconcile: orphan-release %s: %w", e.Key, err)
			}
		}
	}
	return nil
}

// orphanReleaseMarker is the targeted, read-checked CAS that releases one
// abandoned reservation: in a single {id}-shard Transact it reads the
// counter and the marker, and if the marker is still present, subtracts
// the marker's bytes from the counter AND deletes the marker, atomically.
// It is the only counter write the reconciler ever makes, and it is a
// strict delta (counter -= marker.bytes), never an absolute overwrite.
//
// Idempotent: a marker already consumed (by a concurrent confirm, a
// release, or a prior reconcile pass) is read as absent here and the CAS
// is a no-op, so the bytes are never double-credited. Pinning the
// transaction on the counter key (which co-shards with the marker, both
// keyed by {id}) makes the read of the marker and the counter, and the
// two writes, one atomic single-shard CAS.
func (r *ShaleRepo) orphanReleaseMarker(reserveKey []byte) error {
	owner := markerOwnerFromKey(reserveKey)
	counterKey := shaleKeyIdentityBytes(owner)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(reserveKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed; no-op (never double-credit)
			}
			return err
		}
		m, err := parseReservationMarker(raw)
		if err != nil {
			return err
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(counterKey, formatCounter(cur-m.Bytes)); err != nil {
			return err
		}
		return tx.Delete(reserveKey)
	})
}

// markerOwnerFromKey extracts <id> from an identity_reserve/<id>/<slug>
// key. The <id> is everything between the prefix and the FIRST "/".
func markerOwnerFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_reserve/")
	if before, _, ok := strings.Cut(rest, "/"); ok {
		return before
	}
	return rest
}

// markerSlugFromKey extracts <slug> from an identity_reserve/<id>/<slug>
// key. The <slug> is everything after the FIRST "/" (an identity never
// contains a "/", and the slug segment may itself be "<slug>#append").
func markerSlugFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_reserve/")
	if _, after, ok := strings.Cut(rest, "/"); ok {
		return after
	}
	return ""
}
