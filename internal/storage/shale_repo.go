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
//	identity_pastes/<id>/<slug>        -> shard key <id>    (per-owner enumeration index)
//	identity_first_seen/<id>           -> shard key <id>
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
// # Cross-family writes and the scan-derived quota
//
// Insert / AppendVersion / Delete / DeleteVersion span the {slug}
// authoritative shard and the {id} enumeration-index shard, which cannot
// be one transaction. There is deliberately NO stored per-owner byte
// counter: the per-identity quota is DERIVED by ONE single-shard prefix
// scan of the owner's identity_pastes / identity_sites enumeration index,
// summing the cached (size, expires_at) each value-bearing entry carries -
// zero per-entry fan-out to the {slug} shards (docs/SPEC.md "Scan-derived
// quota"). A write is therefore a plain sequence: check the quota (scan),
// authoritative write on {slug}, then best-effort enumeration-index write
// on {id}. The freshness contract: every size-changing operation maintains
// the cached size (insert seeds it, append + version-tombstone refresh it,
// delete/fail drop the entry), and the reconciler's reprojection rebuilds
// every entry's cached values from the authoritative rows - so an
// identity's DURABLE used bytes can never exceed the cap; the transient
// gaps (a crash between the row write and the index write, a lost cache
// refresh, an orphaned entry, two concurrent same-owner uploads passing
// the non-atomic check) are each bounded by one record's bytes and healed
// within one reconcile cycle. The reconciler reprojects the enumeration
// index from the authoritative rows (an idempotent SET heal), so nothing
// can durably drift.

//go:build slatedb

package storage

import (
	"bytes"
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
// gRPC forwarding and joins the ring. The ShardKeyFn and the per-family
// co-location are unchanged across node counts: the same code shards correctly
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

	// RegisterGRPC is an OPTIONAL, OPAQUE hook the composition root passes
	// so other cluster-internal services can register on the SAME gRPC
	// server NewShaleRepo constructs and serves in multi-node mode (the
	// relay's peer fan-out service rides it - one advertised address per
	// pod, one listener lifecycle, reachable at exactly the address shale
	// forwarding already dials; see docs/SPEC.md "Multi-pod relay: the peer
	// transport"). Called once, with the server, after shale's own node
	// service is registered and BEFORE Serve starts. nil registers nothing
	// extra. Ignored in single-node mode (no server exists). The storage
	// package stays relay-agnostic: the hook's type is all it knows.
	RegisterGRPC func(*grpc.Server)
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

	// Retention is the content-TTL policy used to (re)stamp ExpiresAt on
	// update. Defaults to the 30-day policy; the composition root overrides it.
	Retention domain.Retention

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
	// via accessors so a peer can reference this node. nodeID is this
	// node's stable cluster identity (cfg.NodeID), used to exclude self
	// from the peer-address view PeerGRPCAddrs serves.
	grpcAddr string
	bindAddr string
	nodeID   string

	// grpcSrv + grpcLis are the peer-forwarding server and its listener,
	// set only in multi-node mode. nil single-node (the back-compat path
	// stands up neither). Close GracefulStops the server, which drains
	// in-flight RPCs and closes the listener.
	grpcSrv *grpc.Server
	grpcLis net.Listener

	// confirmWG tracks in-flight background confirm goroutines. The insert's
	// index write runs SYNCHRONOUSLY today (the entry is the quota's
	// accounting record - see InsertWithQuotaCheck), so nothing currently
	// enqueues here; the group (and WaitPendingConfirms) is retained because
	// Close joins on it and tests drain through it, keeping the seam if a
	// future write moves off the response path.
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

	// Test seams (nil in production; set only through the _test exports).
	// The reconciler's race windows are microseconds wide, so the tests
	// that pin the guarded index writes inject a concurrent operation at
	// the exact point the window opens.
	//
	// testHookReconcileBeforeIndexWrites runs after Reconcile has captured
	// ALL its snapshots (the enumeration-index snapshot and the
	// authoritative pastes/versions scans) and before the paste
	// reprojection's prune + write loops - the widest point of the
	// snapshot-to-write window a live refresh can race.
	testHookReconcileBeforeIndexWrites func()
	// testHookBeforeOrphanPruneDelete runs inside the orphan prune, after
	// the authoritative-row confirm and before the entry delete - the
	// TOCTOU window a same-slug delete-then-redeploy can race.
	testHookBeforeOrphanPruneDelete func(key []byte)
}

// WaitPendingConfirms blocks until every background confirm goroutine has
// finished. The insert's confirm step (writing the derived identity_pastes
// index entry + first-seen) runs SYNCHRONOUSLY on the upload path today -
// the entry is the quota's accounting record, so the owner's next check
// must see it - which makes this a no-op; it is retained as the drain seam
// tests and Close use, in case a future write moves off the response path.
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
// (<slug> / <id> / <subnet>), which is what makes an owner's enumeration
// index and the per-slug authoritative rows each single-shard.
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

	// Online resharding (declarative). Enable shale's config-driven reshard when
	// the cluster is multi-backend (a BackendFactory + UnitCount) AND a shared CAS
	// arbiter (ConditionalStore) is present - i.e. the homogeneous deployment where
	// every pod declares the same HOSTTHIS_SHALE_UNIT_COUNT. The cluster then DRIVES
	// the reshard target from the unanimously gossiped declared count, so changing
	// HOSTTHIS_SHALE_UNIT_COUNT to another power of two and redeploying triggers an
	// online, lossless split/merge to the new count (docs/SPEC.md "Online
	// resharding"). Off for single-backend (nothing to reshard) and when there is
	// no arbiter to coordinate the generation advance. Mirrors shale's shaled
	// runtime (pkg/shaled/runtime.go).
	clusterCfg.DeclarativeReshard = clusterCfg.BackendFactory != nil && clusterCfg.ConditionalStore != nil

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
		Retention:       domain.DefaultRetention(),
		kv:              kv,
		logger:          cfg.Logger,
		bindAddr:        cfg.BindAddr,
		grpcAddr:        advertiseGRPCAddr,
		nodeID:          cfg.NodeID,
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
		// Composition-root services (the relay's peer fan-out) register on
		// the SAME server via the opaque hook, before Serve. Storage stays
		// agnostic to what rides along.
		if cfg.RegisterGRPC != nil {
			cfg.RegisterGRPC(g)
		}
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

// PeerGRPCAddrs returns the CURRENT gRPC addresses of every OTHER live
// cluster member - self excluded - read from the ring membership the
// cluster gossips. This is the address each peer advertised at join
// (exactly the one shale forwarding dials), kept current by the same
// gossip that tracks joins, leaves, and deploy churn, so it is fresher
// than any DNS view. The composition root adapts this onto the relay's
// Peers port for the multi-pod peer fan-out (docs/SPEC.md "Multi-pod
// relay: peer discovery"); storage itself is relay-agnostic. Single-node
// mode returns an empty slice (the zero-peer degenerate case). Safe for
// concurrent use.
func (r *ShaleRepo) PeerGRPCAddrs() []string {
	var out []string
	for _, m := range r.cluster.Members() {
		if m.ID == r.nodeID || m.Addr == "" {
			continue
		}
		out = append(out, m.Addr)
	}
	return out
}

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

// PendingPasteTimeout is how old a status=pending paste may be before the
// reconciler ages it to failed (the pod-death backstop: its in-memory
// bytes never reached the blob store). It is comfortably longer than a
// healthy blob write (~250 ms) plus retries, so the reconciler never
// races a live finalizer, while short enough that a lost-bytes paste
// self-heals out of the loading screen within a sweep tick or two. A var,
// not a const, so tests can shrink it.
var PendingPasteTimeout = 2 * time.Minute

// --- JSON projections ------------------------------------------------------

// identityPasteRow is the value-bearing projection stored at
// identity_pastes/<id>/<slug>. Size caches the paste's LIVE byte sum (its
// non-deleted version sizes) and ExpiresAt its retention deadline: the
// quota scan sums exactly these two cached fields, one prefix scan with
// zero per-entry fan-out (docs/SPEC.md "Scan-derived quota"). The entry is
// derived (eventually consistent); every size-changing write path
// maintains it and the reconciler's reprojection rebuilds it from the
// authoritative pastes/* + versions/* rows, so cached-value error is
// bounded by a reconcile cycle.
type identityPasteRow struct {
	Name      string    `json:"name"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Placeholder marks a fail-closed entry the reconciler projects for a
	// slug whose authoritative record (head or any version row) cannot be
	// decoded: the live sum is uncomputable, so instead of projecting a
	// partial number (a silent under-count) the entry carries this marker
	// and the quota scan HARD-FAILS on it (docs/SPEC.md "Decode tolerance
	// of the quota scan"). Cleared by the next reprojection once the
	// record decodes again. omitempty keeps ordinary entries byte-shaped
	// as before the field existed.
	Placeholder bool `json:"placeholder,omitempty"`
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

// txGetCounter reads an integer counter value inside a CAS tx, recording the
// read-check. Absent reads as 0 (and an ExpectAbsent check). Used by the
// per-app room byte counter (the one counter the shale backend still keeps,
// because a room write is a strict single-shard CAS - see shale_room_repo.go);
// the per-identity paste/site quota is scan-derived, not counted.
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

// CountByOwner returns the owner's count of LIVE pastes. The
// identity_pastes index is derived + eventually consistent, so it must be
// repaired on read exactly like ListByOwner: an orphan entry (its
// authoritative pastes/<slug> row already deleted) is NOT a live paste and
// must not be counted. A raw len(idx) over-counts orphans - the entries
// whose delete left a stale index pointer - which is the whoami-shows-N /
// list-shows-fewer mismatch (#464). Resolving each entry against the
// {slug} row, counting only the live ones, and dropping the stale entries
// makes the count match ListByOwner (and reality), and self-heals the
// index the same way a list does.
func (r *ShaleRepo) CountByOwner(owner string) (int, error) {
	if owner == "" {
		return 0, nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	live := 0
	var staleKeys [][]byte
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var p pasteRow
		if gerr := r.getJSON(shaleKeyPaste(slug), &p); gerr != nil {
			if errors.Is(gerr, ErrNotFound) {
				staleKeys = append(staleKeys, append([]byte(nil), item.Key...))
				continue
			}
			return 0, gerr
		}
		live++
	}
	// Best-effort repair (same as ListByOwner): a failure is non-fatal -
	// the count is already correct; the entry is re-attempted next read.
	for _, k := range staleKeys {
		_ = r.cluster.Delete(k)
	}
	return live, nil
}

// SumActiveBytesByOwner derives the owner's active PASTE bytes from ONE
// single-shard prefix scan of the per-identity enumeration index
// (identity_pastes/<id>/), summing the cached (size, expires_at) each
// value-bearing entry carries - ZERO per-entry fan-out to the {slug}
// shards. The write paths keep the cached size equal to the paste's live
// (non-deleted) version sum and the reconciler's reprojection heals drift,
// so the figure matches the sqlite/slatedb authoritative sums up to a
// bounded, one-reconcile-cycle window (docs/SPEC.md "Scan-derived quota").
// now IS used: an expired-but-unswept paste self-excludes at read time via
// its cached expiry (ExpiryFreesQuotaAtReadTime = true), matching sqlite +
// slatedb.
func (r *ShaleRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	if owner == "" {
		return 0, nil
	}
	total, err := r.sumActiveBytesForOwner(owner, now)
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

// sumActiveBytesForOwner scans identity_pastes/<owner>/ once and sums the
// cached size of every live entry (cached expires_at > now). It never reads
// the authoritative {slug} rows (the entry IS the accounting record), with
// one upgrade-path exception: an empty-valued LEGACY entry (see the
// fail-closed note below) is read through its authoritative row. The
// consequences of trusting the cache are deliberate and documented
// (docs/SPEC.md "Scan-derived quota"):
//
//   - a stale entry whose paste is GONE keeps counting its cached bytes
//     until the reconciler prunes it (bounded over-count; can only wrongly
//     reject, never admit an over-cap write),
//   - an entry whose paste died of EXPIRY self-excludes (cached expires_at
//     is past),
//   - failed pastes are absent by construction (MarkFailed drops the entry;
//     the reconciler never reprojects a failed row's entry).
//
// Fail-closed (Policy 3, this is a synchronous write-path read): an entry
// that does not decode, or that carries the reconciler's fail-closed
// Placeholder marker (an undecodable authoritative record), HARD-FAILS the
// scan - rejecting the upload rather than silently under-counting. The one
// deliberate exception is the upgrade path: a LEGACY entry recognized by
// shape (an EMPTY value, the slatedb layout's bare-marker convention that
// an in-place migration carries over) is read through its authoritative
// pastes/<slug> row plus live version sum - the slatedb per-entry
// semantics - until the reconciler's reprojection enriches it. Without the
// fallback an empty migrated entry would hard-fail every quota-checked
// create for that owner until the first post-cutover reconcile.
func (r *ShaleRepo) sumActiveBytesForOwner(owner string, now time.Time) (int64, error) {
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		if len(item.Value) == 0 {
			n, err := r.legacyPasteEntryBytes(item.Key, now)
			if err != nil {
				return 0, err
			}
			total += n
			continue
		}
		var row identityPasteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if row.Placeholder {
			return 0, fmt.Errorf("quota scan: %s is a fail-closed placeholder (authoritative record undecodable; the reconciler clears it once the record is repaired)", item.Key)
		}
		if !row.ExpiresAt.After(now) {
			continue // expired (or a stale entry whose cached expiry passed): self-excludes
		}
		total += int64(row.Size)
	}
	return total, nil
}

// legacyPasteEntryBytes resolves a LEGACY (empty-valued) identity_pastes
// entry against its authoritative rows: the read-through path a migrated
// slatedb deployment needs until the reconciler enriches the entry (the
// paste mirror of legacySiteEntryBytes; pastes have no marker-byte legacy
// era, so the empty value is the only recognized paste shape). A stale
// legacy entry (row gone) contributes zero (the reconciler prunes it); an
// undecodable row HARD-FAILS (Policy 3); a live row contributes its live
// version sum under the same read-time expiry filter, exactly what the
// slatedb sum computed for the entry.
func (r *ShaleRepo) legacyPasteEntryBytes(indexKey []byte, now time.Time) (int64, error) {
	slug := domain.Slug(extractSlug(indexKey))
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil // stale legacy entry; the reconciler prunes it
		}
		return 0, err
	}
	if !p.ExpiresAt.After(now) {
		return 0, nil // expired-unswept: stops counting at read time
	}
	return r.sumLiveVersionBytes(slug)
}

// sumLiveVersionBytes sums the sizes of a paste's non-deleted version rows on
// the {slug} shard (the paste's post-revival byte contribution). When
// AppendVersionWithQuotaCheck revives an EXPIRED-unswept paste - the scan
// excludes it, but the append resets its expiry - the check charges this sum
// PLUS the new version so the revived paste's full bytes are counted, matching
// the sqlite/slatedb append revival charge (docs/SPEC.md "Reviving an
// expired-but-unswept record charges its FULL post-revival size").
func (r *ShaleRepo) sumLiveVersionBytes(slug domain.Slug) (int64, error) {
	versions, err := r.scanVersions(slug)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, v := range versions {
		if v.Deleted {
			continue
		}
		total += int64(v.Size)
	}
	return total, nil
}

// combinedActiveBytes sums the owner's active PASTE + SITE bytes via the two
// scan-based owner sums. It is the per-owner "used" figure the quota checks
// compare against the cap before an authoritative write, matching the sqlite
// identityActiveBytes which spans both kinds. Both sums read the CACHED
// enumeration-entry values (two single-shard scans, no per-entry fan-out);
// the write paths + the reconciler keep the cache converged with the
// authoritative rows (docs/SPEC.md "Scan-derived quota").
func (r *ShaleRepo) combinedActiveBytes(owner string, now time.Time) (int64, error) {
	pasteBytes, err := r.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return 0, err
	}
	siteBytes, err := r.SumActiveSiteBytesByOwner(owner, now)
	if err != nil {
		return 0, err
	}
	return int64(pasteBytes) + siteBytes, nil
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

// InsertWithQuotaCheck creates a paste. The per-owner cap is enforced by a
// scan-and-compare BEFORE the authoritative write: the owner's combined
// paste+site used bytes are derived from the enumeration indexes and the
// upload is rejected with ErrOverUserQuota if used+body would exceed the cap.
// The check and the write are NOT atomic, so two concurrent uploads from the
// SAME identity can both pass and both land (a bounded over-admit; acceptable
// because one key is one person and the object-store bucket quota backstops
// the durable total - docs/SPEC.md "Scan-derived quota"). The
// durable total-bytes ceiling is likewise the object-store bucket quota,
// enforced when the blob Put is rejected.
//
// The authoritative write is the {slug} CAS (slug-collision read-check + blob
// bind); the {id} enumeration-index entry + first-seen are written
// SYNCHRONOUSLY after it (not deferred) so a subsequent quota scan sees this
// paste. The derived index is the quota source of truth now, so it cannot lag
// the way the old reservation-counter's deferred index could; a crash between
// the {slug} write and the {id} index write leaves a paste the index does not
// list (a transient, bounded under-count the reconciler's reprojection heals).
func (r *ShaleRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	identity := p.Identity.String()
	body := int64(p.Size)

	// The staged blob refs (if any) ride this call's context, isolated from
	// any concurrent same-slug write. Read once here and pass them down so the
	// authoritative {slug} transaction binds exactly this call's blobs.
	binds := pendingBindsFromContext(ctx)

	// Quota CHECK (scan-based) before the authoritative write.
	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if used+body > userCap {
			return ErrOverUserQuota
		}
	}

	// Authoritative write on the {slug} shard.
	if err := r.insertAuthoritative(p, binds); err != nil {
		return err
	}

	// Enumeration-index maintenance on the {id} shard: write the
	// identity_pastes index entry + first-seen. Synchronous (the entry is
	// the quota's accounting record - the owner's next check must see this
	// paste) but best-effort + reconciler-healed: a failure leaves a paste
	// the index does not list (a transient under-count the reconciler
	// heals), never a failed upload, so the paste (already durable) is
	// returned as success.
	if err := r.confirmInsert(p); err != nil {
		r.repoLog().Printf("shale: index maintenance for %s: %v (index lag; reconciler will heal)", p.Slug, err)
	}
	return nil
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

// confirmInsert writes the value-bearing identity_pastes index entry and sets
// identity_first_seen if absent, on the {id} shard in one CAS. It is the
// enumeration-index maintenance the scan-based quota depends on: the entry is
// what SumActiveBytesByOwner SUMS (the cached size seeds at v1's size, the
// paste's whole live sum at insert) and how ListByOwner / CountByOwner
// enumerate the owner's pastes. Idempotent: a re-run overwrites the same
// entry and leaves an already-set first-seen untouched.
func (r *ShaleRepo) confirmInsert(p domain.Paste) error {
	identity := p.Identity.String()
	slug := p.Slug.String()
	indexKey := shaleKeyIdentityPaste(identity, slug)
	firstSeenKey := shaleKeyIdentityFirstSeen(identity)
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
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

// MarkFailed flips a paste's status pending -> failed and drops its
// enumeration-index entry, the shape both the background finalizer's failure
// transition AND the reconciler's age-out use. The paste ROW stays (flipped to
// failed) so a read can serve an error page; its bytes stop counting toward
// quota the instant the status flips (the scan skips a failed head row) - there
// is no counter to decrement. Guarded: only a still-pending paste transitions,
// so this never un-counts a ready paste. Idempotent on re-call. See docs/SPEC.md
// "Paste lifecycle status (async blob write)".
func (r *ShaleRepo) MarkFailed(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	// Step 1: flip the {slug}-shard status pending -> failed. Capture the
	// owner + size so step 2 can shed the bytes. If the paste isn't pending
	// (already failed/ready, or absent) there's nothing to release.
	var identity string
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
	// Step 2: drop the enumeration-index entry on the {id} shard so the
	// failed paste leaves ListByOwner. Its bytes stop counting toward quota
	// the instant step 1 flips the status: the scan skips a failed head row
	// (and, once the index entry is gone, never enumerates it at all), so
	// there is no counter to decrement.
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			return tx.Delete(indexKey)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return nil
	})
}

// AppendVersionWithQuotaCheck appends a new version. The per-owner cap is
// enforced by a scan-and-compare BEFORE the authoritative write: the owner's
// combined paste+site used bytes (which already include this paste's current
// versions WHEN it is live) plus the new version's bytes must not exceed the
// cap. If the target paste is EXPIRED-unswept the scan excludes it entirely,
// but the append revives it (resets expiry), so the check charges the paste's
// full post-revival total - its existing non-deleted version bytes plus the new
// version - not the new version alone (docs/SPEC.md "Reviving an
// expired-but-unswept record charges its FULL post-revival size"). The check
// and the write are not atomic (bounded same-owner over-admit), the same
// tradeoff InsertWithQuotaCheck documents. The version row is then written +
// the expiry clock reset on the {slug} shard, and the index projection's
// cached size + expiry are refreshed synchronously on the {id} shard (the
// quota scan sums the cached size, so the new version's bytes start
// counting the moment the refresh lands; a lost refresh is healed by the
// reconciler's reprojection).
func (r *ShaleRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	// The new version's staged blob ref (if any) rides this call's context,
	// isolated from any concurrent same-slug append. Read once and pass it down.
	binds := pendingBindsFromContext(ctx)

	// Resolve the owner identity from the authoritative paste.
	var existing pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	identity := existing.Identity
	body := int64(size)

	// Quota CHECK (scan-based) before the authoritative write.
	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return AppendResult{}, err
		}
		// The scan `used` already counts this paste's live versions WHEN the
		// paste is live, so the vanilla charge is just body. But when the target
		// paste is EXPIRED-unswept the scan excludes the WHOLE paste, while
		// appendAuthoritative RESETS its expiry (revives it) and brings its
		// existing live versions back into the sum. So a revived paste must be
		// charged its full post-revival total: its existing non-deleted version
		// bytes (currently excluded from `used`) PLUS the new version, not the
		// new version alone - otherwise the revived paste durably exceeds the cap
		// (docs/SPEC.md "Reviving an expired-but-unswept record charges its FULL
		// post-revival size"). Matches the sqlite + slatedb append revival charge.
		charge := body
		if !existing.ExpiresAt.After(now) {
			revived, err := r.sumLiveVersionBytes(slug)
			if err != nil {
				return AppendResult{}, err
			}
			charge += revived
		}
		if used+charge > userCap {
			return AppendResult{}, ErrOverUserQuota
		}
	}

	// Determine the next version number from a scan (outside the tx). The
	// authoritative tx re-reads the candidate version key as ExpectAbsent,
	// so a racing append that took the same number conflicts + retries.
	res, err := r.appendAuthoritative(slug, kind, contentSHA, size, now, binds)
	if err != nil {
		return AppendResult{}, err
	}

	// Refresh the index projection: the cached live-byte size (which the
	// quota scan sums - the new version's bytes must start counting) and the
	// reset expiry (the retention clock). Best-effort + reconciler-healed (a
	// lost refresh leaves a stale cached size/expiry until the next
	// reprojection - bounded drift, docs/SPEC.md "Scan-derived quota") and
	// GUARDED (a concurrent same-slug write whose refresh landed first wins;
	// this one skips rather than clobbering it with an older sum) - never a
	// failed append.
	if err := r.refreshIndexProjection(identity, slug, r.Retention.ExpiryFor(now)); err != nil {
		r.repoLog().Printf("shale: index refresh for append %s: %v (index lag; reconciler will heal)", slug, err)
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
	newExpiry := r.Retention.ExpiryFor(now)
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

// refreshIndexProjection updates the owner's identity_pastes projection after
// a size-changing {slug} write: it recomputes the paste's live byte sum from
// the authoritative version rows and sets the cached size (what the quota
// scan sums) and, when newExpiry is non-zero, the cached expiry (an append
// resets the retention clock; a version tombstone does not touch it). One
// {id}-shard CAS, GUARDED: the entry's payload is captured BEFORE the
// recompute and the write commits only if the entry still holds it, so two
// concurrent same-slug refreshes cannot land older-sum-last - the one whose
// entry moved underneath it SKIPS (logged; at most one cycle of staleness,
// which the reconciler's reprojection converges) instead of clobbering the
// fresher sum. No recompute retry on the response path (docs/SPEC.md
// "Scan-derived quota" / "Window C"). A missing entry is left missing - the
// reconciler's reprojection rebuilds it with the same values - matching the
// insert-side best-effort contract; a LEGACY empty entry is likewise left
// for the reconciler's enrichment (the quota scan reads it through the
// authoritative rows meanwhile, so it never goes stale). Any Placeholder
// marker is cleared only via the reconciler (a placeholder row has no
// trustworthy fields to preserve, so it is left rather than part-patched).
func (r *ShaleRepo) refreshIndexProjection(identity string, slug domain.Slug, newExpiry time.Time) error {
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	expected, err := r.getRaw(indexKey)
	if err != nil {
		return err
	}
	if len(expected) == 0 {
		return nil // absent (reconciler rebuilds) or legacy-empty (reconciler enriches)
	}
	var row identityPasteRow
	if err := json.Unmarshal(expected, &row); err != nil {
		return fmt.Errorf("decode %s: %w", indexKey, err)
	}
	if row.Placeholder {
		return nil // fail-closed placeholder: only the reconciler replaces it
	}
	live, err := r.sumLiveVersionBytes(slug)
	if err != nil {
		return err
	}
	row.Size = int(live)
	if !newExpiry.IsZero() {
		row.ExpiresAt = newExpiry
	}
	written, err := r.guardedPutIndexEntry(indexKey, expected, true, row)
	if err != nil {
		return err
	}
	if !written {
		r.repoLog().Printf("shale: index refresh %s skipped: entry changed during the recompute (a concurrent write landed; reconciler converges)", indexKey)
	}
	return nil
}

// Delete removes a paste entirely (whole-paste delete is a full removal, not
// a tombstone): the authoritative {slug} rows go away and the {id}
// enumeration-index entry is dropped, so the owner's scan-derived quota sum
// stops counting the paste. Idempotent on a missing paste. There is no byte
// counter to decrement: the freed bytes leave the owner's sum the instant the
// authoritative rows (and their index entry) vanish.
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

	pasteKey := shaleKeyPaste(slug)
	// On the transactional shale-blob path the paste's blobs are unbound in the
	// SAME {slug} transaction (atomic delete): each version's pointer is removed
	// via unbind, so the bytes go unreferenced exactly when the rows vanish, and
	// SweepOrphans reclaims them after the grace. unbind is a no-op on the
	// metadata-only path (the global content-addressed sweep reclaims).
	delBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		if err := tx.Delete(pasteKey); err != nil {
			return err
		}
		for _, v := range versions {
			// Unbind this version's blob (the bref pointer) so its bytes go
			// unreferenced. A tombstoned version's pointer was already unbound
			// by DeleteVersion, so re-unbinding (an idempotent tx.Delete of a
			// missing key) is harmless. Only the BlobID-carrying rows (the
			// shale-blob path) have a pointer; legacy rows carry no BlobID and
			// unbind is skipped.
			if v.BlobID != "" {
				if err := unbind(v.BlobID); err != nil {
					return err
				}
			}
			if err := tx.Delete(shaleKeyVersion(slug, v.VerNum)); err != nil {
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

	// Drop the enumeration-index entry on the {id} shard so the paste leaves
	// the owner's scan (and ListByOwner / CountByOwner). Idempotent.
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			return tx.Delete(indexKey)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return nil
	})
}

// DeleteVersion tombstones a single version (Q1 = Option 2: the version
// stays visible in the list flagged deleted, but its content blob is no
// longer referenced so the GC reclaims it). The tombstoned version's bytes
// leave the owner's scan-derived quota sum via the index-projection refresh
// after the tombstone commits (the quota scan sums the cached size; a lost
// refresh is a bounded stale-cache window the reconciler's reprojection
// heals); there is no byte counter to decrement. A re-delete of an
// already-tombstoned version is a repo-level no-op.
func (r *ShaleRepo) DeleteVersion(slug domain.Slug, ver int) error {
	// Existence gate: a missing paste yields ErrNotFound, matching the sqlite
	// + slatedb DeleteVersion which key off the paste row.
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		return err
	}
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

	verBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		var v versionRow
		if err := shaleTxGetJSON(tx, verKey, &v); err != nil {
			return err
		}
		if v.Deleted {
			return nil // already tombstoned; no-op
		}
		v.Deleted = true
		if err := shaleTxPutJSON(tx, verKey, v); err != nil {
			return err
		}
		if v.BlobID != "" {
			return unbind(v.BlobID)
		}
		return nil
	}
	var txErr error
	if r.kv != nil {
		txErr = r.kv.Transact(verKey, func(tx *cluster.BlobTx) error {
			return verBody(tx, func(blobID string) error {
				return tx.UnbindBlob(r.blobRefFor(pasteKey, blobID))
			})
		})
	} else {
		txErr = r.cluster.Transact(verKey, func(tx backend.Transaction) error {
			return verBody(tx, func(string) error { return nil })
		})
	}
	if txErr != nil {
		return txErr
	}
	// Shed the tombstoned version's bytes from the owner's cached projection:
	// recompute the paste's live sum from the authoritative version rows and
	// refresh the index entry (expiry untouched - a tombstone does not reset
	// the retention clock). Best-effort + reconciler-healed (a lost refresh
	// leaves the cached size too LARGE - a bounded over-count that can only
	// over-reject - until the next reprojection) and GUARDED against a
	// concurrent same-slug refresh landing older-sum-last. Idempotent on a
	// re-delete no-op (the recompute lands the same value).
	if err := r.refreshIndexProjection(p.Identity, slug, time.Time{}); err != nil {
		r.repoLog().Printf("shale: index refresh for tombstone %s/%d: %v (index lag; reconciler will heal)", slug, ver, err)
	}
	return nil
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

// ExpiredPastes fans out across all {slug} shards (cluster.Aggregate) over
// the expiry/* index and returns one reference per entry whose expiry
// timestamp is <= now: the slug plus the entry's full key as the opaque
// IndexRef, so DeleteExpired can remove the EXACT entry that surfaced the
// slug even when the paste record is already gone. The timestamp is the
// middle segment of expiry/<rfc3339>/<slug>; RFC3339Nano sorts
// lexicographically so a string compare is correct at whole-second
// granularity. NOTE: RFC3339Nano is variable-width (a fractional ".5Z"
// sorts before a bare "Z" within one whole second), so this paste index
// carries the same latent sub-second skew documented for the paste expiry
// path; fixing it is a key-format migration left as a follow-up. The site
// expiry index uses a fixed-width format (expirySiteTimeFormat) and has no
// such skew.
func (r *ShaleRepo) ExpiredPastes(now time.Time) ([]domain.ExpiredPaste, error) {
	items, err := r.aggregatePrefix([]byte("expiry/"))
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(time.RFC3339Nano)
	var out []domain.ExpiredPaste
	for _, item := range items {
		k := string(item.Key)
		rest := strings.TrimPrefix(k, "expiry/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		ts := rest[:idx]
		slug := rest[idx+1:]
		if ts <= cutoff {
			out = append(out, domain.ExpiredPaste{Slug: domain.Slug(slug), IndexRef: k})
		}
	}
	return out, nil
}

// DeleteExpired processes one expired reference: the same full-cascade
// delete as Delete when the paste record still exists, and then - in
// every case - removal of the exact expiry-index entry the scan surfaced
// (a single {slug}-shard CAS: the entry's shard key is its trailing slug,
// the same shard the paste rows live on). The unconditional entry removal
// is what keeps an orphaned entry (paste already gone, e.g. legacy
// TTL-era state) from resurfacing on every scan forever; it also
// self-heals an entry whose key drifted from the paste row's ExpiresAt
// (the cascade removes the DERIVED key; this removes the OBSERVED one).
// Idempotent: a missing record and a missing entry are both no-ops.
// Returns whether a paste record was actually deleted. See docs/SPEC.md
// "The storage contract" (Expiry).
func (r *ShaleRepo) DeleteExpired(ref domain.ExpiredPaste) (bool, error) {
	entryKey, err := expiryIndexKey(ref)
	if err != nil {
		return false, err
	}
	deleted := false
	var p pasteRow
	switch err := r.getJSON(shaleKeyPaste(ref.Slug), &p); {
	case errors.Is(err, ErrNotFound):
		// Orphaned entry: nothing to cascade, just clean the entry below.
	case err != nil:
		return false, err
	default:
		if err := r.Delete(ref.Slug); err != nil {
			return false, err
		}
		deleted = true
	}
	if entryKey != nil {
		if err := r.cluster.Transact(entryKey, func(tx backend.Transaction) error {
			return tx.Delete(entryKey)
		}); err != nil {
			return deleted, fmt.Errorf("delete expiry entry %s: %w", entryKey, err)
		}
	}
	return deleted, nil
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

// Reconcile is the metadata backend's maintenance pass and the SOLE
// quota-healing mechanism now that the per-identity quota is DERIVED by
// summing the cached values of the enumeration indexes (docs/SPEC.md
// "Scan-derived quota" / "Derived indexes and repair-on-read"). With no
// stored counter to keep correct, it has exactly two jobs, and NEITHER ever
// writes an aggregate number:
//
//   - REPROJECT the per-owner enumeration indexes from the authoritative
//     rows: identity_pastes/<id>/<slug> from the pastes/ + versions/ scans
//     (reconcileIndexes) and identity_sites/<id>/<slug> from the sites/ scan
//     (reconcileSiteIndexPass). Each adds a missing entry, rebuilds every
//     projection's CACHED QUOTA VALUES (a paste entry's size is the live
//     version sum from the versions/ scan; a site entry's is the row's
//     deduped size), and drops an entry whose paste/site is gone or failed -
//     pruning against a full aggregate of the index family so an orphan
//     lingers nowhere, not even under an owner with no remaining rows. This
//     closes BOTH quota-relevant gaps the cached-sum design has: a crash
//     between the authoritative {slug} row write and the {id} index write
//     (a live row the index does not list, transiently UNDER-counting the
//     owner) and a stale/orphaned cached value (a lost size refresh or a
//     lost entry drop, transiently mis-counting by that record's bytes).
//   - AGE OUT stuck pending pastes (the pod-death backstop): a status=pending
//     paste older than PendingPasteTimeout is flipped to failed so it drops
//     out of the quota scan (MarkFailed drops its enumeration entry) and its
//     loading screen.
//
// Because it only ever reprojects the authoritative rows (a SET plus
// per-record cached values, each a pure function of that record's rows),
// never a stored aggregate, it is idempotently self-healing and safe under
// live traffic on any cadence: every pod running it converges to the same
// state and running it more often only tightens the drift windows. It is
// NOT part of the SweepRepo contract: the sweep's public surface is
// unchanged. Single-node, cross-shard via aggregate; a poisoned row is
// skipped + logged and the pass continues (docs/SPEC.md "Decode tolerance
// is per-scan-semantics", Policy 1) - but never silently dropped from the
// projection (see the fail-closed placeholder below).
func (r *ShaleRepo) Reconcile(now time.Time) error {
	// Snapshot the enumeration index FIRST - strictly before the
	// authoritative scans - because it is the guard baseline for every
	// reprojection write below: a write commits only if its entry still
	// holds this snapshot's value. Snapshotting the index first is what
	// makes a guard pass MEAN something: the value this pass computes comes
	// from authoritative rows read AFTER the baseline, so "entry unchanged
	// since the baseline" proves the computed value is at least as fresh as
	// the entry - while any live refresh that lands mid-pass changes the
	// entry and flips the guard to skip. (Baselined the other way around, a
	// refresh landing between the authoritative scan and a later index
	// snapshot would be adopted as the baseline and the stale computed value
	// would sail through the guard - the exact clobber the guard exists to
	// stop.)
	pasteIdx, err := r.aggregatePrefix(prefixIdentityPastesAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan identity_pastes: %w", err)
	}

	// Gather authoritative paste state across all shards.
	pasteItems, err := r.aggregatePrefix([]byte("pastes/"))
	if err != nil {
		return fmt.Errorf("reconcile: scan pastes: %w", err)
	}

	// Gather the version rows too: the reprojected entry caches the paste's
	// LIVE byte sum (what the quota scan sums), which lives in the version
	// rows, not the head. A slug with an undecodable version row cannot have
	// its live sum computed - projecting a PARTIAL sum would silently
	// under-count - so it is marked and projected as a fail-closed
	// placeholder below (Policy 1 for the pass, Policy 3 for the value).
	verItems, err := r.aggregatePrefix(prefixVersionsAll)
	if err != nil {
		return fmt.Errorf("reconcile: scan versions: %w", err)
	}
	liveBytes := make(map[string]int64)
	poisonedVersions := make(map[string]bool)
	for _, item := range verItems {
		rest := strings.TrimPrefix(string(item.Key), string(prefixVersionsAll))
		slug, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			poisonedVersions[slug] = true
			r.repoLog().Printf("reconcile: undecodable version %s: projecting a fail-closed placeholder for %s: %v", item.Key, slug, err)
			continue
		}
		if v.Deleted {
			continue
		}
		liveBytes[slug] += int64(v.Size)
	}

	pastesByOwner := make(map[string]map[string]identityPasteRow)
	// stalePending collects slugs whose status is pending and whose
	// created_at is older than the pending timeout: the pod-death backstop
	// (the in-memory bytes are gone, no finalizer will ever run). They are
	// aged to failed AFTER the scan + index rebuild so a too-old pending's
	// index entry does not get re-projected by reconcileIndexes on the same
	// pass. See docs/SPEC.md "Reconciler: age out stuck pendings".
	var stalePending []domain.Slug
	for _, item := range pasteItems {
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// Idempotent reconcile: one poisoned paste must not stall the whole
			// pass (which would freeze index reprojection for every healthy
			// owner); the next tick retries it (Policy 1). But we CANNOT simply
			// drop it: the quota scan reads THROUGH the enumeration index, so an
			// undecodable row whose identity_pastes entry was ALSO lost (a crash
			// between the row write and the index write) would be enumerated by
			// nothing and silently drop from the owner's sum - a durable
			// UNDER-count that lets the owner exceed the cap permanently. So
			// derive the owner decode-independently from slug_owner/<slug> and
			// still project a PLACEHOLDER enumeration entry (zero-value row) so
			// the next quota scan enumerates the slug and re-reads the
			// authoritative row - which then hard-fails the scan (fail-closed
			// reject), never a silent under-count. See docs/SPEC.md "Decode
			// tolerance of the quota scan".
			owner := r.ownerOfSlug(domain.Slug(slug))
			if owner == "" {
				// No slug_owner to derive the owner (only reachable on the
				// metadata-only test path; prod always writes slug_owner). Cannot
				// project the entry; log loudly - this is the one residual where
				// the row can stay un-enumerated until it is repaired.
				r.repoLog().Printf("reconcile: undecodable paste %s AND no slug_owner: cannot project enumeration entry; quota may under-count this slug until it is repaired: %v", item.Key, err)
				continue
			}
			if pastesByOwner[owner] == nil {
				pastesByOwner[owner] = make(map[string]identityPasteRow)
			}
			// Fail-closed placeholder: we cannot read name/size/expiry from the
			// corrupt row, and the quota scan sums the CACHED size, so a
			// zero-value entry would silently under-count. The placeholder
			// marker makes the owner's next quota scan HARD-FAIL instead
			// (docs/SPEC.md "Decode tolerance of the quota scan").
			if _, ok := pastesByOwner[owner][slug]; !ok {
				pastesByOwner[owner][slug] = identityPasteRow{Placeholder: true}
			}
			r.repoLog().Printf("reconcile: undecodable paste %s: projected fail-closed placeholder enumeration entry under owner %s: %v", item.Key, owner, err)
			continue
		}
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusFailed {
			// A failed paste is NOT enumerated: MarkFailed drops its entry, and
			// the quota scan sums whatever the index lists, so reprojecting it
			// would resurrect its bytes (and its ListByOwner presence). A
			// leftover entry (crash mid-MarkFailed) is pruned by
			// reconcileIndexes' orphan pass instead.
			continue
		}
		if pastesByOwner[p.Identity] == nil {
			pastesByOwner[p.Identity] = make(map[string]identityPasteRow)
		}
		if poisonedVersions[slug] {
			// The live sum is uncomputable (an undecodable version row): project
			// the fail-closed placeholder rather than a partial number.
			pastesByOwner[p.Identity][slug] = identityPasteRow{Placeholder: true}
		} else {
			pastesByOwner[p.Identity][slug] = identityPasteRow{
				Name:      p.Name,
				Size:      int(liveBytes[slug]),
				CreatedAt: p.CreatedAt,
				ExpiresAt: p.ExpiresAt,
			}
		}
		// Age-out check: a pending paste older than the timeout is a
		// pod-death casualty (its in-memory bytes never reached the blob
		// store). Defer the transition until after the index rebuild.
		if domain.NormalizeStatus(p.Status) == domain.PasteStatusPending &&
			now.Sub(p.CreatedAt) > PendingPasteTimeout {
			stalePending = append(stalePending, domain.Slug(slug))
		}
	}

	if r.testHookReconcileBeforeIndexWrites != nil {
		r.testHookReconcileBeforeIndexWrites()
	}

	// Job 1: reproject the paste enumeration index from the authoritative rows.
	if err := r.reconcileIndexes(pastesByOwner, pasteIdx); err != nil {
		return err
	}
	// Job 1b: age out stuck pending pastes (pod-death backstop). Each
	// MarkFailed is independent + idempotent; a failure on one slug is
	// logged and skipped so it cannot stall the rest of the pass (the next
	// tick retries it), the same per-row discipline the index reprojection uses.
	for _, slug := range stalePending {
		if err := r.MarkFailed(slug); err != nil {
			r.repoLog().Printf("reconcile: age-out pending %s: %v", slug, err)
		}
	}
	// Job 2: reproject the SITE enumeration index from the authoritative sites/
	// rows (the exact parallel of Job 1, re-homed here from the now-deleted
	// site-reservation pass so the site index still heals every tick). This is
	// what backfills sites deployed before the identity_sites index existed and
	// closes the crash-between-row-and-index under-count for sites.
	return r.reconcileSiteIndexPass()
}

// reconcileIndexes rebuilds identity_pastes projections to match the
// authoritative paste set per owner: adds missing entries, refreshes every
// projection's cached values, and drops entries with no live authoritative
// paste. have is the pass's index snapshot, captured by Reconcile BEFORE
// the authoritative scans (see the guard-baseline reasoning there); it
// serves double duty as the orphan-prune candidate set and as the
// expected-value baseline for the guarded writes. The orphan prune runs
// against that FULL aggregate of the index family - not just the owners
// with authoritative rows - so an entry lingering under an owner whose
// pastes are ALL gone is still dropped (the quota scan sums cached values
// without resolving the head rows, so an unpruned orphan would over-count
// that owner forever, not just until a list repaired it).
func (r *ShaleRepo) reconcileIndexes(pastesByOwner map[string]map[string]identityPasteRow, have []scanItem) error {
	snapshot := make(map[string][]byte, len(have))
	for _, item := range have {
		snapshot[string(item.Key)] = item.Value
	}
	for _, item := range have {
		owner, slug, ok := splitIdentityIndexKey(item.Key, prefixIdentityPastesAll)
		if !ok {
			continue
		}
		if _, wanted := pastesByOwner[owner][slug]; wanted {
			continue
		}
		// Not in the reprojection set: an orphan (the authoritative paste is
		// gone), a failed paste's leftover entry (crash mid-MarkFailed), or a
		// FRESH insert that raced this pass's pastes/ snapshot. Confirm
		// against the authoritative row before dropping so the racing insert
		// is kept - and drop via the VALUE-COMPARED delete, because the
		// confirm and the delete are not one step: a same-slug
		// delete-then-redeploy can land a fresh row + entry between the
		// confirm's NotFound and the delete, and the fresh entry must survive
		// (docs/SPEC.md, the reconciler's prune).
		var p pasteRow
		switch gerr := r.getJSON(shaleKeyPaste(domain.Slug(slug)), &p); {
		case errors.Is(gerr, ErrNotFound):
			// Orphan: the paste is gone. Best-effort, guarded drop.
			if _, err := r.guardedDeleteIndexEntry(item.Key, item.Value); err != nil {
				r.repoLog().Printf("reconcile: prune %s: %v (next pass retries)", item.Key, err)
			}
		case gerr != nil:
			// Undecodable row: keep the entry; the fail-closed placeholder
			// projection above handles it (or already did via slug_owner).
		case domain.NormalizeStatus(p.Status) == domain.PasteStatusFailed:
			// Failed pastes are not enumerated. Same guarded drop.
			if _, err := r.guardedDeleteIndexEntry(item.Key, item.Value); err != nil {
				r.repoLog().Printf("reconcile: prune %s: %v (next pass retries)", item.Key, err)
			}
		default:
			// Live row that raced the snapshot: keep; the next pass reprojects it.
		}
	}
	for owner, want := range pastesByOwner {
		for slug, row := range want {
			// Refresh every wanted entry (idempotent; covers add + update).
			// Guarded on the pass's snapshot value: a live write that landed
			// mid-pass (its cached sum is fresher than this pass's
			// point-in-time computation) flips the guard and the write skips
			// - the next pass recomputes from fresher rows.
			key := shaleKeyIdentityPaste(owner, slug)
			expected, present := snapshot[string(key)]
			written, err := r.guardedPutIndexEntry(key, expected, present, row)
			if err != nil {
				return err
			}
			if !written {
				r.repoLog().Printf("reconcile: index write %s skipped: entry changed since the pass snapshot (a live write landed; next pass reprojects)", key)
			}
		}
	}
	return nil
}

// splitIdentityIndexKey splits an enumeration-index key
// (<prefix><owner>/<slug>) into its owner + slug. The slug is the LAST
// segment (slugs are slash-free; an owner identity may itself contain '/',
// e.g. a base64 key fingerprint, so the last '/' is the only safe split).
func splitIdentityIndexKey(key, prefix []byte) (owner, slug string, ok bool) {
	rest := string(key[len(prefix):])
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// errIndexEntryChanged is the internal sentinel a guarded index write's
// closure aborts with when the entry no longer holds the value the writing
// computation started from (a fresher write landed in between). Transact
// returns a non-conflict fn error verbatim, so guardedPutIndexEntry maps it
// to "skipped". It never escapes guardedPutIndexEntry.
var errIndexEntryChanged = errors.New("shale: index entry changed since the computation read it")

// guardedPutIndexEntry writes row (JSON) at key ONLY IF the entry still
// holds expected - the exact payload the writing computation read when it
// started (present=false: the entry must still be absent). This is the
// conditional primitive that keeps every recomputed cached-value write (the
// reconciler's reprojections, the append/tombstone refreshes) from
// clobbering a FRESHER value a concurrent write landed after the
// computation's snapshot: on a mismatch it skips, returning (false, nil) -
// guarded to lose, never to clobber; the next cycle/refresh recomputes from
// fresher rows (docs/SPEC.md "Periodic reconcile" / "Window C"). The
// compare runs inside the {id}-shard CAS with the entry in the read-set, so
// a write landing between the compare and the commit conflicts, and the
// re-run closure re-compares against the new value and skips.
func (r *ShaleRepo) guardedPutIndexEntry(key, expected []byte, present bool, row any) (bool, error) {
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := tx.Get(key) // records the read-check
		switch {
		case errors.Is(gerr, backend.ErrNotFound):
			if present {
				return errIndexEntryChanged // entry deleted since the snapshot
			}
		case gerr != nil:
			return gerr
		default:
			if !present {
				return errIndexEntryChanged // entry appeared since the snapshot
			}
			payload, serr := stripEnvelope(cur)
			if serr != nil {
				return serr
			}
			if !bytes.Equal(payload, expected) {
				return errIndexEntryChanged
			}
		}
		return shaleTxPutJSON(tx, key, row)
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// guardedDeleteIndexEntry drops the enumeration entry at key ONLY IF it
// still holds expected - the payload the pass's snapshot read. It is the
// delete sibling of guardedPutIndexEntry, closing the orphan prune's
// TOCTOU: the prune confirms the authoritative row is gone/failed and THEN
// deletes, and a same-slug delete-then-redeploy landing its fresh row +
// entry in that window must not have the FRESH entry dropped. A changed
// value skips, returning (false, nil) - the redeploy's entry survives, and
// the stale entry (if the skip kept one) lingers at most one more pass; an
// already-gone entry is an idempotent no-op (docs/SPEC.md, the
// reconciler's prune). The compare runs inside the {id}-shard CAS with the
// entry in the read-set, so a write landing between the compare and the
// commit conflicts and the re-run closure re-compares and skips.
func (r *ShaleRepo) guardedDeleteIndexEntry(key, expected []byte) (bool, error) {
	if r.testHookBeforeOrphanPruneDelete != nil {
		r.testHookBeforeOrphanPruneDelete(key)
	}
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := tx.Get(key) // records the read-check
		switch {
		case errors.Is(gerr, backend.ErrNotFound):
			return nil // already gone: idempotent no-op
		case gerr != nil:
			return gerr
		}
		payload, serr := stripEnvelope(cur)
		if serr != nil {
			return serr
		}
		if !bytes.Equal(payload, expected) {
			return errIndexEntryChanged
		}
		return tx.Delete(key)
	})
	if errors.Is(err, errIndexEntryChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
