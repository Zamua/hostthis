package service

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// siteWith builds a site as a READ returns it: a manifest carrying only
// uncompressed sizes (per-entry compressed sizes are not persisted) plus the
// StoredBytes the deploy charged.
func siteWith(uncompressed, stored int, expiresAt time.Time) domain.Site {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "a", Size: uncompressed})
	return domain.Site{Manifest: man, StoredBytes: stored, ExpiresAt: expiresAt}
}

// The replace budget must credit the site's STORED size.
//
// A site is charged as CompressedDedupedSize, so usedSite sums compressed
// bytes. Crediting the uncompressed size subtracts more than was ever added,
// inflating the budget handed to the untar guard and letting a re-deploy expand
// past the owner's real remaining quota.
func TestSiteExtractBudget_CreditsTheCompressedSize(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// The target: 400 uncompressed, stored as 100 compressed, still live.
	existing := siteWith(400, 100, now.Add(time.Hour))

	// The owner ALSO holds 600 compressed bytes of other sites. That matters:
	// with the target as the only site, usedSite-credit goes negative under
	// either unit and the zero floor hides the difference. Other sites are what
	// make the over-credit observable.
	const usedSite = 700 // 600 others + this target's stored 100

	got := siteExtractBudget(1000, 0, usedSite, existing, now)

	// Correct: credit StoredBytes (100) -> used 600 -> budget 400.
	if got != 400 {
		t.Fatalf("want 400 (cap 1000 minus the 600 of OTHER sites), got %d. 700 means the credit used "+
			"the uncompressed manifest total (400), inflating the budget past the real remaining "+
			"quota; 300 means it credited 0, which blocks an in-place update at the cap.", got)
	}
}

// The distinction has to be observable, or the test above passes on a manifest
// whose two sizes happen to match.
func TestSiteExtractBudget_UnitsActuallyDiffer(t *testing.T) {
	existing := siteWith(400, 100, time.Now().Add(time.Hour))
	if existing.Manifest.DedupedSize() == existing.Manifest.CompressedDedupedSize() {
		t.Fatal("fixture is degenerate: the two sizes must differ or the unit check proves nothing")
	}
}

// An expired-but-unswept target is already excluded from usedSite, so crediting
// it would subtract bytes that were never counted.
func TestSiteExtractBudget_NoCreditForAnExpiredSite(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	expired := siteWith(400, 100, now.Add(-time.Hour))
	if got := siteExtractBudget(1000, 0, 600, expired, now); got != 400 {
		t.Fatalf("an expired target must not be credited: want 400, got %d", got)
	}
}

// An over-quota owner gets no headroom, never a negative budget that would read
// as unlimited downstream.
func TestSiteExtractBudget_FloorsAtZero(t *testing.T) {
	now := time.Now()
	if got := siteExtractBudget(100, 500, 500, siteWith(0, 0, now.Add(-time.Hour)), now); got != 0 {
		t.Fatalf("an over-quota owner must get 0 budget, got %d", got)
	}
}
