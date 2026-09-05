// The site port served by the paste family.

package storage_test

import (
	"context"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestArtifactSites_InsertGetAndList(t *testing.T) {
	repo := storage.NewMemRepo()
	sites := storage.NewSites(repo)
	s := siteOf("sitea234", "key:owner-s", 49)
	if err := sites.InsertWithQuotaCheck(context.Background(), s, 49, 0, fixedNow); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := sites.Get(s.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Manifest.Files) != 1 || got.Identity != s.Identity {
		t.Fatalf("round trip = %+v", got)
	}

	// The site listing reports LEGACY rows only: a paste-backed directory
	// is already in the paste listing the caller concatenates this onto, so
	// returning it here too would show it twice.
	listed, err := sites.ListSitesByOwner(s.Identity.String(), fixedNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("list = %+v, want empty: the paste listing already carries it", listed)
	}
	arts, err := repo.ListByOwner(s.Identity.String())
	if err != nil {
		t.Fatalf("paste list: %v", err)
	}
	if len(arts) != 1 || arts[0].Slug != s.Slug || arts[0].Kind != domain.KindSite {
		t.Fatalf("paste listing = %+v, want the directory", arts)
	}
}

// A directory's bytes are already in the ARTIFACT sum the service adds this to,
// so reporting them again would bill every directory twice.
func TestArtifactSites_SumIsZeroToAvoidDoubleCounting(t *testing.T) {
	repo := storage.NewMemRepo()
	sites := storage.NewSites(repo)
	const owner = "key:owner-s"
	insertSite(t, sites, siteOf("sitec234", owner, 49))

	pasteSum, err := repo.SumActiveBytesByOwner(owner, fixedNow)
	if err != nil {
		t.Fatalf("paste sum: %v", err)
	}
	if pasteSum != 49 {
		t.Fatalf("paste sum = %d, want the directory's 49", pasteSum)
	}
	siteSum, err := sites.SumActiveBytesByOwner(owner, fixedNow)
	if err != nil {
		t.Fatalf("site sum: %v", err)
	}
	if siteSum != 0 {
		t.Fatalf("site sum = %d, want 0: the paste sum already counts it", siteSum)
	}
}
