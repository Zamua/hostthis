package storage_test

import (
	"fmt"
	"os"
	"testing"
	"time"

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
	runBlobContract(t, func(t *testing.T) blobBackend {
		// A per-run legacy namespace, so a rerun cannot see the previous run's
		// objects and the "absent" assertions stay meaningful.
		legacy := fmt.Sprintf("blobtest/%d", time.Now().UnixNano())
		bs, err := storage.NewS3BlobStore(storage.S3BlobConfig{
			Endpoint:     endpoint,
			Bucket:       envOrDefaultTest("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
			Region:       "us-east-1",
			AccessKey:    envOrDefaultTest("MINIO_TEST_ACCESS_KEY", "admin"),
			SecretKey:    envOrDefaultTest("MINIO_TEST_SECRET_KEY", "supersecret"),
			LegacyPrefix: legacy,
		})
		if err != nil {
			t.Fatalf("new s3 blob store: %v", err)
		}
		t.Cleanup(func() { _ = bs.DeletePrefix(legacy + "/") })
		return blobBackend{
			store:     bs,
			legacyKey: func(sha string) string { return legacy + "/" + sha[:2] + "/" + sha },
		}
	})
}
