package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// TestShaleDelete_VsDeleteVersionSameSlug_NoUnderCount pins that a whole-paste
// Delete racing a DeleteVersion on the SAME slug never under-counts the owner's
// active bytes. An under-count lets the owner exceed quota, the one
// unacceptable direction; an over-count only over-rejects.
//
// Each owner also holds an untouched "keeper" paste. The victim is gone
// whichever way the race resolves, so the owner's true live bytes equal exactly
// the keeper: the sum must never fall below that floor.
func TestShaleDelete_VsDeleteVersionSameSlug_NoUnderCount(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Now().UTC()

	const (
		keeperBytes = 200 // a live paste the race never touches: the floor
		v1Bytes     = 100 // victim insert
		v2Bytes     = 60  // victim append
		userCap     = 1 << 20
		iters       = 40 // enough rounds to land in the race window
	)

	for i := range iters {
		owner := fmt.Sprintf("key:delrace-%d", i) // fresh owner: counter starts at 0
		keeper := domain.Slug(fmt.Sprintf("kp%06d", i))
		victim := domain.Slug(fmt.Sprintf("vc%06d", i))

		keep := pasteOf(keeper.String(), owner, keeperBytes)
		if err := repo.InsertWithQuotaCheck(context.Background(), keep, userCap, now); err != nil {
			t.Fatalf("iter %d keeper insert: %v", i, err)
		}

		vic := pasteOf(victim.String(), owner, v1Bytes)
		if err := repo.InsertWithQuotaCheck(context.Background(), vic, userCap, now); err != nil {
			t.Fatalf("iter %d victim insert: %v", i, err)
		}
		if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), victim, domain.KindHTML, "sha-"+victim.String()+"-v2", v2Bytes, userCap, now); err != nil {
			t.Fatalf("iter %d victim append: %v", i, err)
		}
		// The owner's sum is now keeperBytes + v1Bytes + v2Bytes.

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = repo.Delete(victim, vic.Identity, vic.CreatedAt) }()
		go func() { defer wg.Done(); _ = repo.DeleteVersion(victim, 2) }()
		wg.Wait()

		// Victim gone -> true live bytes == keeperBytes. An over-count (a lost
		// index write) is the allowed fail-safe direction; under-counting below
		// the floor is not.
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
