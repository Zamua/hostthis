package domain

import "testing"

// TestManifest_StripCommonLeadingDir pins the directory-archive fix: a
// `tar czf - site/` archive (everything nested under one top-level dir) is
// flattened so index.html serves at the root, while a root-level file or
// multiple top-level dirs are left untouched.
func TestManifest_StripCommonLeadingDir(t *testing.T) {
	t.Run("all under one dir -> stripped", func(t *testing.T) {
		m := NewManifest()
		m.Add("site/index.html", ManifestEntry{})
		m.Add("site/css/style.css", ManifestEntry{})
		m.StripCommonLeadingDir()
		if _, ok := m.Files["index.html"]; !ok {
			t.Fatalf("expected index.html at root, got %v", keys(m.Files))
		}
		if _, ok := m.Files["css/style.css"]; !ok {
			t.Fatalf("expected css/style.css, got %v", keys(m.Files))
		}
	})

	t.Run("a root-level file disqualifies stripping", func(t *testing.T) {
		m := NewManifest()
		m.Add("index.html", ManifestEntry{})
		m.Add("site/x", ManifestEntry{})
		m.StripCommonLeadingDir()
		if _, ok := m.Files["index.html"]; !ok {
			t.Fatalf("root file should be untouched, got %v", keys(m.Files))
		}
		if _, ok := m.Files["site/x"]; !ok {
			t.Fatalf("site/x should be untouched, got %v", keys(m.Files))
		}
	})

	t.Run("multiple top-level dirs -> no strip", func(t *testing.T) {
		m := NewManifest()
		m.Add("a/x", ManifestEntry{})
		m.Add("b/y", ManifestEntry{})
		m.StripCommonLeadingDir()
		if _, ok := m.Files["a/x"]; !ok {
			t.Fatalf("multi-dir should be untouched, got %v", keys(m.Files))
		}
	})
}

// TestIsJunkPath pins the OS-sidecar filter used by SafeUntar.
func TestIsJunkPath(t *testing.T) {
	junk := []string{"._index.html", "site/._x", ".DS_Store", "site/.DS_Store", "__MACOSX/foo", "__MACOSX"}
	for _, j := range junk {
		if !isJunkPath(j) {
			t.Errorf("%q should be junk", j)
		}
	}
	keep := []string{"index.html", "site/style.css", "_data.json", "a._b"}
	for _, k := range keep {
		if isJunkPath(k) {
			t.Errorf("%q should NOT be junk", k)
		}
	}
}
