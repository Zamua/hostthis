package domain

import (
	"net/http"
	"testing"
)

func mustManifest(files map[string]string) Manifest {
	m := NewManifest()
	for p, body := range files {
		m.Add(p, ManifestEntry{Key: "key-" + p, Size: len(body), ContentType: ContentTypeForPath(p)})
	}
	return m
}

func TestManifest_Lookup_DirectoryIndex(t *testing.T) {
	m := mustManifest(map[string]string{
		"index.html":      "root",
		"blog/index.html": "blog",
		"css/style.css":   "css",
	})
	cases := []struct {
		req    string
		want   string // expected key, "" means miss
		wantOK bool
	}{
		{"/", "key-index.html", true},
		{"", "key-index.html", true},
		{"/index.html", "key-index.html", true},
		{"/css/style.css", "key-css/style.css", true},
		{"/blog/", "key-blog/index.html", true},
		{"/blog", "key-blog/index.html", true}, // bare dir name resolves to its index
		{"/blog/index.html", "key-blog/index.html", true},
		{"/missing.html", "", false},
		{"/blog/missing.css", "", false},
		{"/nope/", "", false}, // a dir with no index.html is a miss
	}
	for _, c := range cases {
		t.Run(c.req, func(t *testing.T) {
			e, ok := m.Lookup(c.req)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, c.wantOK)
			}
			if ok && e.Key != c.want {
				t.Fatalf("key: got %q, want %q", e.Key, c.want)
			}
		})
	}
}

func TestManifest_LookupWithSPAFallback(t *testing.T) {
	m := mustManifest(map[string]string{
		"index.html":      "root",
		"blog/index.html": "blog",
		"css/style.css":   "css",
		"assets/app.js":   "js",
	})
	cases := []struct {
		name        string
		req         string
		wantKey     string // "" means a miss (404)
		wantHit     bool
		wantViaFall bool
	}{
		// Direct hits resolve exactly like Lookup, never via fallback.
		{"root index", "/", "key-index.html", true, false},
		{"explicit index", "/index.html", "key-index.html", true, false},
		{"real css asset", "/css/style.css", "key-css/style.css", true, false},
		{"nested dir index", "/blog/", "key-blog/index.html", true, false},
		{"bare dir name", "/blog", "key-blog/index.html", true, false},

		// Route-shaped misses fall back to the ROOT index.html (200).
		{"no-extension route", "/about", "key-index.html", true, true},
		{"deep no-ext route", "/users/123", "key-index.html", true, true},
		{"deeper no-ext route", "/users/123/edit", "key-index.html", true, true},
		{"html-extension route", "/about.html", "key-index.html", true, true},
		{"unknown-extension route", "/weird.zzz", "key-index.html", true, true},

		// Asset-shaped misses stay a clean miss (404), no fallback.
		{"missing js asset", "/assets/nope.js", "", false, false},
		{"missing css asset", "/css/missing.css", "", false, false},
		{"missing png asset", "/img/logo.png", "", false, false},
		{"missing woff2 asset", "/fonts/x.woff2", "", false, false},
		{"missing json asset", "/data/x.json", "", false, false},
		{"missing map asset", "/assets/app.js.map", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, hit, via := m.LookupWithSPAFallback(c.req)
			if hit != c.wantHit {
				t.Fatalf("hit: got %v, want %v", hit, c.wantHit)
			}
			if via != c.wantViaFall {
				t.Fatalf("viaFallback: got %v, want %v", via, c.wantViaFall)
			}
			if hit && e.Key != c.wantKey {
				t.Fatalf("key: got %q, want %q", e.Key, c.wantKey)
			}
		})
	}
}

// Without a root index.html a route-shaped miss has nothing to fall back to, so
// it 404s rather than serving some other index.
func TestManifest_LookupWithSPAFallback_NoRootIndex(t *testing.T) {
	m := mustManifest(map[string]string{
		"blog/index.html": "blog", // a nested index only
	})
	if _, hit, _ := m.LookupWithSPAFallback("/about"); hit {
		t.Fatalf("route miss with no root index.html should 404, got a hit")
	}
	if _, hit, via := m.LookupWithSPAFallback("/blog/"); !hit || via {
		t.Fatalf("nested index direct hit: hit=%v via=%v, want true,false", hit, via)
	}
}

// An extracted archive's shape follows the root lookup alone: a root index.html
// is a site, anything else is a knowledge base. Nothing else about the files
// participates, so an archive holding no web content is still a directory.
func TestManifest_ArchiveKind(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  ContentKind
	}{
		{"root index", map[string]string{"index.html": "x", "app.js": "y"}, KindSite},
		{"nested index only", map[string]string{"a/index.html": "x", "b/notes.md": "y"}, KindKnowledgeBase},
		{"markdown only", map[string]string{"README.md": "x", "guide/setup.md": "y"}, KindKnowledgeBase},
		{"neither markdown nor html", map[string]string{"logo.png": "x", "data.json": "y"}, KindKnowledgeBase},
		{"css and js without an index", map[string]string{"style.css": "x", "app.js": "y"}, KindKnowledgeBase},
		{"empty", map[string]string{}, KindKnowledgeBase},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustManifest(c.files).ArchiveKind(); got != c.want {
				t.Fatalf("ArchiveKind: got %q, want %q", got, c.want)
			}
		})
	}
}

// Both archive kinds are directories and no document kind is: serving reads the
// shape off the kind, so a document admitted here would be served as a file
// tree and a directory as one rendered document.
func TestContentKind_IsDirectory(t *testing.T) {
	for _, k := range []ContentKind{KindSite, KindKnowledgeBase} {
		if !k.IsDirectory() {
			t.Errorf("%q: IsDirectory false, want true", k)
		}
	}
	for _, k := range []ContentKind{KindHTML, KindMarkdown, KindDiff, KindPDF, KindCSV, KindJSON, KindText, KindLog, ""} {
		if k.IsDirectory() {
			t.Errorf("%q: IsDirectory true, want false", k)
		}
	}
}

// Markdown is recognised by extension, the rule deciding what a knowledge base
// renders rather than links to.
func TestIsMarkdownPath(t *testing.T) {
	for _, p := range []string{"README.md", "guide/setup.MD", "a/b.markdown"} {
		if !IsMarkdownPath(p) {
			t.Errorf("%q: not markdown, want markdown", p)
		}
	}
	for _, p := range []string{"index.html", "app.js", "notes.txt", "md", "/", ""} {
		if IsMarkdownPath(p) {
			t.Errorf("%q: markdown, want not markdown", p)
		}
	}
}

// Size and CompressedSize count PER PATH: two paths holding the same content
// are two stored objects, so both are counted.
func TestManifest_SizesCountEveryPath(t *testing.T) {
	m := NewManifest()
	m.Add("a.html", ManifestEntry{Key: "x", Size: 100, CompressedSize: 40})
	m.Add("b.html", ManifestEntry{Key: "x", Size: 100, CompressedSize: 40}) // same bytes, still stored
	m.Add("c.css", ManifestEntry{Key: "y", Size: 50, CompressedSize: 20})
	if got := m.Size(); got != 250 {
		t.Fatalf("Size: got %d, want 250 (both x paths counted)", got)
	}
	if got := m.CompressedSize(); got != 100 {
		t.Fatalf("CompressedSize: got %d, want 100 (both x paths counted)", got)
	}
}

func TestDetectKind_Archive(t *testing.T) {
	gzipBytes := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	if k, err := DetectKind(gzipBytes, "", http.DetectContentType); err != nil || k != KindSite {
		t.Fatalf("gzip no hint: got (%q, %v), want (site, nil)", k, err)
	}
	// A text hint must NOT smuggle a gzip stream through as HTML.
	if _, err := DetectKind(gzipBytes, "html", http.DetectContentType); err == nil {
		t.Fatalf("gzip with html hint should reject")
	}
	if k, err := DetectKind([]byte("<!doctype html><h1>x</h1>"), "", http.DetectContentType); err != nil || k != KindHTML {
		t.Fatalf("html still detected: got (%q, %v)", k, err)
	}
}
