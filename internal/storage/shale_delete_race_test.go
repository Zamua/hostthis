//go:build slatedb

package storage_test

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// TestShaleDelete_VsDeleteVersionSameSlug_NoUnderCount pins the closure of
// review finding (b): a whole-paste Delete and a DeleteVersion on the SAME
// slug, run concurrently, must never double-decrement the freed bytes. A
// double-decrement is an UNDER-count, which would let the owner exceed
// quota - the one direction that is unacceptable.
//
// The fix computes Delete's `freed` INSIDE its authoritative CAS by
// re-reading each version's tombstone state; a concurrent DeleteVersion
// that already tombstoned (and decremented) a version commits a change to
// that version key, conflicting Delete's CAS so the retry re-reads the
// now-tombstoned version and excludes it from `freed`.
//
// To make an under-count observable, each owner also holds a "keeper"
// paste that is never touched: its bytes are the floor the counter must
// never drop below. After the race the victim paste is gone (Delete
// removes it whichever way the race resolves), so the true live bytes for
// the owner equal exactly the keeper. The counter must therefore stay >=
// the keeper bytes; the double-decrement bug drops it below.
func TestShaleDelete_VsDeleteVersionSameSlug_NoUnderCount(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; start dev MinIO first")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Now().UTC()

	const (
		keeperBytes = 200 // a live paste the race never touches: the counter floor
		v1Bytes     = 100 // victim insert
		v2Bytes     = 60  // victim append
		userCap     = 1 << 20
		iters       = 40 // enough rounds to land in the race window on slate-on-MinIO
	)

	for i := range iters {
		owner := fmt.Sprintf("key:delrace-%d", i) // fresh owner: counter starts at 0
		keeper := domain.Slug(fmt.Sprintf("kp%06d", i))
		victim := domain.Slug(fmt.Sprintf("vc%06d", i))

		keep := pasteOf(keeper.String(), owner, keeperBytes)
		keep.ExpiresAt = now.Add(domain.RetentionWindow)
		if err := repo.InsertWithQuotaCheck(keep, userCap, now); err != nil {
			t.Fatalf("iter %d keeper insert: %v", i, err)
		}

		vic := pasteOf(victim.String(), owner, v1Bytes)
		vic.ExpiresAt = now.Add(domain.RetentionWindow)
		if err := repo.InsertWithQuotaCheck(vic, userCap, now); err != nil {
			t.Fatalf("iter %d victim insert: %v", i, err)
		}
		if _, err := repo.AppendVersionWithQuotaCheck(victim, domain.KindHTML, "sha-"+victim.String()+"-v2", v2Bytes, userCap, now); err != nil {
			t.Fatalf("iter %d victim append: %v", i, err)
		}
		// counter == keeperBytes + v1Bytes + v2Bytes for this owner.

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = repo.Delete(victim) }()
		go func() { defer wg.Done(); _ = repo.DeleteVersion(victim, 2) }()
		wg.Wait()

		// Victim gone -> true live bytes == keeperBytes. The counter must
		// never have under-counted below that floor. An over-count (a delete
		// whose {id} decrement lost a transient CAS) is the documented
		// fail-safe direction and is allowed; an under-count is the bug.
		got, err := repo.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("iter %d sum: %v", i, err)
		}
		if got < keeperBytes {
			t.Fatalf("iter %d UNDER-COUNT: counter=%d < keeper floor %d; same-slug Delete+DeleteVersion double-decremented",
				i, got, keeperBytes)
		}
	}
}
