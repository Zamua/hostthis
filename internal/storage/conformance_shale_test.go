//go:build slatedb

package storage_test

// Shale backend's entry into the backend-agnostic conformance suite.
//
// Built only under -tags slatedb, which also needs cgo + libslatedb on the
// loader path: the shale cluster wraps the same SlateDB engine. A live
// S3-compatible endpoint backs the store, so it skips unless
// MINIO_TEST_ENDPOINT is set.
//
//	go test -tags slatedb -run TestConformance_Shale ./internal/storage
//
// Each subtest gets a FRESH logical DbName and a fresh single-node cluster, so
// runs cannot see each other's keys and the "empty repo" assertions hold.
//
// StrictIdentityQuotaUnderConcurrency is false because the per-identity quota
// is a scan over the authoritative rows that is NOT atomic with the write, so
// the cap admits a bounded overshoot under same-owner concurrency. The per-room
// cap stays strict via its own single-shard CAS (docs/SPEC.md "Scan-derived
// per-identity quota").

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

var shaleConformSeq atomic.Int64

// A missing or renamed method fails the tagged build here rather than as an
// opaque type error inside the factory closures below.
var _ conformanceRepo = (*storage.ShaleRepo)(nil)
var _ conformanceSiteRepo = (*storage.ShaleSiteRepo)(nil)
var _ conformanceRoomRepo = (*storage.ShaleRoomRepo)(nil)

func TestConformance_Shale(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale conformance (start dev MinIO first)")
	}
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	newShale := func(t *testing.T) *storage.ShaleRepo {
		// Unique per-call logical db so each subtest starts empty within the
		// shared bucket. Epoch nanos + a monotonic counter keeps concurrent CI
		// runs from colliding.
		seq := shaleConformSeq.Add(1)
		dbName := fmt.Sprintf("conform-shale-%d-%d", time.Now().UnixNano(), seq)
		repo, err := storage.NewShaleRepo(storage.ShaleConfig{
			// A stable per-call NodeID keeps the cluster's identity
			// deterministic; ReplicationFactor=1 is the single-owner-per-key
			// path the conformance suite exercises.
			NodeID:            fmt.Sprintf("conform-node-%d", seq),
			Endpoint:          endpoint,
			Region:            "us-east-1",
			Bucket:            bucket,
			AccessKey:         access,
			SecretKey:         secret,
			UseSSL:            false,
			DbName:            dbName,
			ReplicationFactor: 1,
		})
		if err != nil {
			t.Fatalf("NewShaleRepo: %v", err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		return repo
	}
	caps := conformCaps{StrictQuotaUnderConcurrency: true, StrictIdentityQuotaUnderConcurrency: false}
	newRepo := func(t *testing.T) conformanceRepo { return newShale(t) }
	// The site repo wraps the SAME ShaleRepo (one cluster handle, one shard
	// routing), so the cross-quota + cross-family-slug subtests exercise the
	// real interaction rather than two isolated clusters.
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		repo := newShale(t)
		return repo, storage.NewShaleSiteRepo(repo)
	}
	// Room, paste and site repos all wrap the SAME ShaleRepo, so the cross-kind
	// service-wide cap subtest exercises the real interaction.
	newRooms := func(t *testing.T) roomConformanceStores {
		repo := newShale(t)
		return roomConformanceStores{
			Rooms: storage.NewShaleRoomRepo(repo),
			Paste: repo,
			Site:  storage.NewShaleSiteRepo(repo),
		}
	}
	runConformanceWithSites(t, "shale", caps, newRepo, newSites, newRooms)
}

// envOrDefault reads an override, falling back when unset.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
