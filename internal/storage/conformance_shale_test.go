//go:build slatedb

package storage_test

// Shale backend's entry into the backend-agnostic conformance suite.
//
// Built only under -tags slatedb (which also needs cgo + libslatedb on
// the loader path: the shale cluster wraps the slate backend, which is
// the same SlateDB engine the slatedb conformance run uses). It needs a
// live S3-compatible endpoint to back the store, so it skips cleanly
// unless MINIO_TEST_ENDPOINT is set, mirroring conformance_slatedb_test.go.
//
//	go test -tags slatedb -run TestConformance_Shale ./internal/storage
//
// Each subtest gets a FRESH logical DbName (a per-run prefix within the
// bucket) and a fresh single-node cluster so runs don't see each other's
// keys and the "empty repo" assertions hold. The shale backend declares
// conformCaps{ExpiryFreesQuotaAtReadTime: false}: its identity_bytes
// reservation counter sheds an expired paste's bytes at sweep time, not
// read time (docs/SPEC.md "One intentional behavior change").

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

var shaleConformSeq atomic.Int64

// Compile-time assertion that *ShaleRepo satisfies the full conformance
// interface (the union of the four service-layer interfaces). A
// missing or renamed method fails the tagged build here rather than as
// an opaque type error inside the factory closure below.
var _ conformanceRepo = (*storage.ShaleRepo)(nil)

// Compile-time assertion that *ShaleSiteRepo satisfies the site
// conformance interface (service.SiteRepo + service.SweepSites), so the
// shale backend can supply a non-nil bundle Sites and run the site
// subtests the same way sqlite + slatedb do.
var _ conformanceSiteRepo = (*storage.ShaleSiteRepo)(nil)

// Compile-time assertion that *ShaleRoomRepo satisfies the room conformance
// interface (service.RoomRepo + service.SweepRooms), so the shale backend can
// supply a non-nil bundle Rooms and run the room subtests the same way sqlite
// + slatedb do.
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
		// Unique per-call logical db so each subtest starts empty within
		// the shared bucket. Run epoch (nanos) + a monotonic counter keeps
		// concurrent CI runs from colliding.
		seq := shaleConformSeq.Add(1)
		dbName := fmt.Sprintf("conform-shale-%d-%d", time.Now().UnixNano(), seq)
		repo, err := storage.NewShaleRepo(storage.ShaleConfig{
			// Single in-process node. A stable per-call NodeID keeps the
			// cluster's identity deterministic; ReplicationFactor=1 is the
			// single-owner-per-key path the conformance suite exercises.
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
	caps := conformCaps{ExpiryFreesQuotaAtReadTime: false, StrictQuotaUnderConcurrency: true}
	newRepo := func(t *testing.T) conformanceRepo { return newShale(t) }
	// The site repo (ShaleSiteRepo) wraps the SAME ShaleRepo (same cluster
	// handle + shard routing), so the cross-quota + cross-family-slug site
	// subtests exercise the real interaction in one shale cluster.
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		repo := newShale(t)
		return repo, storage.NewShaleSiteRepo(repo)
	}
	// The room repo (ShaleRoomRepo), the paste repo, and the site repo all wrap
	// the SAME ShaleRepo (one cluster handle + shard routing), so the cross-kind
	// service-wide cap room subtest exercises the real interaction.
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
