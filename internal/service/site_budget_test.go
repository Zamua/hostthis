package service

import (
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// siteWith builds a site as a READ returns it: a manifest carrying only
// uncompressed sizes (per-entry compressed sizes are not persisted) plus the
// StoredBytes the deploy charged.
func siteWith(uncompressed, stored int) domain.Site {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "a", Size: uncompressed})
	return domain.Site{Manifest: man, StoredBytes: stored}
}

// The replace budget must credit the site's STORED size.
//
// A site is charged as CompressedSize, so usedSite sums compressed
// bytes. Crediting the uncompressed size subtracts more than was ever added,
// inflating the budget handed to the untar guard and letting a re-deploy expand
// past the owner's real remaining quota.
func TestSiteExtractBudget_CreditsTheCompressedSize(t *testing.T) {
	// The target: 400 uncompressed, stored as 100 compressed.
	existing := siteWith(400, 100)

	// The owner ALSO holds 600 compressed bytes of other sites. That matters:
	// with the target as the only site, usedSite-credit goes negative under
	// either unit and the zero floor hides the difference. Other sites are what
	// make the over-credit observable.
	const usedSite = 700 // 600 others + this target's stored 100

	got := siteExtractBudget(1000, 0, usedSite, existing)

	// Correct: credit StoredBytes (100) -> used 600 -> budget 400.
	if got != 400 {
		t.Fatalf("want 400 (cap 1000 minus the 600 of OTHER sites), got %d. 700 means the credit used "+
			"the uncompressed manifest total (400), inflating the budget past the real remaining "+
			"quota; 300 means it credited 0, which blocks an in-place update at the cap.", got)
	}
}

// The three CANDIDATE credits must be pairwise distinct, or the test above
// passes whichever one the code picked.
//
// siteExtractBudget reads StoredBytes; the two wrong answers are the manifest's
// uncompressed DedupedSize and its (unpersisted, therefore 0)
// CompressedSize. Comparing only the two manifest figures leaves the one
// value the function actually reads unpinned, so a fixture whose StoredBytes
// happened to equal DedupedSize would satisfy both tests while proving nothing.
func TestSiteExtractBudget_CandidateCreditsAreDistinguishable(t *testing.T) {
	existing := siteWith(400, 100)
	stored := existing.StoredBytes
	deduped := existing.Manifest.Size()
	compressed := existing.Manifest.CompressedSize()
	if stored == deduped || stored == compressed || deduped == compressed {
		t.Fatalf("fixture is degenerate: stored=%d deduped=%d compressed=%d must be pairwise distinct, "+
			"or the budget assertion cannot tell which credit was used", stored, deduped, compressed)
	}
}

// An over-quota owner gets no headroom, never a negative budget that would read
// as unlimited downstream.
func TestSiteExtractBudget_FloorsAtZero(t *testing.T) {
	if got := siteExtractBudget(100, 500, 500, siteWith(0, 0)); got != 0 {
		t.Fatalf("an over-quota owner must get 0 budget, got %d", got)
	}
}
