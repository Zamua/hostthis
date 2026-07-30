//go:build slatedb

package storage_test

// The identity-leading keygate index. Two things can go wrong with a derived
// index: it can disagree with the authoritative rows, and it can fail to be
// cleaned up.
//
//	go test -tags slatedb -run TestShaleKeygateIndex ./internal/storage

import (
	"os"
	"testing"
	"time"
)

const kgWindow = 24 * time.Hour

// The index must agree with the authoritative rows across multiple subnets and
// multiple identities.
func TestShaleKeygateIndex_CountsSubnetsForIdentity(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale keygate index test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	const me = "key:alice"
	const other = "key:bob"
	for _, subnet := range []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24"} {
		if _, err := repo.AdmitNewKey(me, subnet, now, 20, kgWindow); err != nil {
			t.Fatalf("admit %s on %s: %v", me, subnet, err)
		}
	}
	// A second identity on a shared subnet and one of its own. Its rows must
	// not leak into alice's count: the failure mode of a prefix that is not
	// actually scoped to the identity.
	for _, subnet := range []string{"10.0.0.0/24", "10.0.9.0/24"} {
		if _, err := repo.AdmitNewKey(other, subnet, now, 20, kgWindow); err != nil {
			t.Fatalf("admit %s on %s: %v", other, subnet, err)
		}
	}

	got, err := repo.SubnetsForIdentity(me, now, kgWindow)
	if err != nil {
		t.Fatalf("SubnetsForIdentity: %v", err)
	}
	if got != 3 {
		t.Fatalf("want 3 subnets for %s, got %d; the identity-leading index must agree with the "+
			"authoritative keygate rows, and a faster index that returns a different answer is "+
			"strictly worse than the scan it replaced", me, got)
	}
	if got, err := repo.SubnetsForIdentity(other, now, kgWindow); err != nil || got != 2 {
		t.Fatalf("want 2 subnets for %s, got %d (err %v); one identity's entries must not be "+
			"counted under another", other, got, err)
	}
	// Guards against a prefix bug that returns the whole table for an unknown
	// key.
	if got, err := repo.SubnetsForIdentity("key:nobody", now, kgWindow); err != nil || got != 0 {
		t.Fatalf("want 0 subnets for an unknown identity, got %d (err %v)", got, err)
	}
}

// Out-of-window entries must not count.
func TestShaleKeygateIndex_ExcludesOutOfWindow(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale keygate index test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	const id = "key:carol"

	if _, err := repo.AdmitNewKey(id, "10.1.0.0/24", old, 20, kgWindow); err != nil {
		t.Fatalf("admit old: %v", err)
	}
	if _, err := repo.AdmitNewKey(id, "10.1.1.0/24", now, 20, kgWindow); err != nil {
		t.Fatalf("admit fresh: %v", err)
	}

	got, err := repo.SubnetsForIdentity(id, now, kgWindow)
	if err != nil {
		t.Fatalf("SubnetsForIdentity: %v", err)
	}
	if got != 1 {
		t.Fatalf("want 1 in-window subnet (the 48h-old one must be excluded), got %d", got)
	}
}

// Pruning an authoritative row must drop its index entry, or the index
// outlives the fact it describes and the count over-reports without bound.
func TestShaleKeygateIndex_PruneDropsTheIndexEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale keygate index test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	const id = "key:dave"

	if _, err := repo.AdmitNewKey(id, "10.2.0.0/24", old, 20, kgWindow); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// Confirm the entry EXISTS first, against a window wide enough to include
	// it: otherwise the post-prune assertion passes vacuously when the entry
	// was never written at all.
	if got, err := repo.SubnetsForIdentity(id, now, 72*time.Hour); err != nil || got != 1 {
		t.Fatalf("setup: want the index entry present before pruning, got %d (err %v)", got, err)
	}

	deleted, err := repo.DeleteFirstSeenOlderThan(now.Add(-kgWindow))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("setup: want 1 authoritative row pruned, got %d", deleted)
	}

	if got, err := repo.SubnetsForIdentity(id, now, 72*time.Hour); err != nil || got != 0 {
		t.Fatalf("want 0 after the authoritative row was pruned, got %d (err %v); a derived index "+
			"that outlives its row makes whoami over-report forever", got, err)
	}
}

// Reconcile must backfill an entry for a row that has none (a key admitted
// before the index existed, or a best-effort co-write that failed at admit)
// and prune an entry whose row is gone.
func TestShaleKeygateIndex_ReconcileBackfillsAndPrunes(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale keygate index test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	const id = "key:erin"

	// A LEGACY row: an authoritative keygate row with NO identity entry.
	if err := repo.PutRawForTest(
		[]byte("keygate/10.3.0.0/24/"+id),
		[]byte(now.Format(time.RFC3339Nano)),
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if got, err := repo.SubnetsForIdentity(id, now, kgWindow); err != nil || got != 0 {
		t.Fatalf("setup: a legacy row must start UNINDEXED, got %d (err %v) - if it already "+
			"counted, this test would not be exercising backfill at all", got, err)
	}

	// An ORPHAN index entry: no authoritative row behind it.
	if err := repo.PutRawForTest(
		[]byte("keygate_id/"+id+"/10.9.9.0/24"),
		[]byte(now.Format(time.RFC3339Nano)),
	); err != nil {
		t.Fatalf("seed orphan index entry: %v", err)
	}

	if err := repo.ReconcileForTest(now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Assert on WHICH entries exist, not the count: the count cannot
	// distinguish success from two errors cancelling, since an un-backfilled
	// row contributes 0 and an un-pruned orphan contributes 1.
	backfilled := []byte("keygate_id/" + id + "/10.3.0.0/24")
	orphan := []byte("keygate_id/" + id + "/10.9.9.0/24")

	if raw, err := repo.GetRawForTest(backfilled); err != nil || raw == nil {
		t.Fatalf("reconcile must BACKFILL the identity entry for the legacy row (%s): got %q err %v; "+
			"without this, every key admitted before the index existed reports 0 subnets forever",
			backfilled, raw, err)
	}
	if raw, err := repo.GetRawForTest(orphan); err == nil && raw != nil {
		t.Fatalf("reconcile must PRUNE the orphan identity entry (%s) whose authoritative row is gone: "+
			"got %q; left alone it makes whoami over-report subnets the gate has already forgotten",
			orphan, raw)
	}

	// The resulting count is then right for the right reason.
	got, err := repo.SubnetsForIdentity(id, now, kgWindow)
	if err != nil {
		t.Fatalf("SubnetsForIdentity after reconcile: %v", err)
	}
	if got != 1 {
		t.Fatalf("want 1 after reconcile (legacy row backfilled, orphan entry pruned), got %d", got)
	}
}
