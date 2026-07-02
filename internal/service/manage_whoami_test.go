package service

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// whoamiPasteStub satisfies PasteAdmin by embedding the interface (unused
// methods stay nil and would panic if called); Whoami only reads the three
// implemented here.
type whoamiPasteStub struct {
	PasteAdmin
	count int
	bytes int
	first time.Time
}

func (s whoamiPasteStub) CountByOwner(string) (int, error)                     { return s.count, nil }
func (s whoamiPasteStub) SumActiveBytesByOwner(string, time.Time) (int, error) { return s.bytes, nil }
func (s whoamiPasteStub) OwnerFirstSeen(string) (time.Time, error)             { return s.first, nil }

type siteSummerStub struct{ bytes int64 }

func (s siteSummerStub) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	return s.bytes, nil
}

// TestWhoami_UsedBytesIncludesSites pins the fix: used_bytes is the COMBINED
// paste + site total the quota cap enforces, not paste bytes alone.
func TestWhoami_UsedBytesIncludesSites(t *testing.T) {
	owner := "key:abc"
	paste := whoamiPasteStub{count: 3, bytes: 1000, first: time.Unix(1_700_000_000, 0)}

	// Without a site summer wired: paste bytes only.
	m := &Manage{Repo: paste, Now: time.Now}
	got, err := m.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami (no sites): %v", err)
	}
	if got.UsedBytes != 1000 {
		t.Fatalf("paste-only used_bytes: got %d, want 1000", got.UsedBytes)
	}

	// With a site summer wired: paste + site.
	m.SiteBytes = siteSummerStub{bytes: 4200}
	got, err = m.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami (with sites): %v", err)
	}
	if got.UsedBytes != 5200 {
		t.Fatalf("combined used_bytes: got %d, want 5200 (1000 paste + 4200 site)", got.UsedBytes)
	}
	if got.Active != 3 {
		t.Fatalf("active_pastes should count pastes only: got %d, want 3", got.Active)
	}
	if got.QuotaBytes != domain.UserQuotaBytes {
		t.Fatalf("quota_bytes: got %d, want %d", got.QuotaBytes, domain.UserQuotaBytes)
	}
}
