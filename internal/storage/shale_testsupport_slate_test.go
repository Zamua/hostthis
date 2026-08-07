//go:build slatedb

package storage_test

// Shared shale test support: a factory for a single-node ShaleRepo on a logical
// db unique per call, within the shared MinIO test bucket, so concurrent tests
// never see each other's keys.

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

var shaleSupportSeq atomic.Int64

// uniqueShaleConfig builds a ReplicationFactor=1 ShaleConfig on a logical db
// unique to this call. The same config drives both storage.NewShaleRepo and a
// raw slate.New backend, so a test can seed through one and read back through
// the other; SlateDB is single-writer-per-db, so the two handles must never be
// open at the same time. Caller owns the MINIO_TEST_ENDPOINT skip gate.
func uniqueShaleConfig(endpoint string) storage.ShaleConfig {
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	seq := shaleSupportSeq.Add(1)
	dbName := fmt.Sprintf("shale-support-%d-%d", time.Now().UnixNano(), seq)
	return storage.ShaleConfig{
		NodeID:            fmt.Sprintf("support-node-%d", seq),
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            bucket,
		AccessKey:         access,
		SecretKey:         secret,
		UseSSL:            false,
		DbName:            dbName,
		ReplicationFactor: 1,
	}
}

// newShaleRepoOnUniqueDB opens a ReplicationFactor=1 shale cluster over the
// slate backend at endpoint, closing it via t.Cleanup. Caller owns the
// MINIO_TEST_ENDPOINT skip gate.
func newShaleRepoOnUniqueDB(t *testing.T, endpoint string) *storage.ShaleRepo {
	t.Helper()
	repo, err := storage.NewShaleRepo(uniqueShaleConfig(endpoint))
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// newShaleRepoForTest is the slate build's engine factory: a real object store
// is required, so a run without one skips rather than pretending.
func newShaleRepoForTest(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale test (start dev MinIO first)")
	}
	return newShaleRepoOnUniqueDB(t, endpoint)
}
