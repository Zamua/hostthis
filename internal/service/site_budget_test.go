package service

import "testing"

// A redeploy appends a retained version, so the untar guard receives only the
// owner's unallocated headroom.
func TestSiteExtractBudget_DoesNotCreditRetainedVersion(t *testing.T) {
	if got := siteExtractBudget(1000, 200, 500); got != 300 {
		t.Fatalf("want 300 bytes of unallocated headroom, got %d", got)
	}
}

// An over-quota owner gets no headroom, never a negative budget that would read
// as unlimited downstream.
func TestSiteExtractBudget_FloorsAtZero(t *testing.T) {
	if got := siteExtractBudget(100, 500, 500); got != 0 {
		t.Fatalf("an over-quota owner must get 0 budget, got %d", got)
	}
}
