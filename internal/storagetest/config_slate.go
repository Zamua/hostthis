//go:build slatedb

package storagetest

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

// config points at the object store, on a logical db unique per call so
// concurrent tests never see each other's keys.
func config(t *testing.T) storage.ShaleConfig {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping (start dev MinIO first)")
	}
	return storage.ShaleConfig{
		Endpoint:  endpoint,
		Region:    "us-east-1",
		Bucket:    envOr("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
		AccessKey: envOr("MINIO_TEST_ACCESS_KEY", "admin"),
		SecretKey: envOr("MINIO_TEST_SECRET_KEY", "supersecret"),
		DbName:    fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), seq.Load()+1),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
