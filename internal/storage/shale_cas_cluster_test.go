// CAS-coordinator cluster gate (-tags slatedb, MinIO-backed units). Pins that
// ShaleConfig.Coordinator = CoordinatorCAS stands up a multi-node cluster
// coordinating over ONE membership document in the shared ConditionalStore:
// no bind address, no seeds, identical config per node. See docs/SPEC.md
// "Coordination is a pluggable choice".
//
// The shared store is a MemConditionalStore standing in for the production
// MinIO-backed one (exactly as the homogeneous gate does); it keys verbatim,
// so the document sits at the adapter's default "__coord/members". The
// <dbName>/ prefix is a property of the MinIO store's KeyPrefix.
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

// startCASNode brings up a node via the production storage.NewShaleRepo path
// with the CAS coordinator over the shared store. No bind addr, no seeds:
// beyond NodeID every node's config is identical.
func startCASNode(t *testing.T, id, dbName string, cs storageunit.ConditionalStore, unitCount, rf int) *rebalNode {
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
		Coordinator:       storage.CoordinatorCAS,
		GRPCAddr:          "127.0.0.1:0", // OS-assigned; the repo serves the actual port
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

// TestShaleCASCluster_TwoNodesOneDocument pins the CAS coordination path end
// to end over the real backend:
//
//   - Node A starts with nothing but the shared store and forms; its row
//     appears in the membership document at the adapter's default key.
//   - Node B starts with the IDENTICAL config; it joins by CAS-ing its row
//     into the same document, and both nodes converge on a 2-member ring
//     with no memberlist port anywhere.
//   - A write through A is readable through B, so ring routing + gRPC
//     forwarding run over the CAS-coordinated membership.
func TestShaleCASCluster_TwoNodesOneDocument(t *testing.T) {
	if os.Getenv("MINIO_TEST_ENDPOINT") == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping CAS-coordinator cluster gate (start dev MinIO first)")
	}

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	dbName := fmt.Sprintf("casco-%d", time.Now().UnixNano()) // fresh prefix: clean form-from-empty
	const units = 4

	cs := storageunit.NewMemConditionalStore()
	nodeA := startCASNode(t, "casco-A", dbName, cs, units, 2)

	if nodeA.repo.BindAddr() != "" {
		t.Fatalf("cas coordinator must run no memberlist, got bind addr %q", nodeA.repo.BindAddr())
	}
	if nodeA.repo.GRPCAddr() == "" {
		t.Fatal("multi-node cas cluster must bind and advertise a gRPC forwarding address")
	}
	data, _, err := cs.Get("__coord/members")
	if err != nil {
		t.Fatalf("expected the membership document after Start, got error: %v", err)
	}
	if !strings.Contains(string(data), "casco-A") {
		t.Fatalf("membership document does not carry node A's row: %q", data)
	}

	nodeB := startCASNode(t, "casco-B", dbName, cs, units, 2)
	waitMembers(t, []*rebalNode{nodeA, nodeB}, 2, 30*time.Second)

	data, _, err = cs.Get("__coord/members")
	if err != nil {
		t.Fatalf("read membership document after join: %v", err)
	}
	for _, id := range []string{"casco-A", "casco-B"} {
		if !strings.Contains(string(data), id) {
			t.Fatalf("membership document does not carry %s's row: %q", id, data)
		}
	}

	// R=2 round-trip: write through A, read through B. The slug uses only
	// SlugAlphabet chars (no l/o/0/1).
	p := pasteFor("cas23456", "key:alice", "cas one", 123, now)
	mustInsert(t, nodeA.repo, p, 0)

	got, err := nodeB.repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("read cas23456 through node B: %v", err)
	}
	if got.Slug != p.Slug || got.Size != p.Size {
		t.Fatalf("round-trip mismatch through node B: got slug=%q size=%d, want slug=%q size=%d",
			got.Slug, got.Size, p.Slug, p.Size)
	}
}
