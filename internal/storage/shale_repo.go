// Package storage's shale-backed metadata implementation.
//
// ShaleRepo satisfies the same service-layer interfaces as PasteRepo and
// SlateRepo, but talks to a shale cluster.Cluster, which routes each key to the
// node owning its shard. The slate backend is its per-node KV engine, so the
// cluster layer above it is the whole difference from SlateRepo.
//
// Design: docs/SPEC.md "Shale-backed metadata storage". Needs cgo +
// libslatedb_uniffi.
//
// # Key layout
//
// Co-location comes from shaleShardKey extracting a shard key per family, not
// from renaming keys:
//
//	pastes/<slug>                  -> <slug>
//	versions/<slug>/<NNNN>         -> <slug>
//	slug_owner/<slug>              -> <slug>
//	identity_pastes/<id>/<slug>    -> <id>     (per-owner enumeration index)
//	identity_first_seen/<id>       -> <id>
//	keygate/<subnet>/<identity>    -> <subnet> (Sybil admission)
//	keygate_id/<identity>/<subnet> -> <identity>
//
// Routing a family's keys to one shard makes a transaction touching one family
// for one subject a single-shard CAS via cluster.Transact(pinKey, fn).
//
// # Transaction model
//
// cluster.Transact is read-modify-write under optimistic concurrency. Two
// constraints shape the code:
//
//   - ScanPrefix is NOT supported inside a CAS transaction, so every scan runs
//     outside it. Where a scan's result must be race-safe (a version number
//     must not be reused), the key the decision hinges on is re-read INSIDE the
//     transaction as a read-check, so a racing writer conflicts.
//   - Put rejects empty values: the empty-payload shape is reserved for delete
//     tombstones. Marker-only families carry a one-byte value instead.
//
// # Cross-family writes and the scan-derived quota
//
// Insert / AppendVersion / Delete span the {slug} and {id} shards, which cannot
// be one transaction. There is no stored byte counter: the quota is DERIVED by
// one single-shard scan of the owner's enumeration index, summing the cached
// size each entry carries.
//
// So a write is: check quota (scan), authoritative write on {slug}, then
// best-effort index write on {id}. Durable used-bytes can never exceed the cap;
// the transient gaps (a crash between the two writes, an orphaned entry, two
// concurrent same-owner uploads passing the non-atomic check) are each bounded
// by one record and healed by the owner's next list.
//
// # File layout
//
// Paste-side: this file (config, lifecycle, CRUD, blob binding),
// shale_helpers.go (keys, codecs, scans), shale_keygate.go,
// shale_guarded_index.go.
// Site and room tiers and shard-key routing live in their own files.

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
	"strings"
	"sync"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/cluster"
	"github.com/Zamua/shale/pkg/coord"
	"github.com/Zamua/shale/pkg/coord/gossip"
	"github.com/Zamua/shale/pkg/rpc"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleConfig captures the parameters needed to open a shale cluster over the
// slate backend. The S3 connection fields mirror SlateConfig (same underlying
// SlateDB-on-object-storage engine); NodeID, the peer-discovery fields, and the
// consistency knobs are the cluster-layer additions.
//
// BindAddr selects the topology: empty is single-node (every op local, no
// gossip, no ring routing), non-empty brings up memberlist + gRPC forwarding
// and joins the ring. ShardKeyFn and the per-family co-location are identical
// at every node count.
type ShaleConfig struct {
	NodeID    string // stable node identity; required by cluster.Open
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false -> slate sets AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files

	// UnitCount selects the backend SHAPE. Zero opens the single-backend path:
	// one slatedb database per node. A value >= 1, which MUST be a power of two,
	// selects MULTI-BACKEND (sharded) mode: the keyspace is partitioned into
	// UnitCount units, each its own slatedb database, distributed across the
	// cluster's nodes by the ring and routed per key by ShardKeyFn ->
	// UnitForHash. Each unit is a full slatedb instance (memtable, WAL,
	// compaction) per owning replica, so keep this small on small deployments.
	// The two layouts are NOT on-disk-compatible: switching an existing
	// deployment is a one-time operator data copy, not in-place (docs/SPEC.md
	// "Sharded metadata (multi-backend mode)"). Operator env:
	// HOSTTHIS_SHALE_UNIT_COUNT.
	UnitCount int

	// BindAddr is the host:port memberlist listens on. NON-EMPTY enables
	// multi-node mode (gossip membership, ring routing, gRPC forwarding,
	// rebalance on membership change); empty resolves every op to the local
	// backend.
	BindAddr string

	// GRPCAddr is this node's gRPC forwarding address, broadcast to peers as
	// their forwarding target. Required whenever BindAddr is set (cluster.Open
	// errors otherwise). Ignored in single-node mode.
	GRPCAddr string

	// Seeds are other nodes' BindAddrs, used to bootstrap gossip discovery.
	// Empty means this node is the seed/founder. Ignored in single-node mode.
	Seeds []string

	// ReplicationFactor is forwarded to cluster.Config. Zero is normalized to 1
	// by cluster.Open (single owner per key, no replicas). R=1 is the
	// horizontal-write-scaling shape; R>1 trades write throughput for
	// availability (docs/SPEC.md "R=1 vs R=2").
	ReplicationFactor int

	// RelaxedDurability selects the slate backend's write-durability mode.
	// False, the zero value, is the SAFE DEFAULT: a write is acked only after
	// the object-store flush. True enables fast-ack at the memtable with a
	// background flush, removing the per-commit flush round-trip from the hot
	// path. ONLY safe at ReplicationFactor >= 2 across anti-affinity-separated
	// nodes (a replica holds the write through the flush window); at R=1 a
	// single crash loses the un-flushed writes. See docs/SPEC.md "Relaxed
	// durability: fast-ack at the memtable".
	//
	// The operator-facing env var HOSTTHIS_METADATA_AWAIT_DURABLE is the
	// INVERSE (default true), matching slatedb's own AwaitDurable terminology;
	// the field is inverted here so the struct's zero value is the safe one.
	RelaxedDurability bool

	// CacheBytes sizes the slatedb SST block + metadata cache (an in-memory
	// Moka cache) the slate backend is given. Zero disables it, and slatedb's
	// no-block-cache default re-fetches SST blocks from the object store on
	// every read, which is pathological on a distributed-MinIO backend. A few
	// hundred MiB holds the hot SST working set in RAM. Operator env:
	// HOSTTHIS_METADATA_CACHE_BYTES.
	CacheBytes uint64

	// ReapFenceWALs enables slatedb's fence-WAL garbage collector. slatedb
	// writes a small "fence" WAL object every time a unit DB is opened (to
	// claim the single-writer epoch), and ships that GC category in DRY-RUN
	// (garbage_collector_options.wal_fence_options.dry_run = true), so fence
	// WALs are never deleted and accumulate one-per-open forever, bloating a
	// unit's object-store prefix until a cold-start open (which re-reads every
	// WAL) crawls. True flips that flag off via the slate backend's Settings so
	// the GC reaps fence WALs older than its min_age (slatedb's conservative
	// 300s default) and a unit's WAL count stays bounded. False leaves
	// slatedb's defaults untouched, which suffices for callers that open
	// briefly and never accumulate (tests, the migration tool). Operator env:
	// HOSTTHIS_SLATEDB_FENCE_GC (default on; "false" is the kill-switch).
	ReapFenceWALs bool

	// WriteTimeout / ReadTimeout, when > 0, override the cluster's per-dispatch
	// write/read deadline (cluster.Config defaults each to 5s). The bulk
	// migration tool raises these well above 5s: a single CAS commit on an
	// un-GC'd (bloated) unit can stall past 5s under a migrate burst, and a
	// DeadlineExceeded there aborts the whole run.
	WriteTimeout time.Duration
	ReadTimeout  time.Duration

	// Logger receives the skip lines from the tolerant background scans (the
	// / repair-on-read) when they hit an undecodable record. nil falls back to
	// log.Default(). It never affects the blob-GC ref-set scans, which fail
	// closed rather than skip+log.
	Logger *log.Logger

	// BlobStore is the OPTIONAL streaming blob byte plane. When set, the repo
	// opens its cluster via cluster.NewBlobKV (the blob-capable surface), so a
	// staged blob's pointer co-commits with the metadata on the owning shard
	// (docs/SPEC.md "Shale-collocated blobs"). It must be a DISTINCT bucket
	// from the metadata's object store. nil keeps the metadata-only path
	// (cluster.Open, no *BlobKV), which ShaleBlobUnit is not wired for.
	BlobStore blob.Store

	// ConditionalStore is the OPTIONAL shared CAS arbiter (create-if-absent /
	// compare-and-set over the metadata object store) that enables the
	// HOMOGENEOUS bootstrap: cluster.Open then decides form-vs-join at runtime
	// against the __cluster/init marker instead of using the founder/joiner
	// seed asymmetry, and AllowSoloStart lets the first pod up come up solo and
	// contend to form. Every pod must wire the SAME store (same bucket, same
	// key prefix) so the marker is one shared object. nil keeps the seed-based
	// bootstrap. Only meaningful in multi-backend (sharded) mode. See
	// docs/SPEC.md "Homogeneous bootstrap (optional)".
	ConditionalStore storageunit.ConditionalStore

	// RegisterGRPC is an OPTIONAL, OPAQUE hook letting the composition root
	// register other cluster-internal services on the SAME gRPC server
	// NewShaleRepo constructs and serves in multi-node mode: one advertised
	// address per pod, one listener lifecycle, reachable at exactly the address
	// shale forwarding already dials (docs/SPEC.md "Multi-pod relay: the peer
	// transport"). Called once, after shale's own node service is registered
	// and BEFORE Serve starts. Ignored in single-node mode (no server exists).
	// The hook's type is all this package knows about what rides along.
	RegisterGRPC func(*grpc.Server)
}

// ShaleRepo is the shale-cluster-backed metadata store, satisfying the same
// service-layer interfaces as SlateRepo. Every operation goes through the
// cluster handle, which routes per-shard via shaleShardKey and commits
// single-shard writes through per-shard CAS.
//
// In multi-node mode (cfg.BindAddr != "") the repo ALSO owns the process-level
// gRPC peer-forwarding server: cluster.Open advertises the node's GRPCAddr via
// gossip but does not stand up the listener peers forward routed
// reads/writes/migrations to (docs/SPEC.md "Peer forwarding"). NewShaleRepo
// binds that listener and serves the cluster's rpc handlers on it.
type ShaleRepo struct {
	cluster *cluster.Cluster

	// kv is the blob-capable cluster surface, set ONLY when cfg.BlobStore is
	// non-nil. It wraps the SAME *Cluster as `cluster` (kv.Cluster() ==
	// cluster), so every r.cluster.* call site works either way; only the
	// authoritative writes that bind a staged blob need r.kv.Transact, so the
	// BindBlob co-commits with the metadata. nil on the metadata-only path.
	kv *cluster.BlobKV

	// logger records skipped records in the tolerant background scans (the
	// repair-on-read family), falling back to log.Default() via repoLog so a
	// persistently-bad row is still visible to an operator. The blob-GC ref-set
	// scans never use it: they fail closed rather than skip.
	logger *log.Logger

	// grpcAddr is the ACTUAL bound forwarding address advertised to peers
	// (lis.Addr().String()), or "" in single-node mode. bindAddr mirrors the
	// memberlist bind address a second node seeds off. nodeID is this node's
	// stable cluster identity, used to exclude self from PeerGRPCAddrs.
	grpcAddr string
	bindAddr string
	nodeID   string

	// grpcSrv + grpcLis are the peer-forwarding server and its listener, set
	// only in multi-node mode. Close GracefulStops the server, which drains
	// in-flight RPCs and closes the listener.
	grpcSrv *grpc.Server
	grpcLis net.Listener

	// confirmWG tracks in-flight background confirm goroutines. The insert's
	// index write is synchronous (the entry is the quota's accounting record -
	// see InsertWithQuotaCheck), so nothing enqueues here; the group and
	// WaitPendingConfirms are the drain seam Close and the tests join on.
	confirmWG sync.WaitGroup

	// cache is the slatedb SST block + metadata cache shared by the slate
	// backend (nil when CacheBytes==0). Operator-owned uniffi handle; Close
	// Destroys it after the cluster (and its backend) have shut down.
	cache *slatedb.DbCache

	// fenceGCSettings is the slatedb Settings object that enables the fence-WAL
	// garbage collector (see ShaleConfig.ReapFenceWALs), forwarded verbatim to
	// every unit's DbBuilder by the slate backend. An operator-owned uniffi
	// handle Close Destroys after the cluster (and its backends) shut down.
	fenceGCSettings *slatedb.Settings

	// closeFactory releases the multi-backend slate Backing after the cluster
	// shuts down: cluster.Close closes the mounted unit databases, this is the
	// backing-level net that flushes/closes anything left. Nil in
	// single-backend mode, where cluster.Close owns the single backend.
	closeFactory func() error

	// Test seams (nil in production; set only through the _test exports). The
	// repair paths' race windows are microseconds wide, so the tests that pin
	// the guarded index writes inject a concurrent operation at the exact point
	// the window opens.
	//
	// testHookReconcileBeforeIndexWrites runs after Reconcile has captured ALL
	// its snapshots (the enumeration index plus the authoritative
	// pastes/versions scans) and before the paste reprojection's prune + write
	// loops: the widest point of the snapshot-to-write window.
	testHookReconcileBeforeIndexWrites func()
	// testHookBeforeOrphanPruneDelete runs inside the orphan prune, after the
	// authoritative-row confirm and before the entry delete: the TOCTOU window
	// a same-slug delete-then-redeploy can race.
	testHookBeforeOrphanPruneDelete func(key []byte)
	// testHookGuardedIndexWrite runs at the top of every guarded index write; a
	// non-nil return fails that write. Fault injection for the Policy-1 pin:
	// one entry's write failure must not stall the rest of the reprojection.
	testHookGuardedIndexWrite func(key []byte) error
}

// WaitPendingConfirms blocks until every background confirm goroutine has
// finished. The insert's confirm step is synchronous, so this is currently a
// no-op; it is the drain seam Close and the tests go through.
func (r *ShaleRepo) WaitPendingConfirms() { r.confirmWG.Wait() }

// repoLog returns the repo's logger, falling back to the process default when
// none was wired, so a skipped undecodable record is never silently swallowed.
func (r *ShaleRepo) repoLog() *log.Logger {
	if r.logger != nil {
		return r.logger
	}
	return log.Default()
}

// slateConfigFromShale maps a ShaleConfig to the slate.Config used to open the
// per-node backend. Pure, so the WriteOptions wiring is unit-testable without a
// live object store. The S3 fields copy straight through; the only logic is the
// durability knob, and nil WriteOptions is slate's AwaitDurable=true.
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
// collector enabled. ONLY dry_run is flipped: every other GC category and the
// conservative min_age (slatedb's 300s default) stay untouched, so the GC reaps
// superseded fence WAL objects and never a data WAL or a still-live fence. See
// ShaleConfig.ReapFenceWALs. The returned handle is operator-owned: the caller
// forwards it to the slate backend and Destroys it on shutdown.
func newFenceWALGCSettings() (*slatedb.Settings, error) {
	s := slatedb.SettingsDefault()
	if err := s.Set("garbage_collector_options.wal_fence_options.dry_run", "false"); err != nil {
		s.Destroy()
		return nil, err
	}
	return s, nil
}

// NewShaleRepo opens a shale cluster over a fresh slate backend. The caller
// must Close() to flush and shut down the cluster, which shuts down the backend
// too.
//
// shaleShardKey is the ShardKeyFn, so every family co-locates on the shard
// keyed by its subject, which is what makes an owner's enumeration index and
// the per-slug authoritative rows each single-shard.
//
// In multi-node mode the gRPC peer-forwarding listener is bound BEFORE the
// cluster opens, and the listener's ACTUAL address is advertised. That matters
// when GRPCAddr is ":0": the real port is known only after Listen, and a peer
// forwarding to the advertised address must reach the one actually served.
func NewShaleRepo(cfg ShaleConfig) (*ShaleRepo, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("ShaleConfig.Bucket required")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ShaleConfig.NodeID required")
	}

	// Bind before opening the cluster so the advertised GRPCAddr is the address
	// actually served (resolving the ":0" / OS-assigned-port case).
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
	// Without a block cache slatedb re-fetches SST blocks from the object store
	// on every read: on a distributed-MinIO backend that is a self-inflicted
	// read storm, the same hot SSTs fetched hundreds of times a second. Close
	// Destroys the handle after the cluster shuts down.
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

	// The slate backend forwards these Settings verbatim to every unit's
	// DbBuilder (both the single-backend slate.Config and the multi-backend
	// BackingConfig below). Built before cleanup so the closure Destroys it on
	// any later open error.
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

	// Coordination adapter: shale exposes membership behind a Coordinator port,
	// of which gossip (SWIM + consistent hash) is one implementation. A nil
	// Coordinator IS the single-node path, so it needs no special case below.
	var coordinator coord.Coordinator
	if cfg.BindAddr != "" {
		coordinator = gossip.New(gossip.Config{
			BindAddr: cfg.BindAddr,
			Seeds:    cfg.Seeds,
		})
	}

	clusterCfg := cluster.Config{
		NodeID:            cfg.NodeID,
		Coordinator:       coordinator,
		GRPCAddr:          advertiseGRPCAddr,
		ReplicationFactor: cfg.ReplicationFactor,
		// Zero leaves cluster.Open's 5s per-dispatch deadline.
		WriteTimeout: cfg.WriteTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		// ReadQuorum, not ReadNearest: at R>1 ReadNearest decides on the first
		// replica to answer and treats a NotFound as usable, so a read served by
		// a still-backfilling replica (a freshly joined node) could return
		// NotFound for a key that exists on the other replica. ReadQuorum reads a
		// quorum and the present value wins on LWW. At R=1 a quorum IS the single
		// replica, so this costs no extra hop there. See docs/SPEC.md "Deploy
		// arc: replication factor 1, then scale out".
		ReadConsistency:  cluster.ReadQuorum,
		ShardKeyFn:       shaleShardKey,
		BlobStore:        cfg.BlobStore,
		ConditionalStore: cfg.ConditionalStore,
	}
	// Cold-start patience: a joiner re-sweeps its seeds for the cluster
	// generation up to GenLearnBudget (shale default 180s), so a still-mounting
	// seed is waited for instead of crash-looping the joiner. Raise it for a
	// slow backend; shorten it plus SHALE_DEBUG_MOUNT_DELAY to reproduce a
	// crash-loop on a real cluster.
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
		// and routed per key by ShardKeyFn (docs/SPEC.md "Sharded metadata").
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
		// SINGLE-BACKEND: one slatedb database.
		be, beErr := slate.New(sc)
		if beErr != nil {
			cleanup()
			return nil, fmt.Errorf("shale: open slate backend: %w", beErr)
		}
		clusterCfg.Backend = be
	}

	// Declarative resharding needs BOTH a multi-backend cluster (nothing to
	// reshard otherwise) and a shared CAS arbiter to coordinate the generation
	// advance. With both, the cluster drives the reshard target from the
	// unanimously gossiped declared count, so redeploying every pod with a
	// different power-of-two HOSTTHIS_SHALE_UNIT_COUNT triggers an online,
	// lossless split/merge (docs/SPEC.md "Online resharding").
	clusterCfg.DeclarativeReshard = clusterCfg.BackendFactory != nil && clusterCfg.ConditionalStore != nil

	// Both surfaces hold the SAME underlying *Cluster, so r.cluster is set
	// either way and every routed op works regardless of which one opened.
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
		nodeID:          cfg.NodeID,
		cache:           cache,
		fenceGCSettings: fenceGCSettings,
		closeFactory:    closeFactory,
	}

	// Stand up the peer-forwarding server the cluster advertised via gossip but
	// does not serve itself.
	if lis != nil {
		// The cluster's inter-node client keepalives every 30s WITH
		// PermitWithoutStream. gRPC's default server enforcement policy (MinTime
		// 5min, pings-without-streams disallowed) GOAWAYs those pings as
		// too_many_pings and closes the connection mid-preface, which presents
		// identically to a network drop ("error reading server preface: use of
		// closed network connection") and stalls the cross-shard scan.
		g := grpc.NewServer(
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
		)
		rpc.NewServer(cl).Register(g)
		// Composition-root services register on the SAME server, before Serve.
		if cfg.RegisterGRPC != nil {
			cfg.RegisterGRPC(g)
		}
		r.grpcSrv = g
		r.grpcLis = lis
		go func() {
			// Serve returns when Close's GracefulStop closes the listener: a
			// clean shutdown, not an error to surface.
			_ = g.Serve(lis)
		}()
	}

	return r, nil
}

// Close shuts down the peer-forwarding gRPC server, then the cluster (and the
// underlying slate backend). GracefulStop drains in-flight RPCs and closes the
// listener, so the forwarding port is released with no leaked goroutine.
func (r *ShaleRepo) Close() error {
	// Drain in-flight confirms first: a confirm stranded by the teardown leaves
	// a later list to heal an index entry that could have been written cleanly.
	r.confirmWG.Wait()
	if r.grpcSrv != nil {
		r.grpcSrv.GracefulStop() // also closes r.grpcLis
	}
	var err error
	if r.cluster != nil {
		err = r.cluster.Close()
	}
	// Release the slate Backing only after the cluster closed its mounted units.
	if r.closeFactory != nil {
		if ferr := r.closeFactory(); ferr != nil && err == nil {
			err = ferr
		}
	}
	// AFTER the cluster (and its slate backend) shut down, so no in-flight read
	// still references it.
	if r.cache != nil {
		r.cache.Destroy()
		r.cache = nil
	}
	// Same ordering: the opened units captured their GC config at Build time, so
	// nothing references this handle once they have shut down.
	if r.fenceGCSettings != nil {
		r.fenceGCSettings.Destroy()
		r.fenceGCSettings = nil
	}
	return err
}

// GRPCAddr returns this node's ACTUAL bound gRPC forwarding address, the one
// advertised to peers, or "" in single-node mode. The configured GRPCAddr may
// have been ":0", so the served port is only knowable post-bind.
func (r *ShaleRepo) GRPCAddr() string { return r.grpcAddr }

// BindAddr returns this node's memberlist bind address, or "" in single-node
// mode. A second node passes it as its Seeds entry to join this node's ring.
func (r *ShaleRepo) BindAddr() string { return r.bindAddr }

// PeerGRPCAddrs returns the CURRENT gRPC addresses of every OTHER live cluster
// member, read from the gossiped ring membership. These are the addresses peers
// advertised at join (exactly what shale forwarding dials), kept current by the
// same gossip that tracks joins, leaves and deploy churn, so the view is
// fresher than DNS. The composition root adapts it onto the relay's Peers port
// (docs/SPEC.md "Multi-pod relay: peer discovery"). Single-node returns empty.
// Safe for concurrent use.
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

// --- PasteRepo / PasteAdmin reads ------------------------------------------

func (r *ShaleRepo) Get(slug domain.Slug) (domain.Paste, error) {
	var row pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &row); err != nil {
		return domain.Paste{}, err
	}
	return row.toDomain(slug), nil
}

// ListByOwner returns the owner's pastes from ONE single-shard prefix scan of
// identity_pastes/<id>/, rendering each row from the entry's own cached fields.
// No per-item reads: listing cost is FLAT in how many pastes an owner holds
// (docs/SPEC.md "Listing is O(1) reads").
//
// Nothing here validates an entry against its authoritative row. A crash
// between the two writes of an insert (or a delete) can leave an entry whose
// paste does not exist, and that entry is simply listed - a deliberate
// acceptance, not an oversight. Detecting it would mean reading every paste on
// every listing, which is the cost this design exists to remove.
//
// An entry written before the display fields existed carries no Kind and cannot
// be rendered from the cache, so it is resolved once against its row and
// rewritten fat. That is a migration path, self-limiting to one read per entry
// ever - not a validation pass.
func (r *ShaleRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return nil, err
	}

	out := make([]domain.Paste, 0, len(idx))
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var e identityPasteRow
		if err := json.Unmarshal(item.Value, &e); err == nil && e.Kind != "" && !e.Placeholder {
			out = append(out, e.toDomain(slug, owner)) // the common path: zero reads
			continue
		}
		paste, ok, err := r.upgradeListEntry(owner, slug, item)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, paste)
		}
	}
	sortByUpdatedAtDesc(out)
	return out, nil
}

// upgradeListEntry renders a pre-fat entry from its authoritative row and
// rewrites the entry with the display fields, so the slow path runs at most
// once per entry. An entry whose row is unreadable is skipped rather than
// repaired: it cannot be rendered, and cleaning it up is explicitly not this
// listing's job.
func (r *ShaleRepo) upgradeListEntry(owner string, slug domain.Slug, item scanItem) (domain.Paste, bool, error) {
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.Paste{}, false, nil
		}
		return domain.Paste{}, false, err
	}
	if domain.NormalizeStatus(head.Status) == domain.PasteStatusFailed {
		return domain.Paste{}, false, nil // its bytes never landed
	}

	live, latest := head.LiveBytes, head.LatestVersion
	if latest == 0 {
		// Predates the head totals: derive them once. The rewrite below stamps
		// them, so this never repeats for this slug.
		versions, err := r.scanVersions(slug)
		if err != nil {
			return domain.Paste{}, false, err
		}
		live, latest = int(liveVersionBytes(versions)), latestActiveVerNum(versions)
	}

	fresh := identityPasteRow{
		Name: head.Name, Size: live, CreatedAt: head.CreatedAt,
		Kind: head.Kind, LatestVersion: latest,
		PinnedVersion: head.PinnedVersion, UpdatedAt: head.UpdatedAt,
	}
	if _, werr := r.guardedPutIndexEntry(item.Key, item.Value, true, fresh); werr != nil {
		r.repoLog().Printf("shale: index upgrade for %s: %v (retried on a later list)", slug, werr)
	}
	return fresh.toDomain(slug, owner), true, nil
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

// CountByOwner returns how many pastes the owner's enumeration index holds.
// It counts entries and reads no authoritative rows, so it agrees with what
// ListByOwner renders (docs/SPEC.md "Listing is O(1) reads") - including a
// phantom entry, which both count and list report. MarkFailed and Delete drop
// the entry, so neither a failed nor a deleted paste is counted.
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

// SumActiveBytesByOwner derives the owner's active PASTE bytes from ONE
// single-shard prefix scan of identity_pastes/<id>/, summing the cached size
// each entry carries with ZERO per-entry fan-out to the {slug} shards. The
// write paths keep the cached size equal to the paste's live (non-deleted)
// version sum and the owner's next list heals drift, so the figure
// trails the authoritative sums by at most that owner's next read (docs/SPEC.md
// "Scan-derived quota").
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

// sumActiveBytesForOwner scans identity_pastes/<owner>/ once and sums each
// entry's cached size. The entry IS the accounting record, so the authoritative
// {slug} rows are never read, save for the legacy exception below. Trusting the
// cache has two consequences (docs/SPEC.md "Scan-derived quota"):
//
//   - a stale entry whose paste is GONE keeps counting its cached bytes until
//     the owner's next list prunes it: a bounded over-count, which can only wrongly
//     reject, never admit an over-cap write,
//   - failed pastes are absent by construction (MarkFailed drops the entry;
//     a failed row's entry is pruned on the owner's next list).
//
// Fail-closed (Policy 2; this is a synchronous write-path read): an entry that
// does not decode, or that carries the fail-closed Placeholder marker for an
// undecodable authoritative record, HARD-FAILS the scan, rejecting the upload
// rather than silently under-counting. The one exception is a LEGACY entry,
// recognized by shape (an EMPTY value, the bare-marker convention an in-place
// migration carries over), which is read through its authoritative rows until
// their next list enriches it. Without that fallback one migrated entry would
// hard-fail every quota-checked create for its owner until they next list.
func (r *ShaleRepo) sumActiveBytesForOwner(owner string, now time.Time) (int64, error) {
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		if len(item.Value) == 0 {
			n, err := r.legacyPasteEntryBytes(item.Key)
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
			return 0, fmt.Errorf("quota scan: %s is a fail-closed placeholder (authoritative record undecodable; a list clears it once the record decodes again)", item.Key)
		}
		total += int64(row.Size)
	}
	return total, nil
}

// legacyPasteEntryBytes resolves a LEGACY (empty-valued) identity_pastes entry
// through its authoritative rows, the read-through a migrated deployment needs
// until the owner's next list enriches the entry. An empty value is the only legacy
// paste shape (unlike sites, pastes have no marker-byte era). A stale entry
// whose row is gone contributes zero; an undecodable row HARD-FAILS (Policy 2);
// a live row contributes its live version sum.
func (r *ShaleRepo) legacyPasteEntryBytes(indexKey []byte) (int64, error) {
	slug := domain.Slug(extractSlug(indexKey))
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil // stale legacy entry; the owner's next list prunes it
		}
		return 0, err
	}
	return r.sumLiveVersionBytes(slug)
}

// sumLiveVersionBytes sums the sizes of a paste's non-deleted version rows on
// the {slug} shard.
func (r *ShaleRepo) sumLiveVersionBytes(slug domain.Slug) (int64, error) {
	versions, err := r.scanVersions(slug)
	if err != nil {
		return 0, err
	}
	return liveVersionBytes(versions), nil
}

// liveVersionBytes sums the non-deleted rows of an ALREADY-scanned version
// slice, so a caller holding them (ListByOwner) pays no second scan.
func liveVersionBytes(versions []versionRow) int64 {
	var total int64
	for _, v := range versions {
		if v.Deleted {
			continue
		}
		total += int64(v.Size)
	}
	return total
}

// combinedActiveBytes is the per-owner "used" figure the quota checks compare
// against the cap before an authoritative write. The cap spans both kinds, so
// it must sum PASTE and SITE bytes: two single-shard scans of cached
// enumeration-entry values, no per-entry fan-out.
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
// stash. That is what keeps two concurrent same-slug writes (two updates, an
// update vs a delete, two DeployToSlug) from observing or clobbering each
// other's binds: there is no mutable state keyed by slug. A retry-on-collision
// loop reuses the same context, so every attempt binds the same refs (BindBlob
// is an idempotent tx.Put of the bref key). With no refs on the context the
// authoritative writes take the no-bind branch.

// pendingBindsKey is the unexported context key the staged refs ride under. A
// dedicated unexported type avoids collisions with any other package's context
// values.
type pendingBindsKey struct{}

// WithPendingBinds returns a child context carrying refs for the next
// authoritative {slug} write triggered under it. Empty refs return the parent
// unchanged (the no-bind path).
func WithPendingBinds(ctx context.Context, refs []cluster.BlobRef) context.Context {
	if len(refs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, pendingBindsKey{}, refs)
}

// pendingBindsFromContext returns the staged refs carried by ctx, or nil. The
// public WithQuotaCheck methods read it once and pass the refs down to the
// authoritative helper, which binds them in the metadata's own transaction.
func pendingBindsFromContext(ctx context.Context) []cluster.BlobRef {
	if ctx == nil {
		return nil
	}
	refs, _ := ctx.Value(pendingBindsKey{}).([]cluster.BlobRef)
	return refs
}

// StageBlobStream streams an already-encoded body to the final object,
// returning the BlobRef the binding transaction consumes. routeKey is the
// metadata key whose shard the blob co-locates with (pastes/<slug> or
// sites/<slug>, which route to the same unit). contentHash rides on the ref
// into the persisted blob.Pointer, feeding the site read path's sha ->
// blob-id side-table.
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

// GetBlobStream streams the STORED (still compressed) bytes for blobid under
// routeKey's shard; ShaleBlobUnit.Read wraps it in the magic-peek + zstd
// decode. ctx MUST outlive the returned reader, which drives the lazy
// object-store stream. An unbound or deleted pointer yields blob.ErrNotFound.
func (r *ShaleRepo) GetBlobStream(ctx context.Context, routeKey []byte, blobid string) (io.ReadCloser, int64, error) {
	if r.kv == nil {
		return nil, 0, errors.New("shale: GetBlobStream requires a blob-configured cluster (cfg.BlobStore was nil)")
	}
	return r.kv.GetBlob(ctx, routeKey, blobid)
}

// SweepBlobOrphans reclaims staged-but-unbound blob objects under this node's
// mounted units, age-gated by grace. A no-op on the metadata-only path; the
// caller gates on HasBlobPlane.
func (r *ShaleRepo) SweepBlobOrphans(ctx context.Context, now time.Time, grace time.Duration) error {
	if r.kv == nil {
		return nil
	}
	return r.kv.SweepOrphans(ctx, now, grace)
}

// HasBlobPlane reports whether this repo runs the transactional shale-blob
// path. The cmd wiring uses it to pick ShaleBlobUnit over StandaloneBlobUnit
// and to schedule SweepOrphans.
func (r *ShaleRepo) HasBlobPlane() bool { return r.kv != nil }

// DebugClusterState returns the cluster's per-position handoff dump for live
// diagnosis, or "" when there is no cluster. Exposed so the metadata adapter
// can serve it on an optional debug port without leaking the cluster handle.
func (r *ShaleRepo) DebugClusterState() string {
	if r.cluster == nil {
		return ""
	}
	return r.cluster.DebugState()
}

// RouteKeyForSlug returns the canonical metadata route key a slug's blobs
// co-locate with: pastes/<slug>. pastes/<slug> and sites/<slug> both shard on
// <slug>, so they resolve to the SAME unit and the SAME bref key, and one route
// key covers staging, reading and unbind-key derivation for either record kind.
// Exported because ShaleBlobUnit lives in another package and shaleKeyPaste
// does not.
func (r *ShaleRepo) RouteKeyForSlug(slug string) []byte {
	return shaleKeyPaste(domain.Slug(slug))
}

// ResolveBlobID maps a (slug, contentSHA) back to the shale blob id GetBlob
// needs. The http/manage read seam holds only the content sha, while the blob
// id lives on the metadata, so this routed lookup bridges the two. It checks,
// in order:
//
//  1. the paste head row: its served ContentSHA -> its BlobID (the common
//     path, since an HTML/markdown paste read passes the head's ContentSHA),
//  2. the paste's version rows: a non-head (pinned or Show'd) version whose
//     ContentSHA matches -> that version's BlobID,
//  3. the site row's FileBlobs[sha], for a static-site file read.
//
// Returns ("", ErrNotFound) when no metadata references the sha (a deleted or
// unbound blob), which the seam maps to blob.ErrNotFound. A legacy row with an
// empty BlobID returns ""; the seam reads "" as sha-keyed and falls back.
func (r *ShaleRepo) ResolveBlobID(slug domain.Slug, contentSHA string) (string, error) {
	var p pasteRow
	perr := r.getJSON(shaleKeyPaste(slug), &p)
	if perr == nil {
		if p.ContentSHA == contentSHA && p.BlobID != "" {
			return p.BlobID, nil
		}
		// Not the head's served sha: a pinned version, or manage Show of a
		// specific version.
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
	// Not a paste: try a site file.
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
// routeKey's shard. UnbindBlob is a tx.Delete of brefKey(ref), which needs only
// the route shard, unit and blob id, all derivable from routeKey by the same
// derivation StageBlob/GetBlob use. Size and ContentHash do not enter the bref
// key, so they stay zero. The pointer co-shards with routeKey, so the unbind
// lands in the same {slug} transaction as the metadata delete.
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
// paste+site used bytes come from the enumeration indexes, and the upload is
// rejected with ErrOverUserQuota if used+body would exceed the cap. The check
// and the write are NOT atomic, so two concurrent uploads from the SAME
// identity can both pass and both land - a bounded over-admit, acceptable
// because one key is one person and the object-store bucket quota backstops the
// durable total (docs/SPEC.md "Scan-derived quota").
//
// The authoritative write is the {slug} CAS (slug-collision read-check + blob
// bind); the {id} enumeration-index entry and first-seen are written
// SYNCHRONOUSLY after it, since the index IS the quota source of truth and a
// subsequent scan must see this paste. A crash between the two leaves a paste
// the index does not list: a bounded under-count the owner's next list heals.
func (r *ShaleRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	identity := p.Identity.String()
	body := int64(p.Size)

	// Read the staged refs once and pass them down, so the authoritative {slug}
	// transaction binds exactly this call's blobs and not a racing write's.
	binds := pendingBindsFromContext(ctx)

	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: used}).Admit(body); err != nil {
			return err
		}
	}

	// Pre-check the slug on the {slug} shard BEFORE writing anything. A taken
	// slug returns the collision sentinel with no entry written, which is what
	// stops the upload's re-mint loop stranding one entry per attempt. It is
	// not atomic with the authoritative insert below - that CAS carries the
	// authoritative check - so a genuine race still strands one entry, bounded
	// and pruned by the next ListByOwner.
	if taken, err := r.slugTaken(p.Slug); err != nil {
		return err
	} else if taken {
		return ErrSlugTaken
	}

	// ENTRY FIRST, then the authoritative row. The two live on different shards
	// and a transaction touches one, so whichever is written second can be lost
	// to a crash. Written this way the survivor is an entry with no row, which
	// ListByOwner sees and repairs; the other order leaves a row no single-shard
	// read can find, which is what used to require a cluster-wide reconcile
	// pass. See docs/SPEC.md "No periodic reconcile: the index entry is written
	// FIRST".
	if err := r.confirmInsert(p); err != nil {
		return fmt.Errorf("enumeration entry: %w", err)
	}

	if err := r.insertAuthoritative(p, binds); err != nil {
		// The entry written above is now an orphan. Best-effort removal keeps
		// the common failure (a slug race) from charging the owner until they
		// next list; a failure here leaves exactly the case ListByOwner repairs.
		if derr := r.cluster.Delete(shaleKeyIdentityPaste(p.Identity.String(), p.Slug.String())); derr != nil {
			r.repoLog().Printf("shale: rollback of enumeration entry for %s: %v (it counts against the owner until their next list)", p.Slug, derr)
		}
		return err
	}
	return nil
}

// slugTaken reports whether slug already names a paste or a site. Both keys
// co-shard with the slug, so this is one shard's read.
func (r *ShaleRepo) slugTaken(slug domain.Slug) (bool, error) {
	for _, key := range [][]byte{shaleKeyPaste(slug), shaleKeySite(slug)} {
		raw, err := r.getRaw(key)
		if err != nil {
			return false, fmt.Errorf("slug pre-check %s: %w", key, err)
		}
		if raw != nil {
			return true, nil
		}
	}
	return false, nil
}

// shaleKVTx is the minimal transaction surface the authoritative writes need.
// BOTH backend.Transaction (the metadata-only path) and *cluster.BlobTx (the
// blob-binding path) satisfy it, so one closure body serves both.
type shaleKVTx interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error
}

// runAuthoritative runs body in a {slug}-pinned single-shard CAS transaction,
// routing through the blob-capable r.kv.Transact when refs must co-commit and
// the plain cluster transaction otherwise. body receives a bind callback that
// is a no-op on the no-bind path, so the collision/read-set logic is written
// once and the two paths cannot drift.
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

// insertAuthoritative writes the {slug}-shard rows in one CAS transaction: the
// paste row, the v1 version row and slug_owner. The
// slug-collision check (reject if pastes/<slug> OR sites/<slug> exists) is part
// of the transaction's read-set, so a racing insert of the same slug as either
// kind conflicts.
//
// A staged blob in refs is BOUND in this same transaction, so the pointer
// co-commits with the row and the blob id lands on both the head and the v1
// version row.
func (r *ShaleRepo) insertAuthoritative(p domain.Paste, refs []cluster.BlobRef) error {
	pasteKey := shaleKeyPaste(p.Slug)
	blobID := firstBlobID(refs)
	return r.runAuthoritative(pasteKey, refs, func(tx shaleKVTx, bind func() error) error {
		// This Get is also the ExpectAbsent read-check that makes a concurrent
		// insert of the same slug conflict.
		if _, err := tx.Get(pasteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("slug check: %w", err)
		}
		// A slug is EITHER a paste or a site. sites/<slug> co-shards with
		// pastes/<slug>, so this stays a same-shard read inside the CAS; the
		// site insert carries the reciprocal check.
		if _, err := tx.Get(shaleKeySite(p.Slug)); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("site slug check: %w", err)
		}
		pr := pasteFromDomain(p)
		pr.BlobID = blobID
		// v1 is the only version at insert, so the totals are known exactly.
		pr.LiveBytes = p.Size
		pr.LatestVersion = 1
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
// staged file refs, so the read path can resolve a manifest sha to the blob id
// GetBlob needs. A site dedups identical files, and keying on sha is what makes
// two manifest paths sharing content collapse onto one blob id. Returns nil for
// no refs, which omitempty keeps out of the row.
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

// confirmInsert writes the identity_pastes index entry and sets
// identity_first_seen if absent, on the {id} shard in one CAS. The entry is
// what SumActiveBytesByOwner sums (its cached size seeds at v1's size, the
// paste's whole live sum at insert) and how ListByOwner / CountByOwner
// enumerate. Idempotent: a re-run overwrites the same entry and leaves an
// already-set first-seen untouched.
func (r *ShaleRepo) confirmInsert(p domain.Paste) error {
	identity := p.Identity.String()
	slug := p.Slug.String()
	indexKey := shaleKeyIdentityPaste(identity, slug)
	firstSeenKey := shaleKeyIdentityFirstSeen(identity)
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if err := shaleTxPutJSON(tx, indexKey, identityPasteRow{
			Name:          p.Name,
			Size:          p.Size,
			CreatedAt:     p.CreatedAt,
			Kind:          string(p.Kind),
			LatestVersion: 1,
			PinnedVersion: p.PinnedVersion,
			UpdatedAt:     p.UpdatedAt,
		}); err != nil {
			return err
		}
		// Write-if-absent keeps this a MIN(created_at).
		if _, err := tx.Get(firstSeenKey); errors.Is(err, backend.ErrNotFound) {
			return tx.Put(firstSeenKey, []byte(p.CreatedAt.UTC().Format(time.RFC3339Nano)))
		} else if err != nil {
			return err
		}
		return nil
	})
}

// MarkReady flips a paste's status pending -> ready on the {slug} shard, the
// background finalizer's success transition once the blob landed. Only a
// still-pending paste advances, so a late finalizer cannot resurrect a paste
// it already failed; already-ready, failed and absent are all
// no-ops. See docs/SPEC.md "Paste lifecycle status (async blob write)".
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
// enumeration-index entry, serving both the background finalizer's failure
// transition and the list-time age-out. The paste ROW stays, flipped to
// failed, so a read can serve an error page; its bytes stop counting the
// instant the status flips, because the scan skips a failed head row. Only a
// still-pending paste transitions, so this never un-counts a ready paste.
// Idempotent. See docs/SPEC.md "Paste lifecycle status (async blob write)".
func (r *ShaleRepo) MarkFailed(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	// Step 1: flip the {slug}-shard status and capture the owner, so step 2 can
	// find the index entry. A paste that is not pending has nothing to release.
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
	// Step 2: drop the enumeration-index entry on the {id} shard so the failed
	// paste leaves ListByOwner and stops being enumerated at all.
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
// versions) plus the new version's bytes must not exceed the cap. The check and
// the write are not atomic, the same bounded over-admit InsertWithQuotaCheck
// documents.
//
// The version row lands on the {slug} shard; the index projection's cached size
// is then refreshed on the {id} shard, which is what starts the new version's
// bytes counting.
func (r *ShaleRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	// Read the staged refs once and pass them down, isolating this append from
	// any concurrent same-slug write.
	binds := pendingBindsFromContext(ctx)

	var existing pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	identity := existing.Identity
	body := int64(size)

	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return AppendResult{}, err
		}
		// `used` already counts the paste's existing versions, so the charge is
		// just the new version's bytes.
		if err := (domain.Allowance{Cap: userCap, Used: used}).Admit(body); err != nil {
			return AppendResult{}, err
		}
	}

	res, err := r.appendAuthoritative(slug, kind, contentSHA, size, now, binds)
	if err != nil {
		return AppendResult{}, err
	}

	// Best-effort: a lost refresh leaves a stale cached size until the next
	// reprojection (bounded drift, docs/SPEC.md "Scan-derived quota"), never a
	// failed append.
	if err := r.refreshIndexProjection(identity, slug); err != nil {
		r.repoLog().Printf("shale: index refresh for append %s: %v (index lag; the owner's next list heals it)", slug, err)
	}
	return res, nil
}

// errVerTaken signals that the candidate version number was already present at
// read time inside a {slug} transaction, so the pre-scan the closure was built
// from is stale. The closure aborts with it (Transact returns a non-conflict fn
// error verbatim). Never escapes the function that raises it.
var errVerTaken = errors.New("shale: candidate version number already taken")

// ErrConcurrentChange aliases the domain-owned sentinel, matching how the rest
// of the storage error vocabulary is re-exported.
var ErrConcurrentChange = domain.ErrConcurrentChange

// appendAuthoritative writes the new version row on the {slug} shard. The next
// version number comes from a pre-scan outside the
// tx, so two race outcomes are retried by re-scanning for a fresh number:
//
//   - the candidate version key is ALREADY present at read time inside the tx
//     (a concurrent append committed it after the scan): errVerTaken,
//   - the candidate key is absent at read time but a concurrent append commits
//     it first, so the ExpectAbsent read-check fails validation: ErrCASConflict.
//
// MAX(ver_num) counts tombstones, so version numbers are never reused.
func (r *ShaleRepo) appendAuthoritative(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, now time.Time, refs []cluster.BlobRef) (AppendResult, error) {
	pasteKey := shaleKeyPaste(slug)
	// The blob id lands on the new version row and, when the head is unpinned
	// (so the public URL follows this version), on the paste head row too.
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
			// Reading the candidate key records an ExpectAbsent read-check, so a
			// concurrent commit of it after this read conflicts at Commit time.
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
			p.UpdatedAt = now
			// The totals roll in THIS transaction, so they can never disagree
			// with the version rows they summarise.
			p.LiveBytes += size
			p.LatestVersion = newVer
			if p.PinnedVersion == 0 {
				p.contentRef = newV.contentRef // unpinned head rolls to the new version, whole
			}
			if err := shaleTxPutJSON(tx, pasteKey, p); err != nil {
				return err
			}
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

// refreshIndexProjection updates the owner's identity_pastes projection after a
// size-changing {slug} write: it recomputes the paste's live byte sum from the
// authoritative version rows into the cached size.
//
// One {id}-shard CAS, GUARDED: the entry's payload is captured BEFORE the
// recompute and the write commits only if the entry still holds it, so two
// concurrent same-slug refreshes cannot land older-sum-last. The loser SKIPS
// rather than clobbering the fresher sum, costing at most one cycle of
// staleness that the next list converges; there is no recompute retry on the
// response path (docs/SPEC.md "Scan-derived quota" / "Window C").
//
// A missing entry is left missing and a LEGACY empty entry left for the
// list-time enrichment (meanwhile the quota scan reads it through the
// authoritative rows). A Placeholder has no trustworthy fields to preserve, so
// it is left whole rather than part-patched.
func (r *ShaleRepo) refreshIndexProjection(identity string, slug domain.Slug) error {
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	expected, err := r.getRaw(indexKey)
	if err != nil {
		return err
	}
	if len(expected) == 0 {
		return nil // absent, or legacy-empty; the owner's next list handles both
	}
	var row identityPasteRow
	if err := json.Unmarshal(expected, &row); err != nil {
		return fmt.Errorf("decode %s: %w", indexKey, err)
	}
	if row.Placeholder {
		return nil // fail-closed placeholder: only a list that decodes the row replaces it
	}
	// Refresh from the head row, which carries the totals transactionally: one
	// routed read instead of a version-family scan.
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		return err
	}
	row.Size = head.LiveBytes
	row.Kind = head.Kind
	row.LatestVersion = head.LatestVersion
	row.PinnedVersion = head.PinnedVersion
	row.UpdatedAt = head.UpdatedAt
	written, err := r.guardedPutIndexEntry(indexKey, expected, true, row)
	if err != nil {
		return err
	}
	if !written {
		r.repoLog().Printf("shale: index refresh %s skipped: entry changed during the recompute (a concurrent write landed; the next list converges)", indexKey)
	}
	return nil
}

// Delete removes a paste entirely: a whole-paste delete is a full removal, not
// a tombstone. The authoritative {slug} rows go away and the {id}
// enumeration-index entry is dropped, which is what stops the owner's
// scan-derived quota sum counting the paste. Idempotent on a missing paste.
//
// The version enumeration runs outside the transaction (ScanPrefix is
// unsupported inside a CAS tx), so the cascade joins the read set two ways: the
// head row is read INSIDE the tx, and the candidate NEXT version key is read
// ExpectAbsent. Without those the tx would carry only writes and commit
// unconditionally, leaving a version row a concurrent append wrote alive under
// a deleted paste - an orphan the owner's next list is the only thing that prunes,
// whose ContentSHA keeps its blob out of the GC's reach forever.
//
// A racing append aborts the delete rather than re-scanning. Transact already
// spends a full CAS budget on the conflict, so a second retry layer here would
// only re-run an exhausted one; and the version set is read OUTSIDE the tx, so
// the only fix for a stale scan is to start over from the caller.
func (r *ShaleRepo) Delete(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	var head pasteRow
	if err := r.getJSON(pasteKey, &head); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	versions, err := r.scanVersions(slug)
	if err != nil {
		return err
	}
	// MAX(ver_num) counts tombstones, so this is the number a racing append
	// would claim next.
	nextVerKey := shaleKeyVersion(slug, maxVerNum(versions)+1)

	// The blobs unbind in the SAME {slug} transaction, so the bytes go
	// unreferenced exactly when the rows vanish and SweepOrphans reclaims
	// them after the grace. unbind is a no-op on the metadata-only path,
	// where the global content-addressed sweep reclaims instead.
	delBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		// Read the head inside the tx so a racing append conflicts.
		if _, err := tx.Get(pasteKey); err != nil {
			return err
		}
		if _, gerr := tx.Get(nextVerKey); gerr == nil {
			return errVerTaken
		} else if !errors.Is(gerr, backend.ErrNotFound) {
			return gerr
		}
		if err := tx.Delete(pasteKey); err != nil {
			return err
		}
		for _, v := range versions {
			// A tombstoned version's pointer was already unbound by
			// DeleteVersion; re-unbinding is an idempotent tx.Delete of a
			// missing key. A legacy row carries no BlobID and has no pointer.
			if v.BlobID != "" {
				if err := unbind(v.BlobID); err != nil {
					return err
				}
			}
			if err := tx.Delete(shaleKeyVersion(slug, v.VerNum)); err != nil {
				return err
			}
		}
		return tx.Delete(shaleKeySlugOwner(slug))
	}
	var txErr error
	if r.kv != nil {
		txErr = r.kv.Transact(pasteKey, func(tx *cluster.BlobTx) error {
			return delBody(tx, func(blobID string) error {
				return tx.UnbindBlob(r.blobRefFor(pasteKey, blobID))
			})
		})
	} else {
		txErr = r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
			return delBody(tx, func(string) error { return nil })
		})
	}
	switch {
	case txErr == nil:
	case errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict):
		return fmt.Errorf("shale: delete %q: %w", slug, ErrConcurrentChange)
	case errors.Is(txErr, ErrNotFound):
		return nil // a concurrent delete cascaded first; idempotent
	default:
		return txErr
	}

	// Drop the enumeration-index entry on the {id} shard so the paste leaves
	// the owner's scan. Idempotent.
	indexKey := shaleKeyIdentityPaste(head.Identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			return tx.Delete(indexKey)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return nil
	})
}

// DeleteVersion tombstones a single version: it stays visible in the list
// flagged deleted, but its content blob is no longer referenced, so the GC
// reclaims it. Its bytes leave the owner's scan-derived sum via the
// index-projection refresh after the tombstone commits; a lost refresh is a
// bounded stale-cache window the owner's next list heals. Re-deleting an
// already-tombstoned version is a no-op.
func (r *ShaleRepo) DeleteVersion(slug domain.Slug, ver int) error {
	// Existence gate: a missing paste yields ErrNotFound.
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		return err
	}
	verKey := shaleKeyVersion(slug, ver)
	// The tombstone tx pins on verKey while the blob pointer routes on {slug},
	// but versions/<slug>/<NNNN> and pastes/<slug> shard on the same <slug>, so
	// the unbind co-commits in one single-shard transaction. Each version's blob
	// has a unique stage-minted id (no within-record dedup), so no live sibling
	// references it and the unbind is unconditional; the served head version
	// cannot be deleted (the service guards it), so the head's blob id is never
	// the one unbound here.
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
		// The head's LiveBytes sheds this version's bytes in the SAME
		// transaction, so the total never disagrees with the rows it
		// summarises. LatestVersion is untouched: version numbers are never
		// reused, so tombstoning one does not lower the high-water mark.
		var head pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &head); err != nil {
			return err
		}
		head.LiveBytes -= v.Size
		if head.LiveBytes < 0 {
			head.LiveBytes = 0
		}
		if err := shaleTxPutJSON(tx, pasteKey, head); err != nil {
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
	// Shed the tombstoned version's bytes from the owner's cached projection.
	// Best-effort: a lost refresh leaves the cached size too LARGE, a bounded
	// over-count that can only over-reject, until the next reprojection.
	if err := r.refreshIndexProjection(p.Identity, slug); err != nil {
		r.repoLog().Printf("shale: index refresh for tombstone %s/%d: %v (index lag; the owner's next list heals it)", slug, ver, err)
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
	// Best-effort refresh of the denormalized name in the index projection.
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

// ownerOfSlug resolves a slug's owner identity from slug_owner in one
// {slug}-shard read. Returns "" if the paste is gone, which makes the
// best-effort index refresh it feeds a harmless no-op.
func (r *ShaleRepo) ownerOfSlug(slug domain.Slug) string {
	raw, err := r.getRaw(shaleKeySlugOwner(slug))
	if err != nil || raw == nil {
		return ""
	}
	return string(raw)
}

func (r *ShaleRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	pasteKey := shaleKeyPaste(slug)
	err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		// Repoint the head's served descriptor as ONE value: the version row
		// carries the full contentRef (including BlobID, which domain.Version
		// does not), so no field can drift. The version row co-shards on {slug},
		// keeping this read inside the same CAS.
		var vr versionRow
		if err := shaleTxGetJSON(tx, shaleKeyVersion(slug, ver.VerNum), &vr); err != nil {
			return err
		}
		p.PinnedVersion = ver.VerNum
		p.contentRef = vr.contentRef
		return shaleTxPutJSON(tx, pasteKey, p)
	})
	if err == nil {
		// The listing renders the pin from the cached entry, so it has to move
		// with the head. Best-effort: a lost refresh is corrected by the
		// spot-check on a later list.
		if rerr := r.refreshIndexProjection(r.ownerOfSlug(slug), slug); rerr != nil {
			r.repoLog().Printf("shale: index refresh after pin %s: %v (a later list corrects it)", slug, rerr)
		}
	}
	return err
}

// Unpin clears the pin and rolls the head to the latest LIVE version, the same
// rule latestActiveVerNum and the read path apply. Tombstoned versions are
// skipped: pointing the head at one would serve bytes the owner deleted, or
// 404 once the GC reclaims them. ErrNotFound when no live version remains.
//
// The version scan runs outside the transaction (ScanPrefix is unsupported
// inside a CAS tx), so the candidate NEXT version key is read ExpectAbsent
// inside the tx. An append that lands after the scan is then either visible to
// that read or fails the read-check, and both routes abort rather than commit a
// head chosen from a stale version set.
func (r *ShaleRepo) Unpin(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	versions, err := r.scanVersions(slug)
	if err != nil {
		return err
	}
	var latest *versionRow
	for i := range versions {
		if versions[i].Deleted {
			continue
		}
		if latest == nil || versions[i].VerNum > latest.VerNum {
			latest = &versions[i]
		}
	}
	if latest == nil {
		return ErrNotFound
	}
	// MAX(ver_num) counts tombstones, so this is the number a racing
	// append would claim next.
	nextVerKey := shaleKeyVersion(slug, maxVerNum(versions)+1)

	txErr := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		if _, gerr := tx.Get(nextVerKey); gerr == nil {
			return errVerTaken
		} else if !errors.Is(gerr, backend.ErrNotFound) {
			return gerr
		}
		p.PinnedVersion = 0
		p.contentRef = latest.contentRef // whole served descriptor rolls to the latest live version
		return shaleTxPutJSON(tx, pasteKey, p)
	})
	if errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict) {
		return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
	}
	if txErr == nil {
		// The listing renders the pin from the cached entry, so it has to move
		// with the head. Best-effort: a lost refresh is corrected by the
		// spot-check on a later list.
		if err := r.refreshIndexProjection(r.ownerOfSlug(slug), slug); err != nil {
			r.repoLog().Printf("shale: index refresh after unpin %s: %v (a later list corrects it)", slug, err)
		}
	}
	return txErr
}

// --- SweepRepo -------------------------------------------------------------
