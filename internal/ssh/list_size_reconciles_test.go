package ssh

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// A list row must report the size the QUOTA CHARGED, so "whoami says I am using
// X, what is using it" is answerable by adding up a list.
//
// A paste row already reports its compressed size. A site's charged figure is
// StoredBytes, recorded at deploy: the manifest cannot reproduce it, because
// per-entry compressed sizes are not persisted.
func TestListSiteSizeIsTheChargedSize(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "a", Size: 4000})
	man.Add("app.js", domain.ManifestEntry{SHA: "b", Size: 2000})

	site := domain.Site{
		Slug:        "sitezzz1",
		Manifest:    man,
		StoredBytes: 1500, // what the deploy charged
		ExpiresAt:   now.Add(time.Hour),
	}

	// The fixture must distinguish the two, or this proves nothing.
	if site.StoredBytes == man.DedupedSize() {
		t.Fatal("degenerate fixture: the charged size must differ from the manifest total")
	}

	got := newSiteListItem(site, now).SizeBytes
	if got != site.StoredBytes {
		t.Fatalf("list reports %d but the quota charged %d; a user adding up a list must arrive at "+
			"the number whoami shows", got, site.StoredBytes)
	}
	if got == man.DedupedSize() {
		t.Fatalf("list is reporting the UNCOMPRESSED manifest total (%d), which overstates the site's "+
			"cost and does not sum to the quota line", got)
	}
}
