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
		ReplicationFactor: replicationFactor,
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	logger.Printf("metadata: shale bucket=%s db=%s rf=%d endpoint=%s", bucket, dbName, replicationFactor, endpoint)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		Close:   repo.Close,
	}, nil
}
