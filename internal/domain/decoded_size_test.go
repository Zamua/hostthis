package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// The extractor records the bytes it read out of each entry as the decoded
// size, whatever the archive header declared.
func TestSiteExtractor_RecordsDecodedSize(t *testing.T) {
	e := NewSiteExtractor(1 << 20)
	if err := e.Add(ArchiveEntry{Name: "index.html", Size: 99, Kind: EntryFile}, strings.NewReader("hello"), stubSink{}); err != nil {
		t.Fatal(err)
	}
	got := e.Finish().Files["index.html"]
	if got.DecodedSize != 5 || got.Size != 5 {
		t.Fatalf("DecodedSize %d, Size %d, want 5 and 5", got.DecodedSize, got.Size)
	}
}

// Only a recorded decoded size is a known length; Size alone never is.
func TestManifestEntry_DecodedLength(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry ManifestEntry
		n     int
		known bool
	}{
		{"recorded", ManifestEntry{Size: 5, DecodedSize: 5}, 5, true},
		{"size alone", ManifestEntry{Size: 21}, 0, false},
		{"zero", ManifestEntry{}, 0, false},
		{"negative", ManifestEntry{DecodedSize: -1}, 0, false},
	} {
		if n, known := c.entry.DecodedLength(); n != c.n || known != c.known {
			t.Errorf("%s: DecodedLength() = %d, %v, want %d, %v", c.name, n, known, c.n, c.known)
		}
	}
}

// An entry without a decoded size stores no field for it, so it reads back
// the same as an entry written before the field existed.
func TestManifestEntry_DecodedSizeOmittedWhenZero(t *testing.T) {
	raw, err := json.Marshal(ManifestEntry{Key: "k", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["DecodedSize"]; ok {
		t.Fatalf("zero DecodedSize serialized: %s", raw)
	}
	raw, err = json.Marshal(ManifestEntry{Key: "k", DecodedSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	var back ManifestEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.DecodedSize != 3 {
		t.Fatalf("DecodedSize round-tripped as %d", back.DecodedSize)
	}
}
