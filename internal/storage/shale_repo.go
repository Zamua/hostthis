// Package storage's shale-backed metadata implementation.
//
// ShaleRepo talks to a shale cluster.Cluster, which routes each key to the node
// owning its shard. The storage engine beneath the cluster is a build-time
// choice (shale_backing.go); nothing here names one.
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

	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/cluster"
	"github.com/Zamua/shale/pkg/coord"
	"github.com/Zamua/shale/pkg/coord/cas"
	"github.com/Zamua/shale/pkg/rpc"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
)

// CoordinatorCAS selects the CAS/lease coordination adapter for
// ShaleConfig.Coordinator: membership as one document in the ConditionalStore.
// It is the only clustered adapter; the empty string is single-node.
const CoordinatorCAS = "cas"

// ShaleConfig captures the parameters needed to open a shale cluster over the
// slate backend. The S3 connection fields mirror SlateConfig (same underlying
// SlateDB-on-object-storage engine); NodeID, the peer-discovery fields, and the
// consistency knobs are the cluster-layer additions.
//
// Coordinator selects the topology: empty is single-node (every op local, no
// ring routing), CoordinatorCAS is multi-node over the membership document,
// with gRPC forwarding and ring routing. ShardKeyFn and the per-family
// co-location are identical at every node count.
type ShaleConfig struct {
	NodeID    string // stable node identity; required by cluster.Open
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false -> slate sets AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files

	// LocalDir is the on-disk location for a build whose engine is LOCAL (see
	// shale_backing.go). Empty means keep the store in memory, which is what a
	// test wants. The object-store engine ignores it: its location is the
	// bucket plus DbName above.
	LocalDir string

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

	// GRPCAddr is this node's gRPC forwarding address, broadcast to peers as
	// their forwarding target. Required in multi-node mode (cluster.Open errors
	// otherwise). Ignored in single-node mode.
	GRPCAddr string

	// Coordinator selects the coordination adapter. Empty is single-node.
	// CoordinatorCAS coordinates through one membership document in
	// ConditionalStore (required then); GRPCAddr is still required, because
	// peer forwarding is transport, not coordination. Operator env:
	// HOSTTHIS_SHALE_COORDINATOR (docs/SPEC.md "Coordination is a pluggable
	// choice").
	Coordinator string

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

	// TombstoneGrace is forwarded to cluster.Config.TombstoneGracePeriod: at
	// R>1, tombstones older than this are purged natively when a replica
	// position mounts, so the backend's compaction can reclaim them. Zero
	// disables the purge. Eligibility (R>1, write-ack bar covering all
	// replicas) is decided inside shale. Operator env:
	// HOSTTHIS_METADATA_TOMBSTONE_GRACE (docs/SPEC.md "Tombstone purge:
	// reclaiming replicated deletes").
	TombstoneGrace time.Duration

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
// service-layer interfaces. Every operation goes through the
// cluster handle, which routes per-shard via shaleShardKey and commits
// single-shard writes through per-shard CAS.
//
// In multi-node mode (a cas Coordinator) the repo ALSO
// owns the process-level gRPC peer-forwarding server: cluster.Open advertises
// the node's GRPCAddr via the coordinator but does not stand up the listener
// peers forward routed reads/writes/migrations to (docs/SPEC.md "Peer
// forwarding"). NewShaleRepo binds that listener and serves the cluster's rpc
// handlers on it.
type ShaleRepo struct {
	cluster *cluster.Cluster

	// nowForTest overrides the heal's clock (DropStaleOwnerEntry's age guard);
	// nil in production means time.Now.
	nowForTest func() time.Time

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

	// intents records in-flight multi-shard writes so a process death cannot
	// lose the knowledge that cleanup is owed. It is a durable.Log, never a
	// concrete store: which mechanism provides durability is not this type's
	// business (docs/SPEC.md "The durability mechanism is a port, not a layer").
	// Defaults to the metadata cluster's own implementation.
	intents durable.Log

	// grpcAddr is the ACTUAL bound forwarding address advertised to peers
	// grpcAddr is the address peers dial for forwarding: what the listener
	// actually bound (lis.Addr().String()), or "" in single-node mode.
	grpcAddr string

	nodeID string

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

	// backing is the opened storage engine (shale_backing.go). The cluster owns
	// the unit databases it mounted; this holds whatever sits UNDER them, and
	// Close releases it once they have shut down.
	backing *backing

	// Test seams (nil in production; set only through the _test exports). The
	// repair paths' race windows are microseconds wide, so the tests that pin
	// the guarded index writes inject a concurrent operation at the exact point
	// the window opens.
	//
	// testHookGuardedIndexWrite runs at the top of every guarded index write; a
	// non-nil return fails that write. Fault injection for the Policy-1 pin:
	// one entry's write failure must not stall the rest of the reprojection.
	testHookGuardedIndexWrite func(key []byte) error

	// testHookVersionScan runs at the top of scanVersions, so a test can count
	// version-family scans and prove the append fast path performs none when the
	// cache is present.
	testHookVersionScan func(slug domain.Slug)
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
	// Bucket is NOT checked here: it is an object-store field, and whether one
	// is required is the engine's business (shale_backing.go).
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ShaleConfig.NodeID required")
	}
	casCoord := cfg.Coordinator == CoordinatorCAS
	if cfg.Coordinator != "" && !casCoord {
		return nil, fmt.Errorf("shale: unknown Coordinator %q (want empty or %q)", cfg.Coordinator, CoordinatorCAS)
	}
	if casCoord {
		if cfg.ConditionalStore == nil {
			return nil, fmt.Errorf("shale: Coordinator %q requires a ConditionalStore (the membership document's store)", CoordinatorCAS)
		}
	}
	multiNode := casCoord

	// Bind before opening the cluster so the advertised GRPCAddr is the address
	// actually served (resolving the ":0" / OS-assigned-port case).
	var lis net.Listener
	advertiseGRPCAddr := cfg.GRPCAddr
	if multiNode {
		if cfg.GRPCAddr == "" {
			return nil, fmt.Errorf("shale: GRPCAddr required in multi-node mode (a cas Coordinator set)")
		}
		l, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			return nil, fmt.Errorf("shale: bind gRPC listener on %q: %w", cfg.GRPCAddr, err)
		}
		lis = l
		advertiseGRPCAddr = l.Addr().String()
	}

	// The storage engine, chosen at build time (shale_backing.go). Opened
	// before the cluster because the cluster mounts it.
	bk, err := openBacking(cfg)
	if err != nil {
		if lis != nil {
			_ = lis.Close()
		}
		return nil, err
	}

	cleanup := func() {
		_ = bk.releaseAll()
		if lis != nil {
			_ = lis.Close()
		}
	}

	// Coordination adapter: shale exposes membership behind a Coordinator port.
	// A nil Coordinator IS the single-node path, so it needs no special case
	// below. cas keeps one membership document in the ConditionalStore, no mesh
	// and no extra port; the adapter's default key under the MinIO store's KeyPrefix (the
	// DB name) places that document at <dbName>/__coord/members, which
	// isolates clusters sharing a bucket and collides with nothing (unit data
	// lives under <dbName>u<N>/... prefixes). Constructed unstarted either
	// way: cluster.Open starts it and owns its lifecycle.
	var coordinator coord.Coordinator
	switch {
	case casCoord:
		coordinator = cas.New(cas.Config{Store: cfg.ConditionalStore})
	}

	clusterCfg := cluster.Config{
		NodeID:            cfg.NodeID,
		Coordinator:       coordinator,
		GRPCAddr:          advertiseGRPCAddr,
		ReplicationFactor: cfg.ReplicationFactor,
		// Zero leaves cluster.Open's 5s per-dispatch deadline.
		WriteTimeout: cfg.WriteTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		// Zero disables the mount-time tombstone purge.
		TombstoneGracePeriod: cfg.TombstoneGrace,
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
	// Cold-start patience: a joiner re-reads the membership document for the
	// cluster generation up to GenLearnBudget (shale default 180s), so a
	// still-mounting founder is waited for instead of crash-looping the joiner. Raise it for a
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

	bk.applyTo(&clusterCfg)

	// Declarative resharding needs BOTH a multi-backend cluster (nothing to
	// reshard otherwise) and a shared CAS arbiter to coordinate the generation
	// advance. With both, the cluster drives the reshard target from the
	// unanimously declared count, so redeploying every pod with a
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
			cleanup()
			return nil, fmt.Errorf("shale: open blob cluster: %w", berr)
		}
		kv = bkv
		cl = bkv.Cluster()
	} else {
		c, oerr := cluster.Open(clusterCfg)
		if oerr != nil {
			cleanup()
			return nil, fmt.Errorf("shale: open cluster: %w", oerr)
		}
		cl = c
	}

	r := &ShaleRepo{
		cluster:  cl,
		intents:  NewShaleIntentLog(cl, cfg.Logger),
		kv:       kv,
		logger:   cfg.Logger,
		grpcAddr: advertiseGRPCAddr,
		nodeID:   cfg.NodeID,
		backing:  bk,
	}

	// Stand up the peer-forwarding server the cluster advertised but
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
	// Only after the cluster closed its mounted units: they read through the
	// engine resources this releases, so a live unit must never outlive them.
	if berr := r.backing.releaseUnowned(); berr != nil && err == nil {
		err = berr
	}
	return err
}

// GRPCAddr returns this node's ACTUAL bound gRPC forwarding address, the one
// advertised to peers, or "" in single-node mode. The configured GRPCAddr may
// have been ":0", so the served port is only knowable post-bind.
func (r *ShaleRepo) GRPCAddr() string { return r.grpcAddr }

// PeerGRPCAddrs returns the CURRENT gRPC addresses of every OTHER live cluster
// member, read from the coordinator's ring membership. These are the addresses
// peers advertised at join (exactly what shale forwarding dials), kept current
// by the same coordination that tracks joins, leaves and deploy churn, so the
// view is fresher than DNS. The composition root adapts it onto the relay's Peers port
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
	// Settle this owner's half-finished writes FIRST, so the listing below does
	// not render a phantom the very next scan would have removed. Best-effort
	// and usually a no-op (docs/SPEC.md "Durable intent").
	r.resolveOwnerIntents(context.Background(), owner, time.Now().UTC())

	// Doc-first: one point Get renders the whole list. The legacy scan below
	// serves only pre-migration owners, READ-ONLY (it never writes the doc).
	if doc, err := r.getOwnerDoc(owner); err != nil {
		return nil, err
	} else if doc != nil {
		return doc.listRows(owner), nil
	}

	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return nil, err
	}

	out := make([]domain.Paste, 0, len(idx))
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var e identityPasteRow
		if err := json.Unmarshal(item.Value, &e); err == nil && e.renderable() {
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
	fresh, ok, err := r.freshRowFromHead(slug)
	if err != nil || !ok {
		return domain.Paste{}, false, err
	}
	if _, werr := r.guardedPutIndexEntry(item.Key, item.Value, true, fresh, nil); werr != nil {
		r.repoLog().Printf("shale: index upgrade for %s: %v (retried on a later list)", slug, werr)
	}
	return fresh.toDomain(slug, owner), true, nil
}

// freshRowFromHead derives the full projection row from the authoritative
// {slug} rows: the read-through for an entry that cannot render from its
// cached fields, shared by the list-walk upgrade and the owner-doc heal.
// ok=false when the paste is gone or failed, so the entry renders nothing.
func (r *ShaleRepo) freshRowFromHead(slug domain.Slug) (identityPasteRow, bool, error) {
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		if errors.Is(err, ErrNotFound) {
			return identityPasteRow{}, false, nil
		}
		return identityPasteRow{}, false, err
	}
	if domain.NormalizeStatus(head.Status) == domain.PasteStatusFailed {
		return identityPasteRow{}, false, nil // its bytes never landed
	}

	live, latest := head.LiveBytes, head.LatestVersion
	if latest == 0 {
		// Predates the head totals: derive them once. The caller's rewrite
		// stamps them, so this never repeats for this slug.
		versions, err := r.scanVersions(slug)
		if err != nil {
			return identityPasteRow{}, false, err
		}
		live, latest = int(liveVersionBytes(versions)), latestActiveVerNum(versions)
	}

	return identityPasteRow{
		Name: head.Name, Size: live, ServedSize: head.Size, CreatedAt: head.CreatedAt,
		Kind: head.Kind, LatestVersion: latest,
		PinnedVersion: head.PinnedVersion, UpdatedAt: head.UpdatedAt,
	}, true, nil
}

func (r *ShaleRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	// A cheap read: serve from the disposable cache, falling back to the rows
	// when it is absent or unreadable (docs/SPEC.md "The version index cache").
	versions, err := r.versionRowsForRead(slug)
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

// IsVersionServed reports whether ver is the version the paste's URL currently
// serves. It backs the per-version delete guard, so it decides from the
// authoritative HEAD and version ROWS and NEVER the disposable cache: a stale
// cache must not be able to let the guard free the served version's blob
// (docs/SPEC.md "Destructive operations decide from the rows and the head").
//
// Pinned: the pinned number, from the head alone. Unpinned: the head serves the
// newest live version, named EXACTLY by its blob id on the blob path (a point
// read of the target row, no scan) and, on the sha-keyed dev path that has no
// blob id, by the newest non-deleted ver_num from a rows scan (a content sha is
// not unique per version, so it cannot identify one).
func (r *ShaleRepo) IsVersionServed(slug domain.Slug, ver int) (bool, error) {
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		return false, err
	}
	if head.PinnedVersion != 0 {
		return ver == head.PinnedVersion, nil
	}
	if head.BlobID != "" {
		var v versionRow
		if err := r.getJSON(shaleKeyVersion(slug, ver), &v); err != nil {
			return false, err
		}
		return v.BlobID == head.BlobID, nil
	}
	versions, err := r.scanVersions(slug)
	if err != nil {
		return false, err
	}
	return ver == newestLiveVerNum(versions), nil
}

// newestLiveVerNum returns the highest non-deleted ver_num, or 0 when none are
// live. Distinct from latestActiveVerNum, which floors at 1.
func newestLiveVerNum(versions []versionRow) int {
	n := 0
	for _, v := range versions {
		if !v.Deleted && v.VerNum > n {
			n = v.VerNum
		}
	}
	return n
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
	if doc, err := r.getOwnerDoc(owner); err != nil {
		return 0, err
	} else if doc != nil {
		return len(doc.Pastes), nil
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
	// Doc-first: the quota figure is one point Get. The legacy scan serves
	// only pre-migration owners.
	if doc, err := r.getOwnerDoc(owner); err != nil {
		return 0, err
	} else if doc != nil {
		return int(doc.summary().PasteBytes), nil
	}
	total, err := r.sumActiveBytesForOwner(owner)
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
// Fail OPEN: an entry that cannot be read - undecodable, or carrying the
// Placeholder marker for an undecodable authoritative record - counts as ZERO
// and the scan continues (docs/SPEC.md "Unreadable entries fail OPEN"). The
// owner is UNDER-charged for exactly those bytes and keeps working, rather than
// being locked out of uploading by damage they did not cause. Skips are
// summarised in one log line per scan. A LEGACY entry, recognized by shape (an
// EMPTY value, the bare-marker convention an in-place migration carries over),
// is read through its authoritative rows until their next list enriches it.
func (r *ShaleRepo) sumActiveBytesForOwner(owner string) (int64, error) {
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	var skips scanSkips
	for _, item := range idx {
		if len(item.Value) == 0 {
			n, err := r.legacyPasteEntryBytes(item.Key)
			if err != nil {
				skips.add(item.Key, err)
				continue
			}
			total += n
			continue
		}
		var row identityPasteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			skips.add(item.Key, err)
			continue
		}
		if row.Placeholder {
			skips.add(item.Key, errUnreadableRecord)
			continue
		}
		total += int64(row.Size)
	}
	skips.report(r, owner, "paste")
	return total, nil
}

// legacyPasteEntryBytes resolves a LEGACY (empty-valued) identity_pastes entry
// through its authoritative rows, the read-through a migrated deployment needs
// until the owner's next list enriches the entry. An empty value is the only legacy
// paste shape (unlike sites, pastes have no marker-byte era). A stale entry
// whose row is gone contributes zero; an undecodable row is reported to the
// caller, which counts it as zero and keeps going (docs/SPEC.md "Unreadable
// entries fail OPEN"); a live row contributes its live version sum.
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

// sumLiveVersionBytes sums the sizes of a paste's non-deleted versions, the
// legacy quota fall-through the head's cached LiveBytes does not already answer.
// A cheap read: it prefers the disposable cache and falls back to the rows
// (docs/SPEC.md "The version index cache"). A stale cache only slightly
// over/under-counts, the same bounded drift the scan-derived quota already
// tolerates.
func (r *ShaleRepo) sumLiveVersionBytes(slug domain.Slug) (int64, error) {
	versions, err := r.versionRowsForRead(slug)
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
// against the cap before an authoritative write. One family holds every kind,
// so it is a single-shard scan of cached enumeration-entry values with no
// per-entry fan-out.
func (r *ShaleRepo) combinedActiveBytes(owner string, now time.Time) (int64, error) {
	used, err := r.SumActiveBytesByOwner(owner, now)
	return int64(used), err
}

func (r *ShaleRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	if owner == "" {
		return time.Time{}, nil
	}
	if doc, err := r.getOwnerDoc(owner); err != nil {
		return time.Time{}, err
	} else if doc != nil {
		return doc.FirstSeen, nil
	}
	return r.legacyOwnerFirstSeen(owner)
}

// legacyOwnerFirstSeen reads the identity_first_seen key, the pre-doc
// representation the heal seeds the doc's FirstSeen from.
func (r *ShaleRepo) legacyOwnerFirstSeen(owner string) (time.Time, error) {
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
	if perr != nil {
		if errors.Is(perr, ErrNotFound) {
			return "", ErrNotFound
		}
		return "", perr
	}
	// The head answers the SERVED sha with no version lookup at all - the common
	// read for an HTML/markdown paste. This never consults the cache, so serving
	// is untouched.
	if p.ContentSHA == contentSHA && p.BlobID != "" {
		return p.BlobID, nil
	}
	// A NON-served version's sha (a pinned or Show'd version, or a file inside a
	// directory). A cheap read: try the disposable cache first, and on a per-sha
	// MISS fall back to the authoritative rows - a stale cache lacking the
	// version costs one extra scan here, never a wrong not-found (docs/SPEC.md
	// "The version index cache").
	resolve := func(versions []versionRow) string {
		for _, v := range versions {
			if v.ContentSHA == contentSHA && v.BlobID != "" {
				return v.BlobID
			}
		}
		// A file INSIDE a directory: its sha names a manifest entry rather than a
		// version's served content, so the entry carries its own blob id. The
		// head first (the common read), then any other live version, so a pinned
		// or rolled-back directory still resolves its files.
		if id := manifestBlobID(p.decode(), contentSHA); id != "" {
			return id
		}
		for _, v := range versions {
			if id := manifestBlobID(v.decode(), contentSHA); id != "" {
				return id
			}
		}
		return ""
	}
	if cached, ok := r.cachedVersionRows(slug); ok {
		if id := resolve(cached); id != "" {
			return id, nil
		}
	}
	versions, verr := r.scanVersions(slug)
	if verr != nil {
		return "", verr
	}
	if id := resolve(versions); id != "" {
		return id, nil
	}
	return "", ErrNotFound
}

// manifestBlobID returns the blob id the manifest records for contentSHA, or ""
// when no entry names it.
func manifestBlobID(m domain.Manifest, contentSHA string) string {
	for _, e := range m.Files {
		if e.SHA == contentSHA && e.BlobID != "" {
			return e.BlobID
		}
	}
	return ""
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
	return r.insertArtifact(ctx, p, userCap, now)
}

func (r *ShaleRepo) insertArtifact(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
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

	// T0: record the intent BEFORE touching anything. Written first so its
	// ABSENCE is unambiguous - no intent means nothing was attempted. That is
	// what lets a later sweep act at all (docs/SPEC.md "Durable intent").
	intent := durable.Intent{
		ID: durable.ID(p.Slug.String()), Kind: durable.KindCreatePaste,
		Scope: durable.Scope(p.Identity.String()), Subject: p.Slug.String(),
		StartedAt: now,
	}
	if err := r.intents.Begin(ctx, intent); err != nil {
		return fmt.Errorf("durable intent: %w", err)
	}

	// ENTRY FIRST, then the authoritative row. The two live on different shards
	// and a transaction touches one, so whichever is written second can be lost
	// to a crash. Written this way the survivor is an entry with no row - the
	// direction that over-counts rather than breaching the cap, and the one a
	// sweep can find from the owner's own shard.
	entryKey := shaleKeyIdentityPaste(p.Identity.String(), p.Slug.String())
	if err := r.confirmInsert(p); err != nil {
		return fmt.Errorf("enumeration entry: %w", err)
	}
	// The guard is the entry exactly as written, so a rollback can tell it apart
	// from a re-upload that landed after a crash.
	guard, gerr := r.getRaw(entryKey)
	if gerr != nil {
		r.repoLog().Printf("shale: reading the intent guard for %s: %v (a rollback will skip rather than risk a fresher entry)", p.Slug, gerr)
	}
	intent.Guard = guard
	if err := r.intents.Begin(ctx, withStep(intent, StepEntryWritten)); err != nil {
		r.repoLog().Printf("shale: recording the entry step for %s: %v (a sweep will still roll back, using the row check alone)", p.Slug, err)
	}

	if err := r.insertAuthoritative(p, binds, blobOwnerEpochFromContext(ctx)); err != nil {
		// Whose row now holds the slug decides whether the entry above is an
		// orphan at all. Both callers of a concurrent insert write that ONE
		// entry key, so the loser's guard matches what the winner left and the
		// rollback below would delete an entry the winner's row depends on -
		// stranding a paste that serves every file and reports its
		// versions while being absent from its owner's listing and free.
		switch owner, oerr := r.pasteOwner(p.Slug); {
		case oerr != nil:
			// Unreadable is not absent. Deleting on a guess risks exactly the
			// stranding above; keeping it over-counts, which is the direction
			// this path already prefers, and the intent left behind is what
			// lets a sweep settle it.
			r.repoLog().Printf("shale: resolving the owner of %s after a failed insert: %v (keeping the enumeration entry for a sweep)", p.Slug, oerr)
			return err
		case owner == p.Identity:
			// A concurrent caller wrote the SAME paste. The entry describes
			// it correctly, so it stays and the intent is discharged.
			if cerr := r.intents.Complete(ctx, intent.ID, intent.Scope); cerr != nil {
				r.repoLog().Printf("shale: forgetting the intent for %s: %v (a sweep will retry it)", p.Slug, cerr)
			}
			return err
		}
		// A different identity holds the slug, or nothing does: the entry
		// written above is an orphan. Best-effort removal keeps the common
		// failure (a slug race) from charging the owner; if it fails, the
		// intent stays and a sweep finishes the job. The doc entry drops in
		// the same CAS: doc-first reads have no pruning path, so a doc
		// phantom would over-count this owner permanently.
		if _, derr := r.guardedDropOwnerEntry(p.Identity.String(), p.Slug.String(), guard); derr != nil {
			r.repoLog().Printf("shale: rollback of enumeration entry for %s: %v (a sweep will retry it)", p.Slug, derr)
		} else if cerr := r.intents.Complete(ctx, intent.ID, intent.Scope); cerr != nil {
			r.repoLog().Printf("shale: forgetting the intent for %s: %v (a sweep will retry it)", p.Slug, cerr)
		}
		return err
	}

	// T3: the write is durable, so the intent has nothing left to protect.
	// Losing this leaves an intent whose row EXISTS, which a sweep rolls
	// forward - it never deletes a live paste.
	if err := r.intents.Complete(ctx, intent.ID, intent.Scope); err != nil {
		r.repoLog().Printf("shale: forgetting the intent for %s: %v (a sweep rolls it forward)", p.Slug, err)
	}
	return nil
}

// withStep returns the intent with step recorded, so Begin can re-write it
// without a separate Advance round trip on the response path.
func withStep(in durable.Intent, step durable.StepName) durable.Intent {
	if in.HasReached(step) {
		return in
	}
	out := in
	out.Reached = append(append([]durable.StepName(nil), in.Reached...), step)
	return out
}

// slugTaken reports whether slug already names a paste. The key co-shards
// with the slug, so this is one shard's read.
func (r *ShaleRepo) slugTaken(slug domain.Slug) (bool, error) {
	for _, key := range [][]byte{shaleKeyPaste(slug)} {
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
// bindFence is what runAuthoritative needs to verify the caller still owns the
// bytes it is about to bind. The zero value skips the check, which is every
// path that stages no blobs.
type bindFence struct {
	slug  domain.Slug
	epoch int64
}

func (r *ShaleRepo) runAuthoritative(pinKey []byte, refs []cluster.BlobRef, fence bindFence, body func(tx shaleKVTx, bind func() error) error) error {
	if len(refs) == 0 || r.kv == nil {
		return translateCrossShard(r.cluster.Transact(pinKey, func(tx backend.Transaction) error {
			return body(tx, func() error { return nil })
		}))
	}
	return translateCrossShard(r.kv.Transact(pinKey, func(tx *cluster.BlobTx) error {
		return body(tx, func() error {
			// The fence, checked INSIDE the binding transaction and before the
			// first bind. Recovery bumps the epoch before it unstages, so a
			// writer it took over aborts here instead of binding bytes that are
			// already gone (docs/SPEC.md "Fencing the writer recovery took over
			// from"). The read joins this transaction's read-set, so a fence
			// landing mid-commit conflicts rather than racing.
			//
			// It lives here, at the single point every bind passes through,
			// rather than in each caller: a per-caller check is one a new write
			// path silently omits, and the omission has no symptom until a
			// recovery races a resumed writer.
			if err := checkBlobOwnership(tx, fence.slug, fence.epoch); err != nil {
				return err
			}
			for _, ref := range refs {
				if err := tx.BindBlob(ref); err != nil {
					return err
				}
			}
			return nil
		})
	}))
}

// insertAuthoritative writes the {slug}-shard rows in one CAS transaction: the
// paste row, the v1 version row and slug_owner. The slug-collision check is
// part of the transaction's read-set, so a racing insert of the same slug
// conflicts.
//
// A staged blob in refs is BOUND in this same transaction, so the pointer
// co-commits with the row and the blob id lands on both the head and the v1
// version row.
func (r *ShaleRepo) insertAuthoritative(p domain.Paste, refs []cluster.BlobRef, epoch int64) error {
	pasteKey := shaleKeyPaste(p.Slug)
	blobID := firstBlobID(refs)
	return r.runAuthoritative(pasteKey, refs, bindFence{slug: p.Slug, epoch: epoch}, func(tx shaleKVTx, bind func() error) error {
		// This Get is also the ExpectAbsent read-check that makes a concurrent
		// insert of the same slug conflict.
		if _, err := tx.Get(pasteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("slug check: %w", err)
		}
		stamped := p
		stamped.Manifest = stampManifestBlobIDs(p.Manifest, refs)
		v1Ref := contentRefFromDomain(stamped)
		v1Ref.BlobID = servedBlobID(stamped, blobID)
		v1 := newVersionRow(1, v1Ref, p.CreatedAt)
		pr := pasteFromDomain(p)
		// The head serves v1, so it takes v1's descriptor WHOLE - the same roll
		// an append and a pin perform. Building the head's copy separately is
		// how the two drift.
		pr.contentRef = v1.contentRef
		// v1 is the only version at insert, so the totals are known exactly.
		pr.LiveBytes = p.Size
		pr.LatestVersion = 1
		if err := shaleTxPutJSON(tx, pasteKey, pr); err != nil {
			return err
		}
		if err := shaleTxPutJSON(tx, shaleKeyVersion(p.Slug, 1), v1); err != nil {
			return err
		}
		if err := tx.Put(shaleKeySlugOwner(p.Slug), []byte(p.Identity.String())); err != nil {
			return err
		}
		// Seed the disposable version cache with v1, in this same {slug}
		// transaction (docs/SPEC.md "The version index cache").
		if err := txPutVersionsDoc(tx, p.Slug, []versionRow{v1}); err != nil {
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

// servedBlobID is the blob the flat descriptor points at: the ROOT entry's,
// when a manifest says which one that is.
//
// A directory stages many blobs, and the first of them is an arbitrary file -
// pairing it with the root's ContentSHA would resolve the root to some other
// file's bytes. Falls back to the supplied id for a paste with no manifest,
// where there is only one blob to mean.
func servedBlobID(p domain.Paste, fallback string) string {
	if e, ok := p.Manifest.Files[domain.Root]; ok && e.BlobID != "" {
		return e.BlobID
	}
	if e, ok := p.Manifest.Lookup("/"); ok && e.BlobID != "" {
		return e.BlobID
	}
	return fallback
}

// stampManifestBlobIDs copies each staged ref's blob id onto the manifest entry
// naming the same sha, so a manifest resolves its own files with no side-table.
//
// A sha with no staged ref keeps whatever it already carried: a redeploy only
// stages the files that CHANGED, and the unchanged ones must keep pointing at
// the blobs a previous deploy bound.
func stampManifestBlobIDs(m domain.Manifest, refs []cluster.BlobRef) domain.Manifest {
	if len(m.Files) == 0 || len(refs) == 0 {
		return m
	}
	byID := make(map[string]string, len(refs))
	for _, ref := range refs {
		if ref.ContentHash != "" && ref.BlobID != "" {
			byID[ref.ContentHash] = ref.BlobID
		}
	}
	out := domain.NewManifest()
	for path, e := range m.Files {
		if id, ok := byID[e.SHA]; ok {
			e.BlobID = id
		}
		out.Add(path, e)
	}
	return out
}

// confirmInsert writes the identity_pastes index entry, sets
// identity_first_seen if absent, and updates the owner doc, on the {id} shard
// in one CAS. The entry is what SumActiveBytesByOwner sums (its cached size
// seeds at v1's size, the paste's whole live sum at insert) and how
// ListByOwner / CountByOwner enumerate. Idempotent: a re-run overwrites the
// same entry and leaves an already-set first-seen untouched.
func (r *ShaleRepo) confirmInsert(p domain.Paste) error {
	identity := p.Identity.String()
	slug := p.Slug.String()
	indexKey := shaleKeyIdentityPaste(identity, slug)
	firstSeenKey := shaleKeyIdentityFirstSeen(identity)
	entry := identityPasteRow{
		Name:          p.Name,
		Size:          p.Size,
		ServedSize:    p.Size,
		CreatedAt:     p.CreatedAt,
		Kind:          string(p.Kind),
		LatestVersion: 1,
		PinnedVersion: p.PinnedVersion,
		UpdatedAt:     p.UpdatedAt,
	}
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return err
	}
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if err := shaleTxPutJSON(tx, indexKey, entry); err != nil {
			return err
		}
		// Write-if-absent keeps this a MIN(created_at).
		if _, err := tx.Get(firstSeenKey); errors.Is(err, backend.ErrNotFound) {
			if perr := tx.Put(firstSeenKey, []byte(p.CreatedAt.UTC().Format(time.RFC3339Nano))); perr != nil {
				return perr
			}
		} else if err != nil {
			return err
		}
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			doc.Pastes[slug] = ownerDocPasteFromRow(entry)
			// Set-if-zero preserves the MIN semantics: an existing owner's
			// first-seen arrives via the doc or the heal seed.
			if doc.FirstSeen.IsZero() {
				doc.FirstSeen = p.CreatedAt.UTC()
			}
		})
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
	var createdAt time.Time
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
		createdAt = p.CreatedAt
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
	// Step 2: drop the enumeration entry and the doc entry on the {id} shard so
	// the failed paste leaves ListByOwner and stops being enumerated at all.
	// Guarded by the failed paste's CreatedAt so a concurrent re-mint of the
	// slug keeps its own fresh entry.
	return r.dropOwnerEntry(identity, slug.String(), createdAt)
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
	return r.appendVersion(ctx, slug,
		contentRef{Kind: string(kind), ContentSHA: contentSHA, Size: size}, userCap, now)
}

// AppendManifestVersion appends a version whose content is a whole manifest -
// the multi-entry shape a directory redeploy writes. Otherwise identical to
// AppendVersionWithQuotaCheck: same quota check, same head roll, same
// single-shard boundary.
//
// root describes the entry a listing shows; size is the version's CHARGED
// bytes, which for a directory is its deduped blob total rather than the root
// file's size.
func (r *ShaleRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, m domain.Manifest, root domain.ManifestEntry, size int, userCap int64, now time.Time) (AppendResult, error) {
	ref := contentRef{Kind: string(domain.KindSite), ContentSHA: root.SHA, Size: size}
	enc, err := encodeManifest(stampManifestBlobIDs(m, pendingBindsFromContext(ctx)))
	if err != nil {
		return AppendResult{}, fmt.Errorf("encode manifest: %w", err)
	}
	ref.Manifest = enc
	return r.appendVersion(ctx, slug, ref, userCap, now)
}

func (r *ShaleRepo) appendVersion(ctx context.Context, slug domain.Slug, ref contentRef, userCap int64, now time.Time) (AppendResult, error) {
	// Read the staged refs once and pass them down, isolating this append from
	// any concurrent same-slug write.
	binds := pendingBindsFromContext(ctx)

	var existing pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	identity := existing.Identity
	body := int64(ref.Size)

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

	res, err := r.appendAuthoritative(slug, ref, now, binds, blobOwnerEpochFromContext(ctx))
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
// read time inside a {slug} transaction, so the mark the closure numbered from
// is stale-low. The closure aborts with it (Transact returns a non-conflict fn
// error verbatim). Never escapes the function that raises it.
var errVerTaken = errors.New("shale: candidate version number already taken")

// errNeedScan aborts an append attempt that found the version cache ABSENT (a
// pre-cache paste). The fast path numbers from point reads, but a cache-absent
// paste needs a rows scan both to number safely (a pre-LatestVersion head mark
// reads zero) and to rebuild the cache, and ScanPrefix cannot run inside a CAS
// tx. The caller scans OUTSIDE the tx and retries. Never escapes the function.
var errNeedScan = errors.New("shale: append needs a version scan to migrate a pre-cache paste")

// ErrConcurrentChange aliases the domain-owned sentinel, matching how the rest
// of the storage error vocabulary is re-exported.
var ErrConcurrentChange = domain.ErrConcurrentChange

// errIdentityChanged aborts an owner-gated {slug} write whose head no longer
// belongs to the paste the caller authorized: a delete+re-mint of the same slug
// committed in the window between the caller's ownership read and this
// transaction, so acting would touch a DIFFERENT paste (another owner's, or the
// same owner's new one). The in-tx head read joins the transaction's read-set,
// so a re-mint racing the commit conflicts instead of slipping through. Mapped
// to ErrConcurrentChange; never escapes the storage layer.
var errIdentityChanged = errors.New("shale: paste identity changed since the caller's read")

// pasteHeadIs reports whether a head row still describes the paste instance the
// caller authorized. Identity + CreatedAt together name one paste: CreatedAt is
// set once at insert and immutable, so a re-mint (necessarily a new insert) can
// never reproduce it, and Identity catches a re-mint by a different owner even
// in the impossible event the stamps collided.
func pasteHeadIs(head pasteRow, wantIdentity domain.Identity, wantCreatedAt time.Time) bool {
	return head.Identity == wantIdentity.String() && head.CreatedAt.Equal(wantCreatedAt)
}

// appendAuthoritative writes the new version row on the {slug} shard and numbers
// it from the head's monotonic high-water mark, NOT a version scan.
//
// Fast path (the cache is present): the candidate is
// max(head.LatestVersion, cache max)+1, computed from reads the transaction
// already performs. No scan. The ExpectAbsent read-check on the candidate key is
// the backstop: a stale-LOW mark, or a concurrent append that committed the
// number first, proposes a TAKEN number; the check rejects it (errVerTaken at
// read time, ErrCASConflict at commit time) and the retry scans to recover the
// true max and advance past it. A stale-HIGH mark only skips numbers, and gaps
// are fine. So a version number is NEVER reused: not under a stale-low mark, not
// under concurrent appends, and not for a tombstoned number (a tombstone keeps
// its row and never lowers the mark, so the mark, the cache and the scan all
// still count it).
//
// Migration path (the cache is absent, a pre-cache paste): one scan OUTSIDE the
// tx recovers the true max (covering a pre-LatestVersion head whose mark reads
// zero) and rebuilds the cache. After it the paste is cache-present and its
// appends rejoin the fast path.
//
// The cache is maintained in the SAME {slug} transaction with no rescan: the new
// row is upserted onto the in-tx read of the present cache, or the rebuilt set
// is stamped on the migration path. The in-tx read records a read-check, so a
// concurrent append or tombstone conflicts and the committing attempt always
// upserts onto the fresh set. A stale cache is harmless regardless (docs/SPEC.md
// "The version index cache").
func (r *ShaleRepo) appendAuthoritative(slug domain.Slug, ref contentRef, now time.Time, refs []cluster.BlobRef, epoch int64) (AppendResult, error) {
	pasteKey := shaleKeyPaste(slug)
	// The blob id lands on the new version row and, when the head is unpinned
	// (so the public URL follows this version), on the paste head row too.
	blobID := firstBlobID(refs)
	const maxRenumberAttempts = 16

	// scanned holds the version rows on the migration and collision-recovery
	// attempts; the fast path never fills it. needScan latches once an attempt
	// aborts, so every following attempt re-scans FRESH (a stale scan would
	// re-propose a number a concurrent append already took).
	var scanned []versionRow
	var haveScan bool
	needScan := false

	for range maxRenumberAttempts {
		haveScan = false
		if needScan {
			vs, err := r.scanVersions(slug)
			if err != nil {
				return AppendResult{}, err
			}
			scanned, haveScan = vs, true
		}

		var newVer int
		var wasPinned bool
		txErr := r.runAuthoritative(pasteKey, refs, bindFence{slug: slug, epoch: epoch}, func(tx shaleKVTx, bind func() error) error {
			var p pasteRow
			if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
				return err
			}
			// Read the disposable cache in-tx: its read-check keeps the upsert
			// below on a fresh set, and its max lifts a pre-LatestVersion head.
			// Absent => a pre-cache paste a scan must migrate, signalled out
			// because ScanPrefix cannot run in a CAS tx.
			cachedRows, cachePresent, cerr := txReadVersionsDoc(tx, slug)
			if cerr != nil {
				return cerr
			}
			if !cachePresent && !haveScan {
				return errNeedScan
			}

			// Number from the monotonic high-water mark, lifted by the cache max
			// and, on a recovery attempt, the scanned true max. Never a live-only
			// figure, so a tombstoned number is never reused.
			floor := p.LatestVersion
			if cachePresent {
				floor = max(floor, maxVerNum(cachedRows))
			}
			if haveScan {
				floor = max(floor, maxVerNum(scanned))
			}
			newVer = floor + 1
			verKey := shaleKeyVersion(slug, newVer)

			// Reading the candidate key records an ExpectAbsent read-check, so a
			// concurrent commit of it after this read conflicts at Commit time.
			if _, gerr := tx.Get(verKey); gerr == nil {
				return errVerTaken
			} else if !errors.Is(gerr, backend.ErrNotFound) {
				return gerr
			}
			wasPinned = p.PinnedVersion != 0

			vRef := ref
			// The ROOT's blob, not an arbitrary one of the staged set: pairing
			// the root's ContentSHA with another file's blob resolves the root
			// to that file's bytes.
			vRef.BlobID = servedBlobID(domain.Paste{Manifest: ref.decode()}, blobID)
			newV := newVersionRow(newVer, vRef, now)
			if err := shaleTxPutJSON(tx, verKey, newV); err != nil {
				return err
			}
			p.UpdatedAt = now
			// The totals roll in THIS transaction, so they can never disagree
			// with the version rows they summarise. LatestVersion advances to
			// newVer, healing a stale-low mark the recovery scan corrected.
			p.LiveBytes += ref.Size
			p.LatestVersion = newVer
			if p.PinnedVersion == 0 {
				p.contentRef = newV.contentRef // unpinned head rolls to the new version, whole
			}
			if err := shaleTxPutJSON(tx, pasteKey, p); err != nil {
				return err
			}
			// Maintain the cache with no rescan: upsert onto the present set, or
			// stamp the rebuilt set from the migration scan.
			base := cachedRows
			if !cachePresent {
				base = scanned
			}
			doc := versionsDoc{Versions: append([]versionRow(nil), base...)}
			doc.upsert(newV)
			if err := txPutVersionsDoc(tx, slug, doc.Versions); err != nil {
				return err
			}
			return bind()
		})
		switch {
		case txErr == nil:
			return AppendResult{NewVer: newVer, WasPinned: wasPinned}, nil
		case errors.Is(txErr, errNeedScan) ||
			errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict):
			needScan = true // migrate or recover: re-scan FRESH next attempt
			continue
		default:
			return AppendResult{}, txErr
		}
	}
	return AppendResult{}, fmt.Errorf("shale: append %q: could not allocate a free version number after %d attempts", slug, maxRenumberAttempts)
}

// refreshIndexProjection updates the owner's identity_pastes projection and
// owner doc after a size-changing {slug} write: it recomputes the paste's
// live byte sum from the authoritative version rows into the cached size.
//
// One {id}-shard CAS, GUARDED: the entry's payload is captured BEFORE the
// recompute and the write commits only if the entry still holds it, so two
// concurrent same-slug refreshes cannot land older-sum-last. The loser SKIPS
// rather than clobbering the fresher sum, costing at most one cycle of
// staleness that the next list converges; there is no recompute retry on the
// response path (docs/SPEC.md "Scan-derived quota" / "Window C"). The doc
// update rides the SAME transaction, so entry and doc move together or not
// at all.
//
// A missing or LEGACY-empty entry is refreshed through the doc when the doc
// carries the slug (doc-first reads have no list-time enrichment, so nothing
// else would ever unfreeze that entry), and left for the list-time enrichment
// on a pre-doc owner. A Placeholder has no trustworthy fields to preserve, so
// it is left whole rather than part-patched.
func (r *ShaleRepo) refreshIndexProjection(identity string, slug domain.Slug) error {
	if identity == "" {
		return nil // paste gone mid-flight; the best-effort refresh has no subject
	}
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	expected, err := r.getRaw(indexKey)
	if err != nil {
		return err
	}
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return err
	}
	if len(expected) == 0 {
		return r.refreshFromEmptyEntry(identity, slug, candidate)
	}
	var row identityPasteRow
	if err := json.Unmarshal(expected, &row); err != nil {
		return fmt.Errorf("decode %s: %w", indexKey, err)
	}
	// A placeholder is NOT skipped: this path holds the head row, so if the
	// record decodes again the refresh below replaces the marker with real
	// values and the entry stops being under-charged.
	// Refresh from the head row, which carries the totals transactionally: one
	// routed read instead of a version-family scan.
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		return err
	}
	row.Size = head.LiveBytes
	row.ServedSize = head.Size
	row.Kind = head.Kind
	row.LatestVersion = head.LatestVersion
	row.PinnedVersion = head.PinnedVersion
	row.UpdatedAt = head.UpdatedAt
	written, err := r.guardedPutIndexEntry(indexKey, expected, true, row, func(tx backend.Transaction) error {
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			doc.Pastes[slug.String()] = ownerDocPasteFromRow(row)
		})
	})
	if err != nil {
		return err
	}
	if !written {
		r.repoLog().Printf("shale: index refresh %s skipped: entry changed during the recompute (a concurrent write landed; the next list converges)", indexKey)
	}
	return nil
}

// refreshFromEmptyEntry refreshes a slug whose legacy entry is absent or the
// old empty-marker shape. A pre-doc owner is left for the list-time
// enrichment; for a doc-present owner that enrichment never runs (lists read
// the doc), so the doc entry is refreshed from the head row HERE and a real
// legacy row is written alongside (the dual-write). The doc mutation UPDATES,
// never creates: entry-absence gives the guard nothing to compare, so a
// racing whole-paste delete must win by the slug simply being gone from the
// doc when the mutation runs.
func (r *ShaleRepo) refreshFromEmptyEntry(identity string, slug domain.Slug, candidate *ownerDoc) error {
	doc, err := r.getOwnerDoc(identity)
	if err != nil {
		return err
	}
	if doc == nil {
		return nil // pre-doc owner; the next list enriches the legacy entry
	}
	if _, ok := doc.Pastes[slug.String()]; !ok {
		return nil // neither representation carries it; a refresh never creates
	}
	fresh, ok, err := r.freshRowFromHead(slug)
	if err != nil {
		return err
	}
	if !ok {
		return nil // gone or failed; the delete / fail paths own removal
	}
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	post := func(tx backend.Transaction) error {
		return r.txApplyOwnerDoc(tx, identity, candidate, func(d *ownerDoc) {
			if _, ok := d.Pastes[slug.String()]; ok {
				d.Pastes[slug.String()] = ownerDocPasteFromRow(fresh)
			}
		})
	}
	// The snapshot cannot tell an empty-marker entry from an absent one, so
	// each shape gets its own guarded attempt; a value landing concurrently
	// fails both guards and wins.
	written, err := r.guardedPutIndexEntry(indexKey, nil, true, fresh, post)
	if err != nil {
		return err
	}
	if !written {
		written, err = r.guardedPutIndexEntry(indexKey, nil, false, fresh, post)
		if err != nil {
			return err
		}
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
// wantIdentity + wantCreatedAt name the exact paste instance the caller
// authorized (from its own ownership read). They are re-checked INSIDE the
// {slug} transaction against the head that transaction reads: the service-layer
// ownership check happens OUTSIDE any transaction, so on its own it cannot stop
// a delete+re-mint of the same slug from committing in the window and having
// this delete destroy the NEW owner's paste.
func (r *ShaleRepo) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	pasteKey := shaleKeyPaste(slug)
	var head pasteRow
	if err := r.getJSON(pasteKey, &head); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// A cheap out-of-tx reject for the common mismatch, so the version scan is
	// skipped when the paste already changed under the caller. The AUTHORITATIVE
	// check is the in-tx one below; this read can itself be stale.
	if !pasteHeadIs(head, wantIdentity, wantCreatedAt) {
		return fmt.Errorf("shale: delete %q: %w", slug, ErrConcurrentChange)
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
		// Read the head inside the tx so a racing append conflicts, AND confirm
		// it is still the paste the caller authorized: a delete+re-mint of this
		// slug in the window leaves a different paste here, and deleting it would
		// destroy the new owner's data. Absence is idempotent (a concurrent
		// delete cascaded first); a mismatch aborts.
		var cur pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &cur); err != nil {
			return err // ErrNotFound -> idempotent; mapped below
		}
		if !pasteHeadIs(cur, wantIdentity, wantCreatedAt) {
			return errIdentityChanged
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
		if err := tx.Delete(shaleKeySlugOwner(slug)); err != nil {
			return err
		}
		// Drop the disposable cache in the same {slug} transaction. Guarded: a
		// pre-cache paste has none, and a needless tombstone-delete is avoided.
		docKey := shaleKeyVersionsDoc(slug)
		if _, gerr := tx.Get(docKey); gerr == nil {
			return tx.Delete(docKey)
		} else if !errors.Is(gerr, backend.ErrNotFound) {
			return gerr
		}
		return nil
	}
	var txErr error
	if r.kv != nil {
		now := time.Now().UTC()
		txErr = r.kv.Transact(pasteKey, func(tx *cluster.BlobTx) error {
			return delBody(tx, func(blobID string) error {
				ref := r.blobRefFor(pasteKey, blobID)
				if err := tx.UnbindBlob(ref); err != nil {
					return err
				}
				// Co-commits with the unbind: the object outlives its pointer,
				// so without a record naming it nothing could ever find it.
				return txRecordOrphanedRef(tx, slug, ref, now)
			})
		})
	} else {
		txErr = r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
			return delBody(tx, func(string) error { return nil })
		})
	}
	switch {
	case txErr == nil:
	case errors.Is(txErr, errIdentityChanged) || errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict):
		return fmt.Errorf("shale: delete %q: %w", slug, ErrConcurrentChange)
	case errors.Is(txErr, ErrNotFound):
		return nil // a concurrent delete cascaded first; idempotent
	default:
		return txErr
	}

	// Drop the enumeration entry and the doc entry on the {id} shard so the
	// paste leaves the owner's scan and doc render. Guarded by CreatedAt so a
	// same-owner re-mint in this window keeps its fresh entry. Idempotent.
	return r.dropOwnerEntry(wantIdentity.String(), slug.String(), wantCreatedAt)
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
		// Follow-on: flip the tombstone in the disposable cache when it exists,
		// in this same {slug} transaction. A pre-cache paste is left cache-less
		// (no scan to build one here); its reads fall back to the rows.
		if err := txMarkVersionDeletedInCache(tx, slug, v); err != nil {
			return err
		}
		if v.BlobID != "" {
			return unbind(v.BlobID)
		}
		return nil
	}
	var txErr error
	if r.kv != nil {
		now := time.Now().UTC()
		txErr = r.kv.Transact(verKey, func(tx *cluster.BlobTx) error {
			return verBody(tx, func(blobID string) error {
				ref := r.blobRefFor(pasteKey, blobID)
				if err := tx.UnbindBlob(ref); err != nil {
					return err
				}
				return txRecordOrphanedRef(tx, slug, ref, now)
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

// wantIdentity + wantCreatedAt name the paste the caller authorized. The head
// rename re-checks them INSIDE the {slug} transaction: without it a delete +
// re-mint of the slug by another owner in the window makes the first tx relabel
// the NEW owner's paste, and the confirmed identity is then reused for the
// index/doc write rather than a fresh out-of-tx read that could resolve to the
// wrong owner.
func (r *ShaleRepo) SetName(slug domain.Slug, name string, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	pasteKey := shaleKeyPaste(slug)
	if err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		if !pasteHeadIs(p, wantIdentity, wantCreatedAt) {
			return errIdentityChanged
		}
		p.Name = name
		return shaleTxPutJSON(tx, pasteKey, p)
	}); err != nil {
		if errors.Is(err, errIdentityChanged) {
			return fmt.Errorf("shale: rename %q: %w", slug, ErrConcurrentChange)
		}
		return err
	}
	// Refresh the denormalized name in the index projection and the owner doc,
	// in one {id}-shard CAS, for the SAME identity the head write just confirmed.
	// Errors PROPAGATE: the doc is what the listing renders, so a swallowed
	// failure here is a rename that reported success while staying invisible.
	identity := wantIdentity.String()
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return err
	}
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		var row identityPasteRow
		gerr := shaleTxGetJSON(tx, indexKey, &row)
		if gerr != nil && !errors.Is(gerr, ErrNotFound) {
			return gerr
		}
		// Guard each representation by CreatedAt: a re-mint landing between the
		// two transactions must not have its fresh entry/doc row relabelled.
		if gerr == nil && row.CreatedAt.Equal(wantCreatedAt) {
			row.Name = name
			if perr := shaleTxPutJSON(tx, indexKey, row); perr != nil {
				return perr
			}
		}
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			if e, ok := doc.Pastes[slug.String()]; ok && e.CreatedAt.Equal(wantCreatedAt) {
				e.Name = name
				doc.Pastes[slug.String()] = e
			}
		})
	})
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
		if err := shaleTxPutJSON(tx, pasteKey, p); err != nil {
			return err
		}
		// Free refresh: unpin already holds the authoritative scan, so re-stamp
		// the disposable cache from it in this same {slug} transaction. The head
		// was chosen from the rows, never the cache (docs/SPEC.md "The version
		// index cache").
		return txPutVersionsDoc(tx, slug, versions)
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

// pasteOwner reports which identity holds slug, and the empty identity when
// no paste does.
//
// A read failure is returned rather than folded into "nobody holds it": the two
// answers call for opposite actions, and treating an unreadable row as absent
// is what removes a live enumeration entry.
func (r *ShaleRepo) pasteOwner(slug domain.Slug) (domain.Identity, error) {
	p, err := r.Get(slug)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return p.Identity, nil
}
