// Homogeneous-bootstrap gate (-tags slatedb, MinIO-backed). Pins the wiring
// that lets hostthis's embedded shale cluster form-vs-join at runtime against
// the __cluster/init marker instead of the founder/joiner seed asymmetry: a
// shared ConditionalStore flows through storage.NewShaleRepo into the cluster,
// AllowSoloStart derives from it, and the marker drives one-founder-then-join.
// See docs/SPEC.md "Homogeneous bootstrap (optional)".
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

// startHomogNode brings up a multi-backend node via the production
// storage.NewShaleRepo path with a shared ConditionalStore wired (the
// homogeneous bootstrap). Mirrors startRebalNode but takes the cond store,
// seed list, unit count, and replication factor explicitly so a test can
// drive the form-vs-join marker decision.
func startHomogNode(t *testing.T, id, dbName string, seeds []string, cs storageunit.ConditionalStore, unitCount, rf int) *rebalNode {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	bindAddr := loopback(freeTCPPort(t))

	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            id,
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            bucket,
		AccessKey:         access,
		SecretKey:         secret,
		UseSSL:            false,
		DbName:            dbName,
		BindAddr:          bindAddr,
		GRPCAddr:          "127.0.0.1:0", // OS-assigned; the repo serves the actual port
		Seeds:             seeds,
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
		bindAddr: bindAddr,
		grpcAddr: repo.GRPCAddr(),
	}
	t.Cleanup(n.close)
	return n
}

// TestShaleHomogeneous_MarkerBootstrap proves the homogeneous path end to end
// over the real backend:
//
//   - Node A is configured with a seed it CANNOT reach. Without a
//     ConditionalStore that config fails Open (the cluster refuses to fork a
//     joiner whose seeds are unreachable). WITH the shared store wired,
//     AllowSoloStart lets it come up solo and FORM the cluster by writing the
//     __cluster/init marker.
//   - The marker records the durable {gen:0, count:N}.
//   - Node B, carrying A's real address and the SAME store, joins gossip and
//     reads the marker (adopts gen 0, no second form).
//   - A write through A is readable through B: R=2 routing over the
//     homogeneously-bootstrapped ring round-trips.
func TestShaleHomogeneous_MarkerBootstrap(t *testing.T) {
	if os.Getenv("MINIO_TEST_ENDPOINT") == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping homogeneous bootstrap gate (start dev MinIO first)")
	}

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	epoch := time.Now().UnixNano()
	dbName := fmt.Sprintf("homog-%d", epoch) // fresh prefix: clean form-from-empty
	const units = 4

	// Shared CAS arbiter: the in-process MemConditionalStore stands in for the
	// MinIO-backed store every pod shares in production. It stores keys
	// verbatim (the prefix namespacing is a MinioConditionalStore property), so
	// the marker key is the bare "__cluster/init".
	cs := storageunit.NewMemConditionalStore()

	// A seed that resolves but has nothing listening: a free port we grab and
	// immediately let go (freeTCPPort closes its probe listener). Node A's join
	// to it fails, so only AllowSoloStart (active because the store is wired)
	// keeps Open from erroring.
	deadSeed := loopback(freeTCPPort(t))
	nodeA := startHomogNode(t, "homog-A", dbName, []string{deadSeed}, cs, units, 2)

	// The founder wrote the marker as {gen:0, count:units}.
	data, _, err := cs.Get("__cluster/init")
	if err != nil {
		t.Fatalf("expected __cluster/init marker after solo form, got error: %v", err)
	}
	if !strings.Contains(string(data), `"count":4`) {
		t.Fatalf("marker should record unit count %d, got %q", units, data)
	}

	// Node B seeds off A's real address + shares the store: joins gossip and
	// reads the marker (adopts gen 0). No second form.
	nodeB := startHomogNode(t, "homog-B", dbName, []string{nodeA.bindAddr}, cs, units, 2)
	waitMembers(t, []*rebalNode{nodeA, nodeB}, 2, 30*time.Second)

	// R=2 round-trip across the homogeneously-bootstrapped ring: write through
	// A, read through B. Slug uses only SlugAlphabet chars (no l/o/0/1).
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
