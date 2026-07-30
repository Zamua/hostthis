//go:build slatedb

package storage_test

import "testing"

// shaleRoomFenceReplicationBar is the replication factor the ScanRoom seq fence
// is sound at (see the REPLICATION BAR comment on ShaleRepo.ScanRoom): R=1 is
// single-owner and R=2 acks a write only once EVERY replica holds it, so any
// member a fence-bracketed read lands on is complete. R >= 3 acks on a quorum,
// and a read-one union scan served mid-handoff by a member outside some write's
// ack set could miss a mutation both fence reads agree on: an undetectable hole
// with S stamped high.
const shaleRoomFenceReplicationBar = 2

// TestShaleRoomSeqFence_ReplicationBar pins that the fence conformance evidence
// (the Rooms/Seq* subtests of TestConformance_Shale) is collected at or below
// that bar, so fixtures moving past R=2 force the fence rework (quorum union
// scan, or scan + fence reads bracketed on one member set) instead of silently
// shipping an unsound S. Config-level and deliberately cheap: no cluster is
// opened and no MinIO is needed.
func TestShaleRoomSeqFence_ReplicationBar(t *testing.T) {
	cfg := uniqueShaleConfig("http://unused.invalid:9000") // config only; nothing is opened
	if cfg.ReplicationFactor > shaleRoomFenceReplicationBar {
		t.Fatalf("shale test fixtures run ReplicationFactor=%d, past the R<=%d bar the ScanRoom seq fence is sound at; revisit the fence (see ShaleRepo.ScanRoom) before raising R",
			cfg.ReplicationFactor, shaleRoomFenceReplicationBar)
	}
	t.Logf("ScanRoom seq fence bar: sound at R<=%d (fixtures run R=%d); R>=3 needs a fence rework - see ShaleRepo.ScanRoom",
		shaleRoomFenceReplicationBar, cfg.ReplicationFactor)
}
