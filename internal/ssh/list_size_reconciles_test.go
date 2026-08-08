package ssh

import (
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// A list row must report the size the QUOTA CHARGED, so "whoami says I am using
// X, what is using it" is answerable by adding up a list.
//
// A paste row already reports its compressed size. A site's charged figure is
// StoredBytes, recorded at deploy: the manifest cannot reproduce it, because
// per-entry compressed sizes are not persisted.
func TestListSiteSizeIsTheChargedSize(t *testing.T) {

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "a", Size: 4000})
	man.Add("app.js", domain.ManifestEntry{SHA: "b", Size: 2000})

	site := domain.Site{
		Slug:        "sitezzz1",
		Manifest:    man,
		StoredBytes: 1500, // what the deploy charged
	}

	// The fixture must distinguish the two, or this proves nothing.
	if site.StoredBytes == man.Size() {
		t.Fatal("degenerate fixture: the charged size must differ from the manifest total")
	}

	got := newSiteListItem(site).SizeBytes
	if got != site.StoredBytes {
		t.Fatalf("list reports %d but the quota charged %d; a user adding up a list must arrive at "+
			"the number whoami shows", got, site.StoredBytes)
	}
	if got == man.Size() {
		t.Fatalf("list is reporting the UNCOMPRESSED manifest total (%d), which overstates the site's "+
			"cost and does not sum to the quota line", got)
	}
}

// A paste row must report every live version, not just the served one, since
// that is what the quota charges.
func TestListPasteSizeCountsAllLiveVersions(t *testing.T) {
	p := domain.Paste{
		Slug:          "pastez01",
		Size:          14266, // the served version
		StoredBytes:   40890, // every live version, what quota charges
		LatestVersion: 3,
	}

	item := newPasteListItem(p)
	if item.SizeBytes != p.StoredBytes {
		t.Fatalf("list reports %d but the quota charges %d; a multi-version paste understates its "+
			"cost when only the served version is shown", item.SizeBytes, p.StoredBytes)
	}
	// And the row must be flagged, so the table can explain a size larger than
	// the version being served.
	if !item.multiVersion {
		t.Fatal("a paste charged for more than it serves must be flagged, or the STORED column is " +
			"unexplained: VERS shows the served version number, not how many are stored")
	}
}

// A single-version paste is unaffected and must NOT trigger the note.
func TestListSingleVersionPasteIsUnflagged(t *testing.T) {
	p := domain.Paste{Slug: "pastez02", Size: 500, StoredBytes: 500, LatestVersion: 1}

	item := newPasteListItem(p)
	if item.SizeBytes != 500 {
		t.Fatalf("single-version size: want 500, got %d", item.SizeBytes)
	}
	if item.multiVersion {
		t.Fatal("a single-version paste must not be flagged, or every list prints the note")
	}
}

// A backend whose list read does not carry the total falls back to the served
// size rather than reporting zero.
func TestListFallsBackWhenStoredBytesAbsent(t *testing.T) {
	p := domain.Paste{Slug: "pastez03", Size: 777, StoredBytes: 0, LatestVersion: 1}

	if got := newPasteListItem(p).SizeBytes; got != 777 {
		t.Fatalf("want the served size 777 as a fallback, got %d; reporting 0 would make the paste "+
			"look free", got)
	}
}

// JSON must carry BOTH figures. The human table explains a multi-version size
// with a footer, but the JSON branch emits only the array, so a consumer has no
// prose to read and cannot infer which number it holds: served_version alone
// does not say, because a deleted version leaves a paste charged for fewer
// versions than its number implies.
func TestListJSONCarriesBothSizes(t *testing.T) {
	p := domain.Paste{Slug: "pastez04", Size: 14266, StoredBytes: 40890, LatestVersion: 3}

	item := newPasteListItem(p)
	if item.SizeBytes != 40890 {
		t.Fatalf("size_bytes must be the charged total: want 40890, got %d", item.SizeBytes)
	}
	if item.ServedSizeBytes == nil || *item.ServedSizeBytes != 14266 {
		t.Fatalf("served_size_bytes must be the served version's size (14266), got %v", item.ServedSizeBytes)
	}
	if item.SizeBytes == *item.ServedSizeBytes {
		t.Fatal("the two fields must be distinguishable on a multi-version paste, or the fixture " +
			"proves nothing")
	}
}

// A site has no versions, so served_size_bytes is null rather than a duplicate,
// matching how served_version is null for a site.
func TestListJSONSiteHasNoServedSize(t *testing.T) {
	site := domain.Site{Slug: "sitezzz2", Manifest: domain.NewManifest(), StoredBytes: 500}

	item := newSiteListItem(site)
	if item.ServedSizeBytes != nil {
		t.Fatalf("a site must report served_size_bytes as null, got %v", *item.ServedSizeBytes)
	}
	if item.SizeBytes != 500 {
		t.Fatalf("a site's size_bytes is its charged size: want 500, got %d", item.SizeBytes)
	}
}
