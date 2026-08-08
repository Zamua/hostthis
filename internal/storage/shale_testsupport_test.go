// Shared shale test support, engine-independent.
//
// newShaleRepoForTest is supplied per build (shale_testsupport_slate_test.go /
// shale_testsupport_pebble_test.go), so every test below reads the same on
// either engine and none of them names one.

package storage_test

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func mustSum(t *testing.T, repo *storage.ShaleRepo, owner string, now time.Time) int {
	t.Helper()
	n, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum active bytes: %v", err)
	}
	return n
}

func mustCount(t *testing.T, repo *storage.ShaleRepo, owner string) int {
	t.Helper()
	// CountByOwner reads the identity_pastes index, which InsertWithQuotaCheck
	// writes from a deferred confirm goroutine; draining makes the count
	// deterministic. A no-op when nothing is pending.
	repo.WaitPendingConfirms()
	n, err := repo.CountByOwner(owner)
	if err != nil {
		t.Fatalf("count by owner: %v", err)
	}
	return n
}
