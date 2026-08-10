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
	"github.com/Zamua/shale/pkg/coord/gossip"
	"github.com/Zamua/shale/pkg/rpc"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
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
// service-layer interfaces. Every operation goes through the
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

	// intents records in-flight multi-shard writes so a process death cannot
	// lose the knowledge that cleanup is owed. It is a durable.Log, never a
	// concrete store: which mechanism provides durability is not this type's
	// business (docs/SPEC.md "The durability mechanism is a port, not a layer").
	// Defaults to the metadata cluster's own implementation.
	intents durable.Log

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

	// versionsDocLogged and ownerDocLogged latch the per-subject report of a
	// document that will not decode, so a read path meeting the same damaged
	// value on every request logs it once rather than once per read. Bounded by
	// the number of damaged documents. A write that repairs the value clears
	// its latch, so a LATER corruption of the same subject is reported again.
	versionsDocLogged sync.Map
	ownerDocLogged    sync.Map

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

	// testHookVersionsDocPlanned runs after a version write has planned its
	// document state and before the transaction reads it again. It is the only
	// way to reach the window where the document decoded at plan time and does
	// not inside the transaction.
	testHookVersionsDocPlanned func(slug domain.Slug)

	// testHookVersionsCascadeMidRead runs BETWEEN the whole-paste delete's two
	// enumeration reads. Committing a version there is what proves the probe
	// number covers exactly the key set the cascade removes, whichever of the
	// two reads the window sits behind.
	testHookVersionsCascadeMidRead func(slug domain.Slug)

	// testHookOwnerEntryDropping runs before the owner-entry drop's {id}
	// transaction. Re-minting the slug there is what proves the created-time
	// guard tells a re-mint from the paste the drop was aimed at.
	testHookOwnerEntryDropping func(identity, slug string)

	// testHookLegacyUnpinScanned runs between the pre-migration unpin's version
	// scan and its transaction. Tombstoning the chosen version there, or
	// planting a document, reaches the two states the candidate-key probe
	// cannot detect on its own.
	testHookLegacyUnpinScanned func(slug domain.Slug)
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

	bk.applyTo(&clusterCfg)

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
		bindAddr: cfg.BindAddr,
		grpcAddr: advertiseGRPCAddr,
		nodeID:   cfg.NodeID,
		backing:  bk,
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

	// Tolerant: an entry damaged at the ENVELOPE layer is read through its
	// authoritative row by the upgrade path below, the same way one whose JSON
	// will not decode is. A strict scan would instead deny the owner their whole
	// listing over one entry, which is the failure mode the read-through exists
	// to prevent.
	idx, err := r.scanPrefixTolerant(shalePrefixIdentityPastes(owner))
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
		versions, err := r.versionSet(slug)
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

// ListVersions renders the paste's history from ONE point Get when the paste
// has a document; a pre-migration paste falls back to the legacy prefix scan,
// READ-ONLY. A truncated document still renders: the list needs only what is
// present, and the miss is an under-count. What it must NOT do is imply the
// list is whole, which is why the render marks the current version from the
// HEAD rather than from the newest live entry it can see.
func (r *ShaleRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	versions, err := r.versionSet(slug)
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

// GetVersion answers from the document when one exists. A number a TRUNCATED
// document lacks is ErrVersionsIncomplete, never ErrNotFound: the caller's
// gates (delete's served-version check, pin's target) would otherwise act on a
// confident wrong answer. Truncated covers both a heal that skipped a row and a
// document behind the head's numbering mark, which is why the read is gated.
func (r *ShaleRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	doc, err := r.versionsDocGated(slug)
	if err != nil {
		return domain.Version{}, err
	}
	if doc != nil {
		if v, ok := doc.find(ver); ok {
			return v.toDomain(slug), nil
		}
		if !doc.Complete {
			return domain.Version{}, ErrVersionsIncomplete
		}
		return domain.Version{}, ErrNotFound
	}
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
	if doc, err := r.getOwnerDoc(owner); err != nil {
		return 0, err
	} else if doc != nil {
		return len(doc.Pastes), nil
	}
	// Tolerant, so the count agrees with what ListByOwner renders: both walk
	// the same entries, and a damaged value still has a countable KEY.
	idx, err := r.scanPrefixTolerant(shalePrefixIdentityPastes(owner))
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
	// Tolerant: a strict scan aborts on ENVELOPE damage before any per-record
	// decision, which turns one bad entry into the lock-out this fail-open rule
	// exists to prevent.
	idx, err := r.scanPrefixTolerant(shalePrefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	var skips scanSkips
	for _, item := range idx {
		if item.Damaged != nil {
			// Counted as zero, like any entry that cannot be read. Checked
			// BEFORE the empty-value branch below: a damaged record carries no
			// value either, and reading it as the legacy empty MARKER would send
			// a corrupt entry down the authoritative read-through.
			skips.add(item.Key, item.Damaged)
			continue
		}
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

// sumLiveVersionBytes sums the sizes of a paste's non-deleted versions, from
// the document when it has one.
//
// The figure feeds the owner's USED bytes, so a document that under-counts
// under-CHARGES: it leaves more headroom and admits a write the true sum would
// have refused. That is the same direction the quota policy already fails on
// purpose - an entry it cannot read at all counts as zero - and for the same
// reason: refusing instead locks a person out of the very uploads that would
// let them replace or delete the damaged thing. The over-admit is bounded by
// one paste's live bytes and the object-store bucket quota still caps the
// total.
//
// It is deliberately NOT gated on the staleness oracle. The gate costs a head
// Get per paste on a path an upload waits behind, and it has no better answer
// to give: the only alternatives to the under-count are refusing the upload
// (the lockout above) or falling back to the version-row scan (a scan on the
// request path, which the document exists to remove).
func (r *ShaleRepo) sumLiveVersionBytes(slug domain.Slug) (int64, error) {
	versions, err := r.versionSet(slug)
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
//  1. the paste head row's served ContentSHA -> its BlobID (the common path,
//     since an HTML/markdown paste read passes the head's ContentSHA),
//  2. the head's MANIFEST: a file inside the served directory, whose sha names
//     a manifest entry rather than a version's served content,
//  3. the paste's version records: a non-head (pinned-away or Show'd) version
//     whose ContentSHA matches -> that version's BlobID, then those records'
//     manifests.
//
// The head answers FIRST and WHOLE, which is what keeps the version document
// off the serving path: the head carries the served version's descriptor and
// its entire manifest, so every sha a SERVED request can name is in hand before
// anything else is read, and a document that will not read cannot fail a
// request the head could answer. Only a version the URL is NOT serving reaches
// step 3.
//
// Returns ("", ErrNotFound) when no metadata references the sha (a deleted or
// unbound blob), which the seam maps to blob.ErrNotFound. A legacy row with an
// empty BlobID returns ""; the seam reads "" as sha-keyed and falls back.
func (r *ShaleRepo) ResolveBlobID(slug domain.Slug, contentSHA string) (string, error) {
	var p pasteRow
	if perr := r.getJSON(shaleKeyPaste(slug), &p); perr != nil {
		if !errors.Is(perr, ErrNotFound) {
			return "", perr
		}
		return "", ErrNotFound
	}
	if p.ContentSHA == contentSHA && p.BlobID != "" {
		return p.BlobID, nil
	}
	if id := manifestBlobID(p.decode(), contentSHA); id != "" {
		return id, nil
	}
	// One point Get on a migrated paste, since every record carries its own
	// descriptor and manifest.
	versions, verr := r.versionSet(slug)
	if verr != nil {
		return "", verr
	}
	for _, v := range versions {
		if v.ContentSHA == contentSHA && v.BlobID != "" {
			return v.BlobID, nil
		}
	}
	for _, v := range versions {
		if id := manifestBlobID(v.decode(), contentSHA); id != "" {
			return id, nil
		}
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
		case owner == "":
			// NOBODY holds the slug as of this read, and this read is OUTSIDE
			// the transaction that would act on it, on a different shard from
			// the entry, so it cannot be moved inside one. A same-identity
			// sibling committing its row after it leaves a guard that still
			// MATCHES, and the rollback would then delete the entry that
			// sibling's row depends on. The sweep asks the same question after
			// the resolve grace, when the answer can no longer change under it,
			// so the decision is deferred there rather than taken on a reading
			// that is stale by construction.
			r.repoLog().Printf("shale: nothing holds %s after a failed insert (keeping the enumeration entry; the intent sweep settles it once the answer is stable)", p.Slug)
			return err
		}
		// A DIFFERENT identity holds the slug: positive evidence that the entry
		// written above is an orphan, since no sibling of ours could have
		// claimed a slug someone else holds. Best-effort removal keeps the
		// common failure (a slug race) from charging the owner; if it fails, the
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
		// A fresh head starts the version set at generation 1. Zero would mean
		// ABSENT, which is what a binary predating the field leaves.
		gen := versionGen{seen: 0, next: 1}
		pr.VersionsGen = gen.next
		if err := shaleTxPutJSON(tx, pasteKey, pr); err != nil {
			return err
		}
		if err := shaleTxPutJSON(tx, shaleKeyVersion(p.Slug, 1), v1); err != nil {
			return err
		}
		// The document is the whole set at insert - v1 and nothing else - so it
		// is written outright rather than mutated, which also repairs any
		// document a previous paste on this slug left behind. A paste created
		// by this release starts COMPLETE; only a heal can produce an
		// incomplete one.
		if err := r.txWriteVersionsDoc(tx, p.Slug,
			versionsDoc{Versions: []versionRow{v1}, HighWater: 1, Complete: true}, gen); err != nil {
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
		identity, createdAt = p.Identity, p.CreatedAt
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
	// Step 2: drop the enumeration entry and the doc entry on the {id} shard
	// so the failed paste leaves ListByOwner and stops being enumerated at all.
	// The paste ROW survives this transition, so the slug does not return to the
	// mint and the created-time guard has nothing to hold it back.
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

	res, err := r.appendAuthoritative(slug, ref, now, binds, blobOwnerEpochFromContext(ctx), markFromHead(existing), identOf(existing))
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

// errVerChanged signals that a version chosen OUTSIDE a transaction is no
// longer the record the choice assumed when the transaction re-read it: a
// concurrent tombstone. The candidate-key probe cannot see it - that probe
// tests an ABSENT number, and a tombstone rewrites a number that is present.
// Never escapes the function that raises it.
var errVerChanged = errors.New("shale: chosen version changed since it was read")

// errVersionsDocAppeared signals that a document exists inside a transaction
// that was dispatched down the pre-migration row path. Never escapes the
// function that raises it.
var errVersionsDocAppeared = errors.New("shale: versions doc appeared since the dispatch read")

// ErrConcurrentChange aliases the domain-owned sentinel, matching how the rest
// of the storage error vocabulary is re-exported.
var ErrConcurrentChange = domain.ErrConcurrentChange

// appendAuthoritative writes the new version on the {slug} shard: the document
// record and the legacy row, in one transaction. The next version number comes
// from the document (or, for a pre-migration paste, the heal walk) OUTSIDE the
// tx, so three race outcomes are retried by re-reading for a fresh number:
//
//   - the candidate version key is ALREADY present at read time inside the tx
//     (a concurrent append committed it after the read): errVerTaken,
//   - the candidate key is absent at read time but a concurrent append commits
//     it first, so the ExpectAbsent read-check fails validation: ErrCASConflict,
//   - the document read INSIDE the tx claims a HIGHER next number than this
//     attempt planned.
//
// headMark is what the head row says about the version set. Its latest_version
// is a FLOOR on the numbers already issued, which is what keeps a document that
// is behind the rows from re-issuing a number: the previous binary appends by
// writing a row and bumping that mark without touching the document, so a
// rollback and roll-forward leaves exactly that state. Its generation is the
// other half of the staleness oracle the plan uses to decide the document needs
// re-walking. A taken key raises the floor so the next attempt starts past it -
// which is what gives the retry loop its progress guarantee.
//
// The mark counts tombstones and never lowers, so version numbers are never
// reused. An append proceeds on a TRUNCATED document: it needs a NUMBER, and
// the mark reserves the numbers the heal could not read.
func (r *ShaleRepo) appendAuthoritative(slug domain.Slug, ref contentRef, now time.Time, refs []cluster.BlobRef, epoch int64, headMark versionMark, owner pasteIdent) (AppendResult, error) {
	pasteKey := shaleKeyPaste(slug)
	// The blob id lands on the new version row and, when the head is unpinned
	// (so the public URL follows this version), on the paste head row too.
	blobID := firstBlobID(refs)
	const maxRenumberAttempts = 16
	mark := headMark // numbers at or below mark.latest are already issued
	for range maxRenumberAttempts {
		// mark.latest doubles as the staleness mark: a candidate key found
		// taken is itself proof the document is behind the rows, so raising the
		// floor also makes the next plan re-walk them.
		plan, heal, err := r.versionsDocPlan(slug, mark)
		if err != nil {
			return AppendResult{}, err
		}
		if heal != nil {
			heal.planIdent = owner
		}
		newVer := max(mark.latest+1, plan.nextVerNum())
		verKey := shaleKeyVersion(slug, newVer)

		var wasPinned bool
		txErr := r.runAuthoritative(pasteKey, refs, bindFence{slug: slug, epoch: epoch}, func(tx shaleKVTx, bind func() error) error {
			var p pasteRow
			if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
				return err
			}
			// Reading the candidate key records an ExpectAbsent read-check, so a
			// concurrent commit of it after this read conflicts at Commit time.
			// The legacy dual write needs that key free.
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
			gen := genFromHead(p)
			if err := r.txApplyVersionsDoc(tx, slug, heal, gen, identOf(p), func(doc *versionsDoc) error {
				// The tx-read document is the authority on the number: a
				// racing append that landed after the plan makes this
				// disagree, and re-planning is the fix.
				if doc.nextVerNum() > newVer {
					return errVerTaken
				}
				doc.upsert(newV)
				doc.HighWater = newVer
				return nil
			}); err != nil {
				return err
			}
			p.UpdatedAt = now
			// The totals roll in THIS transaction, so they can never disagree
			// with the version rows they summarise. The generation moves with
			// the document it stamps, in the same write.
			p.LiveBytes += ref.Size
			p.LatestVersion = newVer
			p.VersionsGen = gen.next
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
		case errors.Is(txErr, errVerTaken):
			mark.latest = newVer // taken; the next attempt starts past it
			continue
		case errors.Is(txErr, backend.ErrCASConflict):
			continue // re-read + re-number
		case errors.Is(txErr, errVersionsDocUnseeded):
			// The document was believable when this attempt planned and is not
			// inside the transaction, so the plan's generation is the thing
			// that misled it. Dropping it forces the next plan to WALK, which
			// is what makes the retry converge instead of re-deciding the same
			// way on the same stale reading.
			mark.gen = 0
			continue
		case errors.Is(txErr, errVersionsSeedForeign):
			// The slug was re-minted as a different paste in the plan-to-tx
			// window; a retry would re-plan against the same outer identity and
			// mismatch again, so this append cannot proceed.
			return AppendResult{}, fmt.Errorf("shale: append %q: %w", slug, ErrConcurrentChange)
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
// never creates: entry-absence gives the legacy guard nothing to compare, so
// the doc's own membership is the guard.
//
// That membership is decided INSIDE the transaction. The doc read below is a
// pre-filter that saves a head read; on its own it would let a whole-paste
// delete commit in the window and still satisfy the absent-guard, CREATING a
// legacy entry for a paste that is gone. Both the doc and the entry live on the
// {id} shard, so the in-transaction answer serializes against that delete: the
// slug missing from the doc aborts the whole transaction and nothing is written.
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
		held := false
		if err := r.txApplyOwnerDoc(tx, identity, candidate, func(d *ownerDoc) {
			if _, ok := d.Pastes[slug.String()]; ok {
				d.Pastes[slug.String()] = ownerDocPasteFromRow(fresh)
				held = true
			}
		}); err != nil {
			return err
		}
		if !held {
			return errIndexEntryChanged // the paste left the owner's index since the pre-filter
		}
		return nil
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
// unsupported inside a CAS tx), so the cascade joins the read set three ways:
// the head row and the version DOCUMENT are read INSIDE the tx, and the
// candidate NEXT version key is read ExpectAbsent. Without those the tx would
// carry only writes and commit unconditionally, leaving a version a concurrent
// append wrote alive under a deleted paste - an orphan the owner's next list is
// the only thing that prunes, whose ContentSHA keeps its blob out of the GC's
// reach forever.
//
// The cascade set comes from deleteCascade, which enumerates the version PREFIX
// and takes blob ids from the rows and the document both. Deriving it from the
// document alone cannot be made safe: a document behind the rows leaves a row
// and its bound blob alive under a deleted paste while the slug returns to the
// mint, so the next paste to mint that slug inherits them.
//
// A racing append aborts the delete rather than re-reading. Transact already
// spends a full CAS budget on the conflict, so a second retry layer here would
// only re-run an exhausted one; and the version set is read OUTSIDE the tx, so
// the only fix for a stale read is to start over from the caller.
//
// The owner's entry lives on a DIFFERENT shard, so it cannot drop in the same
// transaction and its removal is a second step that can fail on its own. Once
// the {slug} rows are gone nothing identifies the owner any more - the head and
// slug_owner both went with them - so a retry finds nothing to do and the entry
// would be a permanent phantom, counted against the owner's quota and listed as
// a slug that does not resolve. A DURABLE INTENT recorded before the cascade is
// what makes that step recoverable: it names the owner, the slug and the entry
// bytes, and the sweep (boot, and the owner's own next listing) finishes the
// removal against a row that is by then stably absent.
func (r *ShaleRepo) Delete(slug domain.Slug) error {
	ctx := context.Background()
	pasteKey := shaleKeyPaste(slug)
	docKey := shaleKeyVersionsDoc(slug)
	var head pasteRow
	if err := r.getJSON(pasteKey, &head); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	cascade, err := r.deleteCascade(slug, head.LatestVersion)
	if err != nil {
		return err
	}
	intent, ierr := r.beginDeleteIntent(ctx, head.Identity, slug)
	if ierr != nil {
		return ierr
	}
	// The mark counts tombstones, so this is the number a racing append would
	// claim next.
	nextVerKey := shaleKeyVersion(slug, cascade.next)

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
		// The document joins the read set whether or not it exists, so a racing
		// append or heal that writes it conflicts with this cascade.
		docPresent := false
		switch _, gerr := tx.Get(docKey); {
		case gerr == nil:
			docPresent = true
		case !errors.Is(gerr, backend.ErrNotFound):
			return gerr
		}
		if err := tx.Delete(pasteKey); err != nil {
			return err
		}
		// A tombstoned version's pointer was already unbound by DeleteVersion;
		// re-unbinding is an idempotent tx.Delete of a missing key. A legacy
		// record carries no BlobID and has no pointer.
		for _, blobID := range cascade.blobIDs {
			if err := unbind(blobID); err != nil {
				return err
			}
		}
		for _, verKey := range cascade.verKeys {
			if err := tx.Delete(verKey); err != nil {
				return err
			}
		}
		if docPresent {
			if err := tx.Delete(docKey); err != nil {
				return err
			}
		}
		return tx.Delete(shaleKeySlugOwner(slug))
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
	case errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict):
		r.forgetDeleteIntent(ctx, intent) // nothing was applied
		return fmt.Errorf("shale: delete %q: %w", slug, ErrConcurrentChange)
	case errors.Is(txErr, ErrNotFound):
		r.forgetDeleteIntent(ctx, intent)
		return nil // a concurrent delete cascaded first; idempotent
	default:
		r.forgetDeleteIntent(ctx, intent)
		return txErr
	}

	// Drop the enumeration entry and the doc entry on the {id} shard so the
	// paste leaves the owner's scan and doc render. Idempotent. The intent is
	// discharged only once that lands; a failure leaves it, and the sweep
	// repeats this step.
	if err := r.dropOwnerEntry(head.Identity, slug.String(), head.CreatedAt); err != nil {
		return fmt.Errorf("shale: delete %q: dropping the owner entry: %w (the intent sweep retries it)", slug, err)
	}
	r.forgetDeleteIntent(ctx, intent)
	return nil
}

// beginDeleteIntent records that the owner's enumeration entry for slug must be
// removed, BEFORE the {slug} rows that name the owner are destroyed. The entry's
// own bytes ride along as the compensating delete's guard, so a re-mint of the
// slug that rewrote the entry is never eaten by a late sweep.
//
// An entry that cannot be read leaves the guard empty, which the guarded delete
// treats as "match nothing" - the phantom then survives, which beats a sweep
// deleting an entry it could not identify.
//
// A head with no identity has no owner index to clean up, so it records
// nothing: the zero intent's Complete is a no-op.
func (r *ShaleRepo) beginDeleteIntent(ctx context.Context, identity string, slug domain.Slug) (durable.Intent, error) {
	if identity == "" {
		return durable.Intent{}, nil
	}
	guard, gerr := r.getRaw(shaleKeyIdentityPaste(identity, slug.String()))
	if gerr != nil {
		r.repoLog().Printf("shale: reading the delete guard for %s: %v (a sweep will skip rather than risk a fresher entry)", slug, gerr)
	}
	in := durable.Intent{
		ID: durable.ID(slug.String()), Kind: durable.KindDeletePaste,
		Scope: durable.Scope(identity), Subject: slug.String(),
		Guard: guard, StartedAt: time.Now().UTC(),
	}
	if err := r.intents.Begin(ctx, in); err != nil {
		return durable.Intent{}, fmt.Errorf("durable intent: %w", err)
	}
	return in, nil
}

// forgetDeleteIntent discharges the delete intent. A lost Complete leaves an
// intent whose subject row is absent, which the sweep settles by repeating a
// removal that is idempotent.
func (r *ShaleRepo) forgetDeleteIntent(ctx context.Context, in durable.Intent) {
	if in.ID == "" {
		return
	}
	if err := r.intents.Complete(ctx, in.ID, in.Scope); err != nil {
		r.repoLog().Printf("shale: forgetting the delete intent for %s: %v (a sweep will retry it)", in.Subject, err)
	}
}

// DeleteVersion tombstones a single version: it stays visible in the list
// flagged deleted, but its content blob is no longer referenced, so the GC
// reclaims it. Its bytes leave the owner's scan-derived sum via the
// index-projection refresh after the tombstone commits; a lost refresh is a
// bounded stale-cache window the owner's next list heals. Re-deleting an
// already-tombstoned version is a no-op.
//
// The version the URL serves is refused with ErrVersionServed, decided from the
// head and the record the TRANSACTION reads. Everything below the transaction
// is planning: a plan is a snapshot, and the one check whose subject a
// concurrent pin can move under it has to be re-taken where the act happens.
func (r *ShaleRepo) DeleteVersion(slug domain.Slug, ver int) error {
	// Existence gate: a missing paste yields ErrNotFound.
	var p pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &p); err != nil {
		return err
	}
	plan, heal, err := r.versionsDocPlan(slug, markFromHead(p))
	if err != nil {
		return err
	}
	if heal != nil {
		heal.planIdent = identOf(p)
	}
	// The tombstone proceeds on a TRUNCATED document for a target the document
	// HOLDS: the document is a subset of the true set, so its newest live
	// version is never higher than the true one, which keeps the service's
	// served-version guard exact. A target it does NOT hold is unavailable
	// rather than not-found.
	target, held := plan.find(ver)
	if !held {
		if !plan.Complete {
			return ErrVersionsIncomplete
		}
		return ErrNotFound
	}
	if target.Deleted {
		return nil // already tombstoned; no-op
	}
	verKey := shaleKeyVersion(slug, ver)
	// The tombstone tx pins on verKey while the blob pointer routes on {slug},
	// but versions/<slug>/<NNNN>, versions_doc/<slug> and pastes/<slug> shard on
	// the same <slug>, so the unbind co-commits in one single-shard transaction.
	// Each version's blob has a unique stage-minted id (no within-record dedup),
	// so no live sibling references it and the unbind is unconditional; the
	// served version refuses below, from the head this transaction reads, so
	// the head's blob id is never the one unbound here.
	pasteKey := shaleKeyPaste(slug)

	verBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		var shed int
		var blobID string
		alreadyDeleted := false
		// The head is read FIRST because the document write below is stamped
		// with the generation this transaction reads off it, and the two must
		// be written from the same reading to stay accountable to each other.
		var head pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &head); err != nil {
			return err
		}
		gen := genFromHead(head)
		if err := r.txApplyVersionsDoc(tx, slug, heal, gen, identOf(head), func(doc *versionsDoc) error {
			v, ok := doc.find(ver)
			if !ok {
				if !doc.Complete {
					return ErrVersionsIncomplete
				}
				return ErrNotFound
			}
			if v.Deleted {
				alreadyDeleted = true
				return nil
			}
			// The served-version guard, on the two operands this transaction
			// already holds. The service refuses first for the message, but a
			// pin or an unpin committing between that refusal and this
			// transaction moves the head onto this very version, and a guard
			// read outside the transaction that acts cannot see it. Here the
			// guard and the act cannot be split.
			if headServesVersion(head, v) {
				return ErrVersionServed
			}
			v.Deleted = true
			doc.upsert(v)
			shed, blobID = v.Size, v.BlobID
			return nil
		}); err != nil {
			return err
		}
		if !alreadyDeleted {
			if err := r.txTombstoneLegacyVersionRow(tx, slug, ver); err != nil {
				return err
			}
			// The head's LiveBytes sheds this version's bytes in the SAME
			// transaction, so the total never disagrees with the set it
			// summarises. LatestVersion is untouched: version numbers are never
			// reused, so tombstoning one does not lower the mark.
			head.LiveBytes -= shed
			if head.LiveBytes < 0 {
				head.LiveBytes = 0
			}
		}
		// The head is rewritten even when the tombstone was a no-op, because
		// the transaction still WROTE the document (a heal it applied, a
		// generation it stamped). A document whose generation the head does not
		// carry is unaccountable, and leaving it that way would refuse every
		// gated operation on the paste until some later write happened to fix
		// it.
		head.VersionsGen = gen.next
		if err := shaleTxPutJSON(tx, pasteKey, head); err != nil {
			return err
		}
		if alreadyDeleted || blobID == "" {
			return nil
		}
		return unbind(blobID)
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
	if errors.Is(txErr, errVersionsDocUnseeded) {
		// The document decoded when the tombstone was planned and not inside
		// the transaction, so nothing was applied and the re-plan is the whole
		// fix: its read meets the damage and walks the legacy rows.
		return fmt.Errorf("shale: delete version %q/%d: %w", slug, ver, ErrConcurrentChange)
	}
	if errors.Is(txErr, errVersionsSeedForeign) {
		// The slug was re-minted as a different paste in the plan-to-tx window,
		// so the plan's seed is foreign to the paste the transaction found;
		// nothing was applied and the re-plan runs against the new paste.
		return fmt.Errorf("shale: delete version %q/%d: %w", slug, ver, ErrConcurrentChange)
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

// SetName sets the paste's human label on the {slug} shard, then carries it
// into the owner's index projection and document on the {id} shard.
//
// The identity comes OUT of the renaming transaction rather than from a
// separate slug_owner read. A whole-paste delete returns the slug to the mint,
// so a second read can resolve it to a DIFFERENT owner who has since re-minted
// it, and the rename would then be written into that owner's index for a paste
// it does not describe. Taken from the row this call renamed, the projection
// can only ever target the paste that was renamed.
//
// Its created time rides along for the same reason one shard down: the same
// owner re-minting the slug leaves an entry the rename must not touch, so an
// entry created strictly after the renamed paste is left alone.
func (r *ShaleRepo) SetName(slug domain.Slug, name string) error {
	pasteKey := shaleKeyPaste(slug)
	var identity string
	var createdAt time.Time
	if err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		p.Name = name
		identity, createdAt = p.Identity, p.CreatedAt
		return shaleTxPutJSON(tx, pasteKey, p)
	}); err != nil {
		return err
	}
	// Refresh the denormalized name in the index projection and the owner
	// doc, in one {id}-shard CAS. Errors PROPAGATE: the doc is what the
	// listing renders, so a swallowed failure here is a rename that reported
	// success while staying invisible.
	if identity == "" {
		return nil // a row with no owner; nothing to refresh
	}
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return err
	}
	indexKey := shaleKeyIdentityPaste(identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		var row identityPasteRow
		gerr := shaleTxGetJSON(tx, indexKey, &row)
		switch {
		case errors.Is(gerr, ErrNotFound):
			// No cached entry to relabel; the doc below still carries the name.
		case gerr != nil:
			return gerr
		case !row.CreatedAt.After(createdAt):
			row.Name = name
			if perr := shaleTxPutJSON(tx, indexKey, row); perr != nil {
				return perr
			}
		}
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			if e, ok := doc.Pastes[slug.String()]; ok && !e.CreatedAt.After(createdAt) {
				e.Name = name
				doc.Pastes[slug.String()] = e
			}
		})
	})
}

// ownerOfSlug resolves a slug's owner identity from slug_owner in one
// {slug}-shard read. Returns "" if the paste is gone, which makes the
// best-effort index refresh it feeds a harmless no-op.
//
// Its callers are PROJECTION refreshes only, which is why the read may sit
// outside their transaction. A delete plus a re-mint by a different owner
// resolves to that new owner, and the refresh then rebuilds THAT owner's entry
// from the head - which is their own paste's head, so the answer is correct for
// whoever the slug now belongs to. A path that carried a value from the old
// paste would have to take its identity out of the row it read instead: see
// SetName.
func (r *ShaleRepo) ownerOfSlug(slug domain.Slug) string {
	raw, err := r.getRaw(shaleKeySlugOwner(slug))
	if err != nil || raw == nil {
		return ""
	}
	return string(raw)
}

// SetPinnedVersion rolls the head onto a named version and makes it sticky.
// A TOMBSTONED target is refused with ErrVersionDeleted from inside the
// transaction: its blob is already unbound, so the pin would publish bytes the
// reclaimer is coming for.
func (r *ShaleRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	pasteKey := shaleKeyPaste(slug)
	err := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		// Repoint the head's served descriptor as ONE value: the document's
		// record carries the full contentRef (including the MANIFEST and the
		// BlobID, neither of which domain.Version carries), so no field can
		// drift and the roll needs no second record that may be damaged. The
		// document co-shards on {slug}, keeping this read inside the same CAS.
		ref, err := r.txServedRefForVersion(tx, slug, ver.VerNum, markFromHead(p))
		if err != nil {
			return err
		}
		p.PinnedVersion = ver.VerNum
		p.contentRef = ref
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
// It REFUSES on a document it cannot trust rather than acting: a truncated
// list's newest live version is not the newest, so the roll would supersede a
// newer version permanently - the head would go BACKWARDS and the newer
// version's content would stop being served with no way to get it back.
// Refusing costs the owner one operation and leaves the paste serving exactly
// what it served. Three shapes reach it: a heal that skipped a row, a document
// below the head's numbering mark (an older binary's append), and a document
// the head's generation cannot account for (an older binary's per-version
// delete, which leaves the newest record claiming to be live while its blob is
// already unbound). The document is repaired by the paste's next write, and the
// unpin then rolls to the version the refusal preserved.
//
// The DISPATCH read below is outside any transaction, so the row path re-takes
// it inside its own and hands back ErrConcurrentChange when a document turns
// out to exist. A document that will not DECODE reads as absent to both, which
// is deliberate: the rows are what repairs it, so the row path has to stay
// reachable for such a paste.
func (r *ShaleRepo) Unpin(slug domain.Slug) error {
	doc, err := r.getVersionsDoc(slug)
	if err != nil {
		return err
	}
	if doc != nil {
		return r.unpinFromDoc(slug)
	}
	return r.unpinFromLegacyRows(slug)
}

// unpinFromDoc rolls the head from the document, read INSIDE the tx so the
// decision joins its read set: a racing append conflicts rather than the head
// rolling off a stale set. The head read in that same transaction supplies both
// the numbering mark and the generation, so the staleness check costs nothing
// and cannot itself be stale relative to the document it judges.
func (r *ShaleRepo) unpinFromDoc(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	docKey := shaleKeyVersionsDoc(slug)
	txErr := r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		var p pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &p); err != nil {
			return err
		}
		var fresh versionsDoc
		raw, gerr := tx.Get(docKey)
		if gerr != nil {
			// Gone between the two reads (only a whole-paste delete removes
			// it). Nothing was applied, so the caller retries and takes
			// whichever path the paste is then in.
			return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
		}
		if derr := decodeVersionsDoc(raw, &fresh); derr != nil {
			// Damaged between the two reads; a retry falls back to the legacy
			// rows, which are still dual-written and true.
			return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
		}
		fresh.applyHead(markFromHead(p))
		if !fresh.Complete {
			return fmt.Errorf("shale: unpin %q: %w", slug, ErrVersionsIncomplete)
		}
		latest, ok := fresh.latestLive()
		if !ok {
			return ErrNotFound
		}
		p.PinnedVersion = 0
		p.contentRef = latest.contentRef // whole served descriptor rolls, manifest included
		return shaleTxPutJSON(tx, pasteKey, p)
	})
	if errors.Is(txErr, backend.ErrCASConflict) {
		return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
	}
	if txErr == nil {
		r.refreshPinProjection(slug)
	}
	return txErr
}

// unpinFromLegacyRows is the pre-migration path: the version scan runs outside
// the transaction (ScanPrefix is unsupported inside a CAS tx), so the candidate
// NEXT version key is read ExpectAbsent inside the tx. An append that lands
// after the scan is then either visible to that read or fails the read-check,
// and both routes abort rather than commit a head chosen from a stale version
// set. A row that cannot be read means the set cannot be read in full, so the
// roll REFUSES with the versions-incomplete sentinel rather than rolling off a
// truncated set - the same refusal the document path makes, spelled so the verb
// reports it instead of surfacing a raw envelope-strip error.
//
// The probe detects a writer that ADDS a number and nothing else. A per-version
// delete MODIFIES an existing row, leaving the probe key untouched, so the roll
// TARGET is re-read inside the transaction and its descriptor taken from there:
// otherwise a tombstone committing after the scan would put the head on a blob
// that same tombstone unbound. The DISPATCH is re-taken too - a document
// appearing after the caller chose this path means the decision belongs to
// unpinFromDoc, which applies the completeness gate this path has no way to.
func (r *ShaleRepo) unpinFromLegacyRows(slug domain.Slug) error {
	pasteKey := shaleKeyPaste(slug)
	docKey := shaleKeyVersionsDoc(slug)
	items, err := r.scanPrefixTolerant(shalePrefixVersions(slug))
	if err != nil {
		return err
	}
	versions := make([]versionRow, 0, len(items))
	for _, item := range items {
		if item.Damaged != nil {
			return fmt.Errorf("shale: unpin %q: %w", slug, ErrVersionsIncomplete)
		}
		var v versionRow
		if derr := json.Unmarshal(item.Value, &v); derr != nil {
			return fmt.Errorf("shale: unpin %q: %w", slug, ErrVersionsIncomplete)
		}
		versions = append(versions, v)
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
	targetKey := shaleKeyVersion(slug, latest.VerNum)
	if hook := r.testHookLegacyUnpinScanned; hook != nil {
		hook(slug)
	}

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
		// An UNDECODABLE document also reads as absent to the dispatch, and the
		// rows are what repairs that, so only a document that decodes hands the
		// roll back.
		switch raw, gerr := tx.Get(docKey); {
		case gerr == nil:
			var fresh versionsDoc
			if decodeVersionsDoc(raw, &fresh) == nil {
				return errVersionsDocAppeared
			}
		case !errors.Is(gerr, backend.ErrNotFound):
			return gerr
		}
		var target versionRow
		if err := shaleTxGetJSON(tx, targetKey, &target); err != nil {
			return err
		}
		if target.Deleted || target.VerNum != latest.VerNum {
			return errVerChanged
		}
		p.PinnedVersion = 0
		p.contentRef = target.contentRef // whole served descriptor rolls to the latest live version
		return shaleTxPutJSON(tx, pasteKey, p)
	})
	if errors.Is(txErr, errVerChanged) || errors.Is(txErr, errVersionsDocAppeared) {
		return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
	}
	if errors.Is(txErr, errVerTaken) || errors.Is(txErr, backend.ErrCASConflict) {
		return fmt.Errorf("shale: unpin %q: %w", slug, ErrConcurrentChange)
	}
	if txErr == nil {
		r.refreshPinProjection(slug)
	}
	return txErr
}

// refreshPinProjection moves the cached pin with the head after a roll. The
// listing renders the pin from the cached entry, so it has to move with the
// head. Best-effort: a lost refresh is corrected by the spot-check on a later
// list.
func (r *ShaleRepo) refreshPinProjection(slug domain.Slug) {
	if err := r.refreshIndexProjection(r.ownerOfSlug(slug), slug); err != nil {
		r.repoLog().Printf("shale: index refresh after unpin %s: %v (a later list corrects it)", slug, err)
	}
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
