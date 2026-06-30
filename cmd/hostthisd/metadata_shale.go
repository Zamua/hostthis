// metadata_shale.go - shale-cluster-backed metadataBundle. Active only
// when the binary is built with -tags slatedb (the shale backend wraps
// slatedb as its per-node KV engine, so it carries the same build/runtime
// requirements as the direct slatedb backend).
//
// Reads the same S3 connection config as the slatedb backend:
//   HOSTTHIS_METADATA_S3_ENDPOINT   (e.g. http://minio:9000)
//   HOSTTHIS_METADATA_S3_BUCKET     (required)
//   HOSTTHIS_METADATA_S3_REGION     (default us-east-1)
//   HOSTTHIS_METADATA_S3_ACCESS_KEY (required)
//   HOSTTHIS_METADATA_S3_SECRET_KEY (required)
//   HOSTTHIS_METADATA_S3_USE_SSL    (true|false; default true)
//   HOSTTHIS_METADATA_DB_NAME       (default "hostthis-metadata")
//
// Plus the cluster-layer additions:
//   HOSTTHIS_NODE_ID                  (default os.Hostname(), or "hostthis-1")
//   HOSTTHIS_SHALE_REPLICATION_FACTOR (default 1)
//   HOSTTHIS_SHALE_BIND_ADDR          (host:port; NON-EMPTY enables multi-node mode)
//   HOSTTHIS_SHALE_GRPC_ADDR          (host:port; required when BIND_ADDR is set)
//   HOSTTHIS_SHALE_SEEDS              (comma-separated peer BIND_ADDRs; empty = seed node)
//   HOSTTHIS_METADATA_AWAIT_DURABLE   (true|false; default true; false = relaxed
//                                      durability/fast-ack, only safe at RF>=2)
//
// With HOSTTHIS_SHALE_BIND_ADDR unset (the default) the node runs the
// single-node path exactly as before: no gossip, no ring routing, every
// op local. Setting it brings up memberlist + gRPC forwarding and joins
// the ring (docs/SPEC.md "Multi-node shale").
//
// ReadConsistency is fixed at cluster.ReadQuorum inside
// storage.NewShaleRepo (ShaleConfig exposes no knob for it), so there is
// no env var for it here. Quorum is required at R>1 (ReadNearest could
// return a still-backfilling replica's NotFound for live data); at R=1 a
// quorum is the single replica, so it is behavior-identical there.

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

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
	slatedb "slatedb.io/slatedb-go/uniffi"
)

// slatedbLogLevel maps HOSTTHIS_SLATEDB_LOG_LEVEL to a slatedb LogLevel.
// Empty / "off" => no slatedb tracing (the default). Diagnostic only:
// enabling debug/trace is verbose and meant for short-lived investigation.
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

func buildMetadataShale(retention domain.Retention, logger *log.Logger) (*metadataBundle, error) {
	// Optional slatedb tracing (to stderr) for diagnosing the SST-read
	// pattern. Off unless HOSTTHIS_SLATEDB_LOG_LEVEL is set.
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
	// Backend shape: 0 (default) = single slatedb DB per node (today). A power
	// of two = MULTI-BACKEND sharded mode: that many slatedb units distributed
	// across the ring, routed per key. Each unit is a full slatedb instance per
	// owning replica, so keep small on the RAM-tight boxes. See docs/SPEC.md
	// "Sharded metadata (multi-backend mode)".
	unitCount := envOrInt("HOSTTHIS_SHALE_UNIT_COUNT", 0)
	if unitCount < 0 {
		unitCount = 0
	}
	// Durability mode: default true (ack after the durable object-store
	// flush). false = relaxed durability (fast-ack at the memtable), the big
	// write-throughput win; only safe at RF>=2 on separate nodes. See
	// docs/SPEC.md "Relaxed durability: fast-ack at the memtable".
	awaitDurable := strings.EqualFold(envOr("HOSTTHIS_METADATA_AWAIT_DURABLE", "true"), "true")
	// Block cache for the slatedb metadata layer (Moka in-memory). Without it
	// slatedb has NO block cache and re-fetches SST blocks from MinIO on every
	// read - a steady self-inflicted read storm against the object store.
	// Default 128 MiB; set 0 to disable. Tunable on the RAM-tight boxes.
	cacheBytes := envOrInt("HOSTTHIS_METADATA_CACHE_BYTES", 128<<20)
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	// Fence-WAL GC: enable slatedb's fence-WAL garbage collector so the
	// per-open fence WAL objects don't accumulate unboundedly (slatedb ships
	// that GC category in dry-run by default; see storage.ShaleConfig.ReapFenceWALs).
	// Default ON for this long-lived server; HOSTTHIS_SLATEDB_FENCE_GC=false is
	// the kill-switch (fall back to slatedb's untouched defaults).
	reapFenceWALs := !strings.EqualFold(strings.TrimSpace(os.Getenv("HOSTTHIS_SLATEDB_FENCE_GC")), "false")

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

	// Optional transactional shale-blob plane. When HOSTTHIS_SHALE_BLOB_BUCKET
	// is set, blobs go THROUGH shale (the pointer co-commits with the metadata
	// on the owning shard) over a MinIO blob.Store pointed at that DISTINCT blob
	// bucket on the SAME object store the metadata uses. Unset keeps the
	// metadata-only path (the detached content-addressed store via
	// buildBlobStore stays the blob backend).
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

	// Optional HOMOGENEOUS bootstrap. When HOSTTHIS_SHALE_HOMOGENEOUS=true, build
	// a MinIO-backed ConditionalStore over the SAME metadata bucket, namespaced by
	// the DB name (so the __cluster/init marker is one shared object for every
	// pod), and hand it to the cluster. cluster.Open then decides form-vs-join at
	// runtime against the marker (try-join-else-form) instead of the founder/
	// joiner seed asymmetry; every pod runs identical config. Requires multi-
	// backend (sharded) + multi-node mode - the marker's durable {gen, count}
	// records the unit count, which is meaningless without sharding, and the
	// solo-start/form race only arises across gossiping pods. See docs/SPEC.md
	// "Homogeneous bootstrap (optional)". Unset keeps the seed-based bootstrap.
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
		Logger:            logger,
		BlobStore:         blobStore,
		ConditionalStore:  condStore,
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	repo.Retention = retention
	if bindAddr == "" {
		logger.Printf("metadata: shale (single-node) node=%s bucket=%s db=%s rf=%d shards=%d awaitDurable=%t fenceGC=%t blobBucket=%q endpoint=%s",
			nodeID, bucket, dbName, replicationFactor, unitCount, awaitDurable, reapFenceWALs, blobBucket, endpoint)
	} else {
		logger.Printf("metadata: shale (multi-node) node=%s bind=%s grpc=%s seeds=%d rf=%d shards=%d awaitDurable=%t fenceGC=%t homogeneous=%t blobBucket=%q bucket=%s db=%s endpoint=%s",
			nodeID, bindAddr, grpcAddr, len(seeds), replicationFactor, unitCount, awaitDurable, reapFenceWALs, homogeneous, blobBucket, bucket, dbName, endpoint)
	}
	bundle := &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		// Static-site hosting on shale: the ShaleSiteRepo adapter shares the
		// same shale cluster (shard routing + per-shard CAS) as the paste
		// repo, so a non-nil Sites lights up archive hosting on shale, the
		// same way it does on slatedb.
		Sites: storage.NewShaleSiteRepo(repo),
		// Room persistence on shale: the ShaleRoomRepo adapter shares the same
		// shale cluster, co-locating every room family on the {app-slug} shard
		// so the room tier runs on shale clusters too.
		Rooms: storage.NewShaleRoomRepo(repo),
		Close: repo.Close,
	}
	// Transactional shale-blob seam: when the repo opened a blob plane, supply
	// the shaleblob.Unit (pointer co-commits with metadata) + schedule
	// SweepOrphans for orphan-bytes reclamation. main picks these over the
	// standalone unit + the global content-addressed sweep.
	if repo.HasBlobPlane() {
		unit, uErr := shaleblob.New(repo)
		if uErr != nil {
			_ = repo.Close()
			return nil, fmt.Errorf("build shale blob unit: %w", uErr)
		}
		bundle.BlobUnit = unit
		bundle.BlobOrphanSweeper = repo
	}

	// OPTIONAL live-diagnosis endpoint. When HOSTTHIS_SHALE_DEBUG_ADDR is set
	// (e.g. ":6060"), serve the embedded cluster's per-position handoff dump at
	// /debug/shale/state - the production-safe equivalent of the shaled binary's
	// SHALE_DEBUG_ADDR endpoint, for diagnosing a stuck position (desired-but-
	// unmounted, a parked handoff phase, the last swallowed acquire error). OFF
	// unless the env var is set, so production is unaffected.
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

// stripScheme removes a leading http:// or https:// from a metadata S3 endpoint
// so the blobstore.Config gets the bare host:port it wants (the metadata config
// carries the full URL; the blobstore adapter takes EndpointHost + a UseSSL
// flag separately). A scheme-less endpoint is returned unchanged.
func stripScheme(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// parseSeeds splits a comma-separated HOSTTHIS_SHALE_SEEDS value into peer
// bind addresses, trimming whitespace around each entry and dropping empty
// ones (so a trailing comma or an all-whitespace value yields nil, which
// cluster.Open reads as "this node is the seed"). Returns nil for an empty
// input so the single-node path sees a nil Seeds slice unchanged.
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
