//go:build slatedb

package storage_test

// Shared test support for the shale migration + reconciler tests: a
// repo factory that opens a fresh single-node ShaleRepo on a unique
// logical db within the shared MinIO test bucket. Each call gets its own
// DbName so the tests never see each other's keys. Mirrors the
// per-subtest factory the conformance file uses, lifted out so both the
// migration and reconciler tests share one setup.

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

var shaleSupportSeq atomic.Int64

// uniqueShaleConfig builds a ReplicationFactor=1 ShaleConfig on a logical
// db unique to this call, within the shared MinIO test bucket. The same
// config drives both storage.NewShaleRepo AND a raw slate.New backend, so
// a test can seed/transform a bucket through the raw backend and then read
// it back through ShaleRepo on the SAME DbName (sequential single-writer:
// SlateDB is single-writer-per-db, so the two handles must not be open at
// the same time). The caller is responsible for the MINIO_TEST_ENDPOINT
// skip gate before invoking.
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

// newShaleRepoOnUniqueDB opens a ReplicationFactor=1 shale cluster over
// the slate backend at endpoint, on a logical db unique to this call.
// Registers a Cleanup that closes the repo. The caller is responsible
// for the MINIO_TEST_ENDPOINT skip gate before invoking.
func newShaleRepoOnUniqueDB(t *testing.T, endpoint string) *storage.ShaleRepo {
	t.Helper()
	repo, err := storage.NewShaleRepo(uniqueShaleConfig(endpoint))
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
