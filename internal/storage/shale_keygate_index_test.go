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
// An admission that walks past an out-of-window authoritative row drops it AND
// its identity-leading view. Without the second delete the derived index
// outlives its row and whoami over-reports forever, with nothing left to
// repair it.
func TestShaleKeygateIndex_AdmissionDropsExpiredRowAndItsEntry(t *testing.T) {
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
	// it: otherwise the assertion below passes vacuously when the entry was
	// never written at all.
	if got, err := repo.SubnetsForIdentity(id, now, 72*time.Hour); err != nil || got != 1 {
		t.Fatalf("setup: want the index entry present before the prune, got %d (err %v)", got, err)
	}

	// Any admission on that subnet scans it, and the scan is what prunes.
	if _, err := repo.AdmitNewKey("key:someone-else", "10.2.0.0/24", now, 20, kgWindow); err != nil {
		t.Fatalf("triggering admit: %v", err)
	}

	if got, err := repo.SubnetsForIdentity(id, now, 72*time.Hour); err != nil || got != 0 {
		t.Fatalf("want 0 after the admission scan pruned the aged-out row, got %d (err %v); a derived "+
			"index that outlives its row makes whoami over-report forever", got, err)
	}
}

// The identity-side read prunes its own expired entries, so an entry whose
// subnet never sees another admission still stops occupying space.
func TestShaleKeygateIndex_IdentityReadDropsExpiredEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale keygate index test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	const id = "key:frank"

	entry := []byte("keygate_id/" + id + "/10.4.0.0/24")
	if err := repo.PutRawForTest(entry, []byte(old.Format(time.RFC3339Nano))); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	if raw, err := repo.GetRawForTest(entry); err != nil || raw == nil {
		t.Fatalf("setup: the seeded entry must exist, got %q err %v", raw, err)
	}

	// The read filters it out AND removes it.
	if got, err := repo.SubnetsForIdentity(id, now, kgWindow); err != nil || got != 0 {
		t.Fatalf("an out-of-window entry must not count: got %d (err %v)", got, err)
	}
	if raw, err := repo.GetRawForTest(entry); err != nil || raw != nil {
		t.Fatalf("the read must also DROP the out-of-window entry; still present: %q (err %v)", raw, err)
	}
}
