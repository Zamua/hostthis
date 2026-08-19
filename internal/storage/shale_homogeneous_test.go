// Homogeneous-bootstrap gate (-tags slatedb, MinIO-backed). Pins that a shared
// ConditionalStore flows through storage.NewShaleRepo into the cluster, that
// AllowSoloStart derives from it, and that the __cluster/init marker drives
// one-founder-then-join. See docs/SPEC.md "Homogeneous bootstrap (optional)".
//
//go:build slatedb

package storage_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/storageunit"

	"github.com/Zamua/hostthis/internal/storage"
)

// startHomogNode brings up a node via the production storage.NewShaleRepo path
// with a shared ConditionalStore wired, taking the cond store, unit count,
// and replication factor explicitly so a test can drive the form-vs-join
// marker decision.
func startHomogNode(t *testing.T, id, dbName string, cs storageunit.ConditionalStore, unitCount, rf int) *rebalNode {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            id,
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            bucket,
		AccessKey:         access,
		SecretKey:         secret,
		UseSSL:            false,
		DbName:            dbName,
		GRPCAddr:          "127.0.0.1:0", // OS-assigned; the repo serves the actual port
		Coordinator:       storage.CoordinatorCAS,
		ReplicationFactor: rf,
		UnitCount:         unitCount,
		ConditionalStore:  cs,
	})
	if err != nil {
		t.Fatalf("node %s: NewShaleRepo: %v", id, err)
	}

	n := &rebalNode{
		id:       id,
		repo:     repo,
		grpcAddr: repo.GRPCAddr(),
	}
	t.Cleanup(n.close)
	return n
}

// TestShaleHomogeneous_MarkerBootstrap pins the homogeneous path end to end
// over the real backend:
//
//   - Node A comes up alone against an empty store and FORMS the cluster by
//     writing the __cluster/init marker.
//   - The marker records the durable {gen:0, count:N}.
//   - Node B, sharing the SAME store, joins through the membership document
//     and reads the marker (adopts gen 0, no second form).
//
// The unreachable-seed premise this test once carried retired with the gossip
// adapter: seeds no longer exist, so AllowSoloStart has no unreachable-join to
// rescue and there is nothing left to pin there.
//   - A write through A is readable through B, so R=2 routing over the
//     homogeneously-bootstrapped ring round-trips.
func TestShaleHomogeneous_MarkerBootstrap(t *testing.T) {
	if os.Getenv("MINIO_TEST_ENDPOINT") == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping homogeneous bootstrap gate (start dev MinIO first)")
	}

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	epoch := time.Now().UnixNano()
	dbName := fmt.Sprintf("homog-%d", epoch) // fresh prefix: clean form-from-empty
	const units = 4

	// Shared CAS arbiter standing in for the MinIO-backed store every pod
	// shares in production. It stores keys verbatim (prefix namespacing is a
	// MinioConditionalStore property), so the marker key is the bare
	// "__cluster/init".
	cs := storageunit.NewMemConditionalStore()

	nodeA := startHomogNode(t, "homog-A", dbName, cs, units, 2)

	data, _, err := cs.Get("__cluster/init")
	if err != nil {
		t.Fatalf("expected __cluster/init marker after solo form, got error: %v", err)
	}
	if !strings.Contains(string(data), `"count":4`) {
		t.Fatalf("marker should record unit count %d, got %q", units, data)
	}

	// Node B shares the store, so it joins through the membership document
	// and adopts gen 0 from the marker rather than forming a second cluster.
	nodeB := startHomogNode(t, "homog-B", dbName, cs, units, 2)
	waitMembers(t, []*rebalNode{nodeA, nodeB}, 2, 30*time.Second)

	// R=2 round-trip: write through A, read through B. The slug uses only
	// SlugAlphabet chars (no l/o/0/1).
	p := pasteFor("hmg23456", "key:alice", "homog one", 123, now)
	mustInsert(t, nodeA.repo, p, 0)

	got, err := nodeB.repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("read hmg23456 through node B: %v", err)
	}
	if got.Slug != p.Slug || got.Size != p.Size {
		t.Fatalf("round-trip mismatch through node B: got slug=%q size=%d, want slug=%q size=%d",
			got.Slug, got.Size, p.Slug, p.Size)
	}
}
