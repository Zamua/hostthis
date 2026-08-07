// A version carries its whole content as a manifest, and a row written before
// that resolves through its flat fields instead.

package storage

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestNewVersionRow_CarriesAOneEntryManifest(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	row := newVersionRow(3, contentRef{
		Kind: string(domain.KindMarkdown), ContentSHA: "sha-abc", Size: 42,
	}, now)

	got := row.toDomain(domain.Slug("slug1234"))
	if !got.IsSingle() {
		t.Fatalf("a document must be a one-entry manifest, got %d entries", len(got.Manifest.Files))
	}
	e, ok := got.Manifest.Files[domain.Root]
	if !ok {
		t.Fatalf("manifest has no root entry: %+v", got.Manifest.Files)
	}
	if e.SHA != "sha-abc" || e.Size != 42 || e.Kind != string(domain.KindMarkdown) {
		t.Fatalf("root entry = %+v, want sha-abc/42/markdown", e)
	}
	// The accessors must read the MANIFEST, not the flat copy, or a multi-entry
	// version would silently report its root as whatever the flat fields said.
	if got.RootKind() != domain.KindMarkdown || got.RootSHA() != "sha-abc" || got.RootSize() != 42 {
		t.Fatalf("root accessors = %s/%s/%d", got.RootKind(), got.RootSHA(), got.RootSize())
	}
}

// A row stored before versions carried a manifest: the flat fields are the only
// description of its content, so the accessors must still answer from them.
func TestVersionRow_WithoutManifestFallsBackToFlatFields(t *testing.T) {
	row := versionRow{
		VerNum:     1,
		contentRef: contentRef{Kind: string(domain.KindHTML), ContentSHA: "sha-old", Size: 7},
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	got := row.toDomain(domain.Slug("slug5678"))
	if len(got.Manifest.Files) != 0 {
		t.Fatalf("a pre-manifest row must decode with no manifest, got %+v", got.Manifest.Files)
	}
	if got.RootKind() != domain.KindHTML || got.RootSHA() != "sha-old" || got.RootSize() != 7 {
		t.Fatalf("fallback accessors = %s/%s/%d, want html/sha-old/7", got.RootKind(), got.RootSHA(), got.RootSize())
	}
}

// A multi-entry manifest survives the encode/decode the row does, including the
// per-entry render kind and compressed size the quota and renderer need.
func TestVersionRow_MultiEntryManifestRoundTrips(t *testing.T) {
	m := domain.NewManifest()
	m.Add(domain.Root, domain.ManifestEntry{
		SHA: "sha-index", Size: 100, CompressedSize: 40,
		ContentType: "text/html", Kind: string(domain.KindHTML)})
	m.Add("/app.css", domain.ManifestEntry{
		SHA: "sha-css", Size: 20, CompressedSize: 9, ContentType: "text/css"})

	enc, err := encodeManifest(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	row := versionRow{VerNum: 1,
		contentRef: contentRef{Kind: string(domain.KindHTML), ContentSHA: "sha-index", Size: 100, Manifest: enc}}

	got := row.toDomain(domain.Slug("site1234"))
	if got.IsSingle() {
		t.Fatal("a two-entry manifest must not report as a single document")
	}
	css, ok := got.Manifest.Files["/app.css"]
	if !ok {
		t.Fatalf("lost the non-root entry: %+v", got.Manifest.Files)
	}
	if css.SHA != "sha-css" || css.Size != 20 || css.CompressedSize != 9 || css.ContentType != "text/css" {
		t.Fatalf("css entry = %+v", css)
	}
	if root := got.Manifest.Files[domain.Root]; root.Kind != string(domain.KindHTML) || root.CompressedSize != 40 {
		t.Fatalf("root entry lost kind or compressed size: %+v", root)
	}
}
