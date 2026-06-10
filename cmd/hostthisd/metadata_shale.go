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
//
// With HOSTTHIS_SHALE_BIND_ADDR unset (the default) the node runs the
// single-node path exactly as before: no gossip, no ring routing, every
// op local. Setting it brings up memberlist + gRPC forwarding and joins
// the ring (docs/SPEC.md "Multi-node shale").
//
// ReadConsistency is fixed at cluster.ReadNearest inside
// storage.NewShaleRepo (ShaleConfig exposes no knob for it), so there is
// no env var for it here.

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
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	if bindAddr == "" {
		logger.Printf("metadata: shale (single-node) node=%s bucket=%s db=%s rf=%d endpoint=%s",
			nodeID, bucket, dbName, replicationFactor, endpoint)
	} else {
		logger.Printf("metadata: shale (multi-node) node=%s bind=%s grpc=%s seeds=%d rf=%d bucket=%s db=%s endpoint=%s",
			nodeID, bindAddr, grpcAddr, len(seeds), replicationFactor, bucket, dbName, endpoint)
	}
	return &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		Close:   repo.Close,
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
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}
