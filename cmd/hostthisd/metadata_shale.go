// metadata_shale.go - shale-cluster-backed metadataBundle, active only under
// -tags slatedb (shale wraps slatedb as its per-node KV engine).
//
// S3 connection config, shared with the slatedb backend:
//   HOSTTHIS_METADATA_S3_ENDPOINT   (e.g. http://minio:9000)
//   HOSTTHIS_METADATA_S3_BUCKET     (required)
//   HOSTTHIS_METADATA_S3_REGION     (default us-east-1)
//   HOSTTHIS_METADATA_S3_ACCESS_KEY (required)
//   HOSTTHIS_METADATA_S3_SECRET_KEY (required)
//   HOSTTHIS_METADATA_S3_USE_SSL    (true|false; default true)
//   HOSTTHIS_METADATA_DB_NAME       (default "hostthis-metadata")
//
// Cluster-layer additions:
//   HOSTTHIS_NODE_ID                  (default os.Hostname(), or "hostthis-1")
//   HOSTTHIS_SHALE_REPLICATION_FACTOR (default 1)
//   HOSTTHIS_SHALE_BIND_ADDR          (host:port; NON-EMPTY enables multi-node mode)
//   HOSTTHIS_SHALE_GRPC_ADDR          (host:port; required when BIND_ADDR is set)
//   HOSTTHIS_SHALE_SEEDS              (comma-separated peer BIND_ADDRs; empty = seed node)
//   HOSTTHIS_METADATA_AWAIT_DURABLE   (true|false; default true; false = relaxed
//                                      durability/fast-ack, only safe at RF>=2)
//   HOSTTHIS_SHALE_READ_TIMEOUT       (Go duration, e.g. "8s"; unset/empty =
//                                      shale's 5s default; malformed fails startup)
//   HOSTTHIS_SHALE_WRITE_TIMEOUT      (same, for the per-dispatch write deadline)
//
// With BIND_ADDR unset the node runs single-node: no gossip, no ring routing,
// every op local. Setting it brings up memberlist + gRPC forwarding and joins
// the ring (docs/SPEC.md "Multi-node shale").
//
// ReadConsistency is fixed at ReadQuorum inside NewShaleRepo, so there is no env
// var. Quorum is required at R>1, where ReadNearest could return a
// still-backfilling replica's NotFound for live data; at R=1 the quorum is the
// single replica, so it is behaviour-identical.

//go:build slatedb

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/backends/slate/blobstore"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/relay/relaygrpc"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
	slatedb "slatedb.io/slatedb-go/uniffi"
)

// slatedbLogLevel maps HOSTTHIS_SLATEDB_LOG_LEVEL to a slatedb LogLevel.
// Empty / "off" disables slatedb tracing. Diagnostic only: debug/trace are
// verbose and meant for short-lived investigation.
func slatedbLogLevel(s string) (slatedb.LogLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return 0, false
	case "error":
		return slatedb.LogLevelError, true
	case "warn":
		return slatedb.LogLevelWarn, true
	case "info":
		return slatedb.LogLevelInfo, true
	case "debug":
		return slatedb.LogLevelDebug, true
	case "trace":
		return slatedb.LogLevelTrace, true
	default:
		return slatedb.LogLevelInfo, true
	}
}

// openShaleRepoFromEnv builds the shale ShaleRepo from the HOSTTHIS_* env and
// nothing else: no background reconcile loop, no blob-unit wiring, no debug
// server, just the opened repo. The caller owns
// repo.Close().
//
// registerGRPC (optional, nil for callers that want the bare repo) is the
// opaque hook threaded into ShaleConfig.RegisterGRPC: in multi-node mode
// NewShaleRepo calls it with the cluster gRPC server before serving, so
// composition-root services (the relay's peer fan-out) ride the same listener +
// advertised address shale forwarding uses.
func openShaleRepoFromEnv(logger *log.Logger, registerGRPC func(*grpc.Server)) (*storage.ShaleRepo, error) {
	if lvl, on := slatedbLogLevel(os.Getenv("HOSTTHIS_SLATEDB_LOG_LEVEL")); on {
		if err := slatedb.InitLogging(lvl, nil); err != nil {
			logger.Printf("metadata: slatedb InitLogging failed: %v", err)
		} else {
			logger.Printf("metadata: slatedb tracing enabled at %v", os.Getenv("HOSTTHIS_SLATEDB_LOG_LEVEL"))
		}
	}
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")

	nodeID := strings.TrimSpace(os.Getenv("HOSTTHIS_NODE_ID"))
	if nodeID == "" {
		if h, err := os.Hostname(); err == nil {
			nodeID = strings.TrimSpace(h)
		}
		if nodeID == "" {
			nodeID = "hostthis-1"
		}
	}
	replicationFactor := envOrInt("HOSTTHIS_SHALE_REPLICATION_FACTOR", 1)
	// Backend shape: 0 (default) = one slatedb DB per node. A power of two =
	// MULTI-BACKEND sharded mode: that many slatedb units distributed across the
	// ring, routed per key. Each unit is a full slatedb instance per owning
	// replica, so keep it small on RAM-tight boxes. See docs/SPEC.md "Sharded
	// metadata (multi-backend mode)".
	unitCount := envOrInt("HOSTTHIS_SHALE_UNIT_COUNT", 0)
	if unitCount < 0 {
		unitCount = 0
	}
	// true (default) acks after the durable object-store flush. false = relaxed
	// durability (fast-ack at the memtable), the big write-throughput win; only
	// safe at RF>=2 on separate nodes. See docs/SPEC.md "Relaxed durability:
	// fast-ack at the memtable".
	awaitDurable := strings.EqualFold(envOr("HOSTTHIS_METADATA_AWAIT_DURABLE", "true"), "true")
	// Block cache for the slatedb metadata layer. Without it slatedb has NO
	// block cache and re-fetches SST blocks from the object store on every read,
	// a steady self-inflicted read storm. 0 disables.
	cacheBytes := envOrInt("HOSTTHIS_METADATA_CACHE_BYTES", 128<<20)
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	// Enable slatedb's fence-WAL garbage collector so the per-open fence WAL
	// objects do not accumulate unboundedly (slatedb ships that GC category in
	// dry-run by default). HOSTTHIS_SLATEDB_FENCE_GC=false is the kill-switch.
	reapFenceWALs := !strings.EqualFold(strings.TrimSpace(os.Getenv("HOSTTHIS_SLATEDB_FENCE_GC")), "false")
	// Per-dispatch cluster deadlines (shale defaults each to 5s; zero keeps
	// them). The read budget is the one deploys raise: during a rollout a read
	// landing in a shard's sub-second handoff window re-polls within
	// ReadTimeout, so a bigger budget turns the rare window-exceeding read into
	// latency instead of a client error. Malformed values fail startup, never a
	// silent default. See docs/SPEC.md "Dispatch deadlines: the read/write
	// timeout knobs".
	readTimeout, writeTimeout, err := shaleTimeoutsFromEnv()
	if err != nil {
		return nil, err
	}

	// Multi-node peer-discovery config. A non-empty bind addr is the
	// switch that takes the cluster out of the single-node path.
	bindAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BIND_ADDR"))
	grpcAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_GRPC_ADDR"))
	seeds := parseSeeds(os.Getenv("HOSTTHIS_SHALE_SEEDS"))

	if bucket == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_BUCKET is required for shale backend")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}
	// Checked with the other required-env checks, i.e. BEFORE anything is
	// allocated, so a refusal needs no cleanup path. Validated here rather than
	// in the cluster layer so the operator gets an error naming the env var they
	// set, not one about internal config fields.
	if err := checkUnitCountForMode(unitCount, bindAddr); err != nil {
		return nil, err
	}

	// Optional transactional shale-blob plane. With HOSTTHIS_SHALE_BLOB_BUCKET
	// set, blobs go THROUGH shale (the pointer co-commits with the metadata on
	// the owning shard) over a MinIO blob.Store pointed at that DISTINCT blob
	// bucket on the SAME object store the metadata uses. Unset keeps the
	// detached content-addressed store (buildBlobStore) as the blob backend.
	var blobStore blob.Store
	blobBucket := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BLOB_BUCKET"))
	if blobBucket != "" {
		bs, bsErr := blobstore.New(blobstore.Config{
			EndpointHost: stripScheme(endpoint),
			AccessKey:    accessKey,
			SecretKey:    secretKey,
			UseSSL:       useSSL,
			Bucket:       blobBucket,
		})
		if bsErr != nil {
			return nil, fmt.Errorf("open shale blob store: %w", bsErr)
		}
		blobStore = bs
	}

	// Optional HOMOGENEOUS bootstrap. With HOSTTHIS_SHALE_HOMOGENEOUS=true, a
	// MinIO-backed ConditionalStore over the SAME metadata bucket (namespaced by
	// the DB name, so the __cluster/init marker is one shared object for every
	// pod) lets cluster.Open decide form-vs-join at runtime against the marker
	// instead of the founder/joiner seed asymmetry; every pod runs identical
	// config. Requires multi-backend (sharded) + multi-node mode: the marker's
	// durable {gen, count} records the unit count, meaningless without sharding,
	// and the solo-start/form race only arises across gossiping pods. See
	// docs/SPEC.md "Homogeneous bootstrap (optional)".
	var condStore storageunit.ConditionalStore
	homogeneous := strings.EqualFold(strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_HOMOGENEOUS")), "true")
	if homogeneous {
		if unitCount <= 0 {
			return nil, fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-backend mode (HOSTTHIS_SHALE_UNIT_COUNT > 0)")
		}
		if bindAddr == "" {
			return nil, fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-node mode (HOSTTHIS_SHALE_BIND_ADDR set)")
		}
		cs, csErr := slate.NewMinioConditionalStore(slate.MinioConditionalStoreConfig{
			EndpointHost: stripScheme(endpoint),
			AccessKey:    accessKey,
			SecretKey:    secretKey,
			UseSSL:       useSSL,
			Bucket:       bucket,
			KeyPrefix:    dbName,
		})
		if csErr != nil {
			return nil, fmt.Errorf("open shale conditional store: %w", csErr)
		}
		condStore = cs
	}

	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            nodeID,
		Endpoint:          endpoint,
		Region:            region,
		Bucket:            bucket,
		AccessKey:         accessKey,
		SecretKey:         secretKey,
		UseSSL:            useSSL,
		DbName:            dbName,
		BindAddr:          bindAddr,
		GRPCAddr:          grpcAddr,
		Seeds:             seeds,
		ReplicationFactor: replicationFactor,
		RelaxedDurability: !awaitDurable,
		UnitCount:         unitCount,
		CacheBytes:        uint64(cacheBytes),
		ReapFenceWALs:     reapFenceWALs,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		Logger:            logger,
		BlobStore:         blobStore,
		ConditionalStore:  condStore,
		RegisterGRPC:      registerGRPC,
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	if bindAddr == "" {
		logger.Printf("metadata: shale (single-node) node=%s bucket=%s db=%s rf=%d shards=%d awaitDurable=%t fenceGC=%t blobBucket=%q endpoint=%s",
			nodeID, bucket, dbName, replicationFactor, unitCount, awaitDurable, reapFenceWALs, blobBucket, endpoint)
	} else {
		logger.Printf("metadata: shale (multi-node) node=%s bind=%s grpc=%s seeds=%d rf=%d shards=%d awaitDurable=%t fenceGC=%t homogeneous=%t blobBucket=%q bucket=%s db=%s endpoint=%s",
			nodeID, bindAddr, grpcAddr, len(seeds), replicationFactor, unitCount, awaitDurable, reapFenceWALs, homogeneous, blobBucket, bucket, dbName, endpoint)
	}
	return repo, nil
}

// buildMetadataShale opens the shale repo (openShaleRepoFromEnv) and wires the
// full daemon bundle around it: the site + room repos, the optional
// transactional blob unit, the optional debug endpoint, and the periodic
// Reconcile loop. The audit subcommand deliberately bypasses it: it wants the
// bare repo with no background loops.
func buildMetadataShale(logger *log.Logger) (*metadataBundle, error) {
	// Multi-pod relay peer transport (SPEC "Multi-pod relay: the peer
	// transport"). The RECEIVER must exist before the repo opens: its Register
	// method is the opaque func(*grpc.Server) hook NewShaleRepo calls when it
	// stands up the cluster gRPC server in multi-node mode. Its local-delivery
	// target is late-bound by main once the relay exists; frames arriving before
	// that bind are dropped, and no client can connect before the HTTP server is
	// up. Single-node mode never invokes the hook and wires no transport: the
	// relay keeps its nil publisher, the zero-peer degenerate case.
	//
	// The receiver's cap must admit the LARGEST legal frame on this channel: a
	// durable mirror carrying a committed room value verbatim
	// (domain.MaxRoomValueBytes, several times the client-socket frame cap) with
	// worst-case JSON-string inflation. Sizing it to the client-socket cap
	// silently severs cross-pod mirrors for every legal value above it (SPEC
	// "Trust boundary").
	relayRecv := relaygrpc.NewReceiver(relay.MaxDurableFrameBytes(domain.MaxRoomValueBytes))

	// /readyz mount floor (docs/SPEC.md "Readiness vs liveness"). Parsed BEFORE
	// the heavy cluster open: a malformed or out-of-range fraction is a
	// configuration error that must refuse startup.
	minMountedFraction, err := readyMinMountedFractionFromEnv()
	if err != nil {
		return nil, err
	}

	repo, err := openShaleRepoFromEnv(logger, relayRecv.Register)
	if err != nil {
		return nil, err
	}
	// THROWAWAY (never merge): wire the LEGACY site repo so a normal deploy
	// creates a pre-collapse row, which is the fixture the migration needs and
	// which no shipping build can produce any more.
	sites := storage.NewShaleSiteRepo(repo)
	bundle := &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		// A directory is an artifact, sharing the same cluster (shard routing
		// + per-shard CAS) as every other. The old site family is the
		// read-only fallback, so deploys predating the collapse keep serving
		// until they are migrated.
		Sites: sites,
		// ShaleRoomRepo shares the same cluster, co-locating every room family
		// on the {app-slug} shard.
		Rooms: storage.NewShaleRoomRepo(repo),
		// Gate /readyz on the cluster's mount floor so a rollout stalls on a
		// pod that cannot mount its storage instead of surging past it. The
		// fraction semantics (0 = no floor, desired == 0 vacuously ready) live
		// in the shale predicate.
		Readiness: shaleReadinessProber{repo: repo, minMountedFraction: minMountedFraction},
		Close:     repo.Close,
	}
	logger.Printf("readiness: /readyz mount floor minMountedFraction=%g (0 disables the floor)", minMountedFraction)
	// Multi-node: supply the relay peer transport. The publisher fans every
	// frame out to the CURRENT peer set, discovered per publish from the ring
	// membership the cluster gossips (self excluded), adapted onto the relay's
	// narrow Peers port so the relay stays storage-agnostic. main wires
	// Publisher + Bind into the relay and Closes the publisher at shutdown.
	if repo.GRPCAddr() != "" {
		pub := relaygrpc.NewPublisher(shalePeers{repo: repo}, relaygrpc.PublisherConfig{Logf: logger.Printf})
		bundle.RelayPeer = &relayPeerTransport{
			Publisher: pub,
			Bind:      func(deliver func(relay.RoomKey, relay.Frame)) { relayRecv.Bind(deliver) },
			Close:     pub.Close,
		}
		logger.Printf("relay: multi-pod peer transport ready (peer service on the cluster gRPC server at %s; discovery via ring membership)", repo.GRPCAddr())
	}

	// Transactional shale-blob seam: when the repo opened a blob plane, supply
	// the shaleblob.Unit (pointer co-commits with metadata) + schedule
	// SweepOrphans for orphan-bytes reclamation.
	if repo.HasBlobPlane() {
		unit, uErr := shaleblob.New(repo)
		if uErr != nil {
			_ = repo.Close()
			return nil, fmt.Errorf("build shale blob unit: %w", uErr)
		}
		bundle.BlobUnit = unit
		bundle.BlobOrphanSweeper = repo
	}
	// Shale writes span two shards, so it is the one backend that can be left
	// with a half-finished write to settle.
	bundle.IntentSweeper = repo

	// OPTIONAL live-diagnosis endpoint. With HOSTTHIS_SHALE_DEBUG_ADDR set (e.g.
	// ":6060"), serve the embedded cluster's per-position handoff dump at
	// /debug/shale/state, for diagnosing a stuck position (desired-but-unmounted,
	// a parked handoff phase, the last swallowed acquire error).
	if dbgAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_DEBUG_ADDR")); dbgAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/shale/state", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, repo.DebugClusterState())
		})
		srv := &http.Server{Addr: dbgAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Printf("metadata: shale debug server on %s exited: %v", dbgAddr, err)
			}
		}()
		logger.Printf("metadata: shale debug endpoint serving %s/debug/shale/state", dbgAddr)
	}

	return bundle, nil
}

// shalePeers adapts the ShaleRepo's gossiped ring-membership view onto the
// relay's Peers port (current peer gRPC addresses, self excluded). Defined at
// the composition root so storage stays relay-agnostic and the relay stays
// storage-agnostic.
type shalePeers struct{ repo *storage.ShaleRepo }

func (p shalePeers) Addresses() []string { return p.repo.PeerGRPCAddrs() }

// stripScheme removes a leading http:// or https:// from a metadata S3 endpoint
// so blobstore.Config gets the bare host:port it wants: the metadata config
// carries the full URL, the blobstore adapter takes EndpointHost + UseSSL
// separately.
func stripScheme(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// parseSeeds splits a comma-separated HOSTTHIS_SHALE_SEEDS value into peer bind
// addresses, dropping empty entries so a trailing comma or an all-whitespace
// value yields nil, which cluster.Open reads as "this node is the seed".
func parseSeeds(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}
