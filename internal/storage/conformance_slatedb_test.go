//go:build slatedb

package storage_test

// SlateDB backend's entry into the backend-agnostic conformance suite.
//
// Needs cgo + libslatedb on the loader path (like the rest of the slatedb
// build) and a live S3-compatible endpoint, so it skips unless
// MINIO_TEST_ENDPOINT is set. Bring one up with the dev MinIO compose, then:
//
//	go test -tags slatedb -run TestConformance_Slate ./internal/storage
//
// Each subtest gets a FRESH logical DbName (a per-run prefix within the bucket)
// so runs cannot see each other's keys and the "empty repo" assertions hold.

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

	caps := conformCaps{StrictQuotaUnderConcurrency: true, StrictIdentityQuotaUnderConcurrency: true}
	newSlate := func(t *testing.T) *storage.SlateRepo {
		// Run epoch plus a monotonic counter: unique per call so each
		// subtest starts empty, and unique per run so concurrent CI runs
		// against the shared bucket cannot collide.
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
	}
	newRepo := func(t *testing.T) conformanceRepo { return newSlate(t) }
	// The site repo wraps the SAME SlateRepo, so the cross-quota and
	// cross-family-slug subtests exercise the real interaction rather than
	// two independent stores.
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		repo := newSlate(t)
		return repo, storage.NewSlateSiteRepo(repo)
	}
	// Room, paste and site repos all wrap the SAME SlateRepo, so the
	// cross-kind service-wide cap subtest sees one shared store.
	newRooms := func(t *testing.T) roomConformanceStores {
		repo := newSlate(t)
		return roomConformanceStores{
			Rooms: storage.NewSlateRoomRepo(repo),
			Paste: repo,
			Site:  storage.NewSlateSiteRepo(repo),
		}
	}
	runConformanceWithSites(t, "slatedb", caps, newRepo, newSites, newRooms)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
