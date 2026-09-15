package storage_test

import (
	"os"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

func envOrDefaultTest(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// TestS3BlobContract runs the object store contract against a real S3 API.
// Skipped unless MINIO_TEST_ENDPOINT is set.
func TestS3BlobContract(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping the s3 blob store tests")
	}
	runBlobContract(t, func(t *testing.T) rawBlobStore {
		bs, err := storage.NewS3BlobStore(storage.S3BlobConfig{
			Endpoint:  endpoint,
			Bucket:    envOrDefaultTest("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
			Region:    "us-east-1",
			AccessKey: envOrDefaultTest("MINIO_TEST_ACCESS_KEY", "admin"),
			SecretKey: envOrDefaultTest("MINIO_TEST_SECRET_KEY", "supersecret"),
		})
		if err != nil {
			t.Fatalf("new s3 blob store: %v", err)
		}
		return bs
	})
}
