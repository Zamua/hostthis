package service

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// summaryRepoStub provides ONLY OwnerSummary; the embedded nil PasteAdmin
// makes any other repo call panic, so a pass proves whoami is one call.
type summaryRepoStub struct {
	PasteAdmin
	sum   domain.OwnerSummary
	calls int
}

func (s *summaryRepoStub) OwnerSummary(string, time.Time) (domain.OwnerSummary, error) {
	s.calls++
	return s.sum, nil
}

// Whoami is exactly one OwnerSummary call, and used_bytes is the summary's
// paste + site total (the same combined figure the quota cap enforces).
func TestWhoami_IsOneOwnerSummaryCall(t *testing.T) {
	first := time.Unix(1_700_000_000, 0)
	stub := &summaryRepoStub{sum: domain.OwnerSummary{
		Active: 4, FirstSeen: first, PasteBytes: 1000, SiteBytes: 200,
	}}
	m := &Manage{Repo: stub, Now: time.Now}

	got, err := m.Whoami("key:abc", "")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("OwnerSummary calls: got %d, want 1", stub.calls)
	}
	if got.Active != 4 {
		t.Fatalf("active: got %d, want 4", got.Active)
	}
	if !got.FirstSeen.Equal(first) {
		t.Fatalf("first seen: got %v, want %v", got.FirstSeen, first)
	}
	if got.UsedBytes != 1200 {
		t.Fatalf("used bytes: got %d, want 1200 (paste 1000 + site 200)", got.UsedBytes)
	}
	if got.QuotaBytes != domain.UserQuotaBytes {
		t.Fatalf("quota bytes: got %d, want %d", got.QuotaBytes, domain.UserQuotaBytes)
	}
}
