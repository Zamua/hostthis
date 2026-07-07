//go:build slatedb

package storage_test

import "testing"

// shaleRoomFenceReplicationBar is the replication factor the ScanRoom seq
// fence is known airtight at (see the REPLICATION BAR comment on
// ShaleRepo.ScanRoom): R=1 is single-owner, R=2 acks a write only once
// EVERY replica holds it (write bar 2/2), so any member a fence-bracketed
// read lands on is complete. R >= 3 acks on a quorum, and a read-one union
// scan served mid-handoff by a member outside some write's ack set could
// miss a mutation both fence reads agree on - an undetectable hole (S
// stamped high). The fence must be revisited before crossing this bar.
const shaleRoomFenceReplicationBar = 2

// TestShaleRoomSeqFence_ReplicationBar documents the ScanRoom seq fence's
// replication bar and pins that the fence conformance evidence (the
// Rooms/Seq* subtests of TestConformance_Shale, which run over
// uniqueShaleConfig-shaped single-node clusters) is collected AT OR BELOW
// it. It is a config-level assertion, deliberately cheap: no cluster is
// opened and no MinIO is needed, so it always runs under -tags slatedb.
// If the test fixtures ever move past R=2, this fails and forces the
// fence rework (quorum union scan, or scan + fence reads bracketed on one
// member set) instead of silently shipping an unsound S.
func TestShaleRoomSeqFence_ReplicationBar(t *testing.T) {
	cfg := uniqueShaleConfig("http://unused.invalid:9000") // config only; nothing is opened
	if cfg.ReplicationFactor > shaleRoomFenceReplicationBar {
		t.Fatalf("shale test fixtures run ReplicationFactor=%d, past the R<=%d bar the ScanRoom seq fence is sound at; revisit the fence (see ShaleRepo.ScanRoom) before raising R",
			cfg.ReplicationFactor, shaleRoomFenceReplicationBar)
	}
	t.Logf("ScanRoom seq fence bar: sound at R<=%d (fixtures run R=%d); R>=3 needs a fence rework - see ShaleRepo.ScanRoom",
		shaleRoomFenceReplicationBar, cfg.ReplicationFactor)
}
