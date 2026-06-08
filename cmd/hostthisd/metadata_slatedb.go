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
	"net/url"
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

	// OpenDAL URL grammar:
	//   s3://<bucket>?endpoint=<url>&region=<r>&access_key_id=<id>&secret_access_key=<sk>&allow_http=true
	// allow_http=true is required when endpoint scheme is http (MinIO
	// dev clusters); harmless when https.
	q := url.Values{}
	if endpoint != "" {
		q.Set("endpoint", endpoint)
	}
	q.Set("region", region)
	q.Set("access_key_id", accessKey)
	q.Set("secret_access_key", secretKey)
	if !useSSL {
		q.Set("allow_http", "true")
	}
	objURL := fmt.Sprintf("s3://%s?%s", bucket, q.Encode())

	repo, err := storage.NewSlateRepo(storage.SlateConfig{
		ObjectStoreURL: objURL,
		DbName:         dbName,
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
