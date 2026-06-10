//go:build slatedb

package storage_test

// SlateDB backend's entry into the backend-agnostic conformance suite.
//
// Built only under `-tags slatedb` (which also needs cgo +
// libslatedb on the loader path, exactly like the rest of the slatedb
// build). It needs a live S3-compatible endpoint to back the store, so
// it skips cleanly unless MINIO_TEST_ENDPOINT is set: the same env-var
// gate the S3 blob round-trip test uses. Bring one up with the dev
// MinIO compose, then run:
//
//	go test -tags slatedb -run TestConformance_Slate ./internal/storage
//
// Each subtest gets a FRESH logical DbName (a per-run prefix within the
// bucket) so runs don't see each other's keys and the "empty repo"
// assertions hold.

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

var slateConformSeq atomic.Int64

func TestConformance_Slate(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping slatedb conformance (start dev MinIO first)")
	}
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	runConformance(t, "slatedb", conformCaps{ExpiryFreesQuotaAtReadTime: true, StrictQuotaUnderConcurrency: false}, func(t *testing.T) conformanceRepo {
		// Unique per-call logical db so each subtest starts empty within
		// the shared bucket. Run epoch (nanos) + a monotonic counter keeps
		// concurrent CI runs from colliding.
		dbName := fmt.Sprintf("conform-%d-%d", time.Now().UnixNano(), slateConformSeq.Add(1))
		repo, err := storage.NewSlateRepo(storage.SlateConfig{
			Endpoint:  endpoint,
			Region:    "us-east-1",
			Bucket:    bucket,
			AccessKey: access,
			SecretKey: secret,
			UseSSL:    false,
			DbName:    dbName,
		})
		if err != nil {
			t.Fatalf("NewSlateRepo: %v", err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		return repo
	})
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
