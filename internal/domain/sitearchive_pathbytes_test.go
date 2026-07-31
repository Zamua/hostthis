package domain

import (
	"io"
	"strconv"
	"strings"
	"testing"
)

// stubSink stores nothing: it drains the entry so the byte cap sees the read,
// and reports a per-path sha.
type stubSink struct{}

func (stubSink) Store(p string, r io.Reader, _ int64) (string, int, error) {
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return "", 0, err
	}
	return "sha-" + p, int(n), nil
}

func addFile(t *testing.T, e *SiteExtractor, name, body string) error {
	t.Helper()
	return e.Add(
		ArchiveEntry{Name: name, Size: int64(len(body)), Kind: EntryFile},
		strings.NewReader(body),
		stubSink{},
	)
}

// The extractor's carried path-text total equals what re-summing the manifest
// would produce, including when an archive repeats a path (one manifest key,
// charged once).
func TestSiteExtractor_PathBytesMatchesManifest(t *testing.T) {
	e := NewSiteExtractor(1 << 20)
	for _, name := range []string{"index.html", "a/b/c.css", "a/b/c.css", "img/logo.png"} {
		if err := addFile(t, e, name, "x"); err != nil {
			t.Fatalf("add %q: %v", name, err)
		}
	}
	if e.pathBytes != e.man.PathTextBytes() {
		t.Fatalf("carried path bytes %d != manifest path bytes %d", e.pathBytes, e.man.PathTextBytes())
	}
}

// The manifest-footprint cap still trips, on the same measurement.
func TestSiteExtractor_ManifestByteCapTrips(t *testing.T) {
	e := NewSiteExtractor(1 << 30)
	longDir := strings.Repeat("d", 200)
	var err error
	for i := range MaxSiteFiles {
		// Distinct 1000-byte-ish paths: MaxManifestBytes is reached long before
		// the file-count cap.
		name := longDir + "/" + strings.Repeat("n", 700) + "/" + strconv.Itoa(i) + ".html"
		if err = addFile(t, e, name, "x"); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatalf("manifest path-text cap never tripped")
	}
	if !strings.Contains(err.Error(), "manifest path text") {
		t.Fatalf("expected the manifest path-text cap, got %v", err)
	}
	if e.pathBytes <= MaxManifestBytes {
		t.Fatalf("cap tripped at %d bytes, which is not over %d", e.pathBytes, MaxManifestBytes)
	}
}
