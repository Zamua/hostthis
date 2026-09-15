package domain

import (
	"strings"
	"testing"
)

// A paste's root is its manifest's "/" entry, else its index.html. Without
// either, the flat descriptor supplies no key, so nothing names bytes.
func TestPaste_RootEntry(t *testing.T) {
	for name, tc := range map[string]struct {
		paste Paste
		want  string
	}{
		"document":    {Paste{Manifest: DocumentManifest(ManifestEntry{Key: "uploads/d/0"})}, "uploads/d/0"},
		"directory":   {Paste{Manifest: Manifest{Files: map[string]ManifestEntry{"index.html": {Key: "uploads/s/0"}}}}, "uploads/s/0"},
		"no manifest": {Paste{Kind: KindHTML, Size: 3}, ""},
		"no root":     {Paste{Manifest: Manifest{Files: map[string]ManifestEntry{"app.js": {Key: "uploads/s/1"}}}}, ""},
	} {
		if got := tc.paste.RootEntry().Key; got != tc.want {
			t.Errorf("%s: root key = %q, want %q", name, got, tc.want)
		}
	}
}

func TestNewRandomSlug_ShapeAndAlphabet(t *testing.T) {
	for range 256 {
		s := NewRandomSlug()
		if len(s) != SlugLength {
			t.Fatalf("slug length: got %d, want %d (slug=%q)", len(s), SlugLength, s)
		}
		for _, r := range s {
			if !strings.ContainsRune(SlugAlphabet, r) {
				t.Fatalf("slug %q contains char %q outside SlugAlphabet", s, r)
			}
		}
	}
}
