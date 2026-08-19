// CAS-coordinator config guards: pins that ShaleConfig.Coordinator refuses a
// nil ConditionalStore, gossip config, a missing GRPCAddr, and an unknown
// value. Untagged on purpose: every guard fires before the storage engine
// opens, so the pins run in any build with nothing installed. The positive
// path (a cluster actually coordinating over the document) needs multi-backend
// units and lives in shale_cas_cluster_test.go (-tags slatedb, MinIO).

package storage_test

import (
	"strings"
	"testing"

	"github.com/Zamua/shale/pkg/storageunit"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleCASCoordinator_ConfigGuards(t *testing.T) {
	t.Run("nil ConditionalStore is refused", func(t *testing.T) {
		_, err := storage.NewShaleRepo(storage.ShaleConfig{
			NodeID:      "cas-guard-1",
			DbName:      "cas-guard",
			Coordinator: storage.CoordinatorCAS,
			GRPCAddr:    "127.0.0.1:0",
		})
		if err == nil {
			t.Fatal("want error for cas coordinator with nil ConditionalStore, got nil")
		}
		if !strings.Contains(err.Error(), "ConditionalStore") {
			t.Fatalf("error %q does not name ConditionalStore", err)
		}
	})

	t.Run("missing GRPCAddr is refused", func(t *testing.T) {
		_, err := storage.NewShaleRepo(storage.ShaleConfig{
			NodeID:           "cas-guard-3",
			DbName:           "cas-guard",
			Coordinator:      storage.CoordinatorCAS,
			ConditionalStore: storageunit.NewMemConditionalStore(),
		})
		if err == nil {
			t.Fatal("want error for cas coordinator without a GRPCAddr, got nil")
		}
		if !strings.Contains(err.Error(), "GRPCAddr") {
			t.Fatalf("error %q does not name GRPCAddr", err)
		}
	})

	t.Run("unknown coordinator value is refused", func(t *testing.T) {
		_, err := storage.NewShaleRepo(storage.ShaleConfig{
			NodeID:      "cas-guard-4",
			DbName:      "cas-guard",
			Coordinator: "raft",
		})
		if err == nil {
			t.Fatal("want error for unknown Coordinator value, got nil")
		}
		if !strings.Contains(err.Error(), "raft") {
			t.Fatalf("error %q does not name the rejected value", err)
		}
	})
}
