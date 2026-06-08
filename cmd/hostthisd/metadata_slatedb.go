// metadata_slatedb.go — slatedb-backed metadataBundle. Active only
// when the binary is built with -tags slatedb.
//
// Reads connection config from env vars:
//   HOSTTHIS_METADATA_S3_ENDPOINT   (e.g. http://minio:9000)
//   HOSTTHIS_METADATA_S3_BUCKET     (required)
//   HOSTTHIS_METADATA_S3_REGION     (default us-east-1)
//   HOSTTHIS_METADATA_S3_ACCESS_KEY (required)
//   HOSTTHIS_METADATA_S3_SECRET_KEY (required)
//   HOSTTHIS_METADATA_S3_USE_SSL    (true|false; default true)
//   HOSTTHIS_METADATA_DB_NAME       (default "hostthis-metadata")

//go:build slatedb

package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataSlate(logger *log.Logger) (*metadataBundle, error) {
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")

	if bucket == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_BUCKET is required for slatedb backend")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}

	repo, err := storage.NewSlateRepo(storage.SlateConfig{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		UseSSL:    useSSL,
		DbName:    dbName,
	})
	if err != nil {
		return nil, fmt.Errorf("open slatedb: %w", err)
	}
	logger.Printf("metadata: slatedb bucket=%s db=%s endpoint=%s", bucket, dbName, endpoint)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		Close:   repo.Close,
	}, nil
}
