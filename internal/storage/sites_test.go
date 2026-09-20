// The site port served by the paste family.

package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// A directory is stored under the kind that decides how it is SERVED, so a kind
// that is not a directory kind is refused at the door. Stored, it would read
// back as not-found through the site port: the deploy would report success and
// hand out a URL nothing answers.
func TestArtifactSites_RefusesANonDirectoryKind(t *testing.T) {
	repo := storage.NewMemRepo()
	sites := storage.NewSites(repo)

	for _, kind := range []domain.ContentKind{"", domain.KindHTML, domain.KindMarkdown} {
		s := siteOf("kinda234", "key:owner-k", 10)
		s.Kind = kind
		if err := sites.InsertWithQuotaCheck(context.Background(), s, 10, 0, fixedNow); err == nil {
			t.Fatalf("kind %q: insert must be refused", kind)
		}
		if _, err := repo.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("kind %q: a refused insert left a row behind (%v)", kind, err)
		}
	}

	// A redeploy carries the kind THAT deploy decided, so it is guarded the
	// same way and leaves the live version untouched when refused.
	live := siteOf("kindb234", "key:owner-k", 10)
	insertSite(t, sites, live)
	blank := siteOf("kindb234", "key:owner-k", 20)
	blank.Kind = ""
	if err := sites.ReplaceWithQuotaCheck(context.Background(), blank, 20, 0, fixedNow); err == nil {
		t.Fatal("a redeploy with no kind must be refused")
	}
	got, err := sites.Get("kindb234")
	if err != nil || got.Kind != domain.KindSite {
		t.Fatalf("after the refused redeploy the site reads back as (%q, %v), want site", got.Kind, err)
	}
}

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
