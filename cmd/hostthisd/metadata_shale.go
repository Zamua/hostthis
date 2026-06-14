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
	"os"
	"strings"

	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataShale(logger *log.Logger) (*metadataBundle, error) {
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
		CacheBytes:        uint64(cacheBytes),
		Logger:            logger,
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	if bindAddr == "" {
		logger.Printf("metadata: shale (single-node) node=%s bucket=%s db=%s rf=%d awaitDurable=%t endpoint=%s",
			nodeID, bucket, dbName, replicationFactor, awaitDurable, endpoint)
	} else {
		logger.Printf("metadata: shale (multi-node) node=%s bind=%s grpc=%s seeds=%d rf=%d awaitDurable=%t bucket=%s db=%s endpoint=%s",
			nodeID, bindAddr, grpcAddr, len(seeds), replicationFactor, awaitDurable, bucket, dbName, endpoint)
	}
	return &metadataBundle{
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
	}, nil
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
