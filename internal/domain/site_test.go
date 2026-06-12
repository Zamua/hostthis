package domain

import "testing"

func mustManifest(files map[string]string) Manifest {
	m := NewManifest()
	for p, body := range files {
		m.Add(p, ManifestEntry{SHA: "sha-" + p, Size: len(body), ContentType: ContentTypeForPath(p)})
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
		want   string // expected SHA, "" means miss
		wantOK bool
	}{
		{"/", "sha-index.html", true},
		{"", "sha-index.html", true},
		{"/index.html", "sha-index.html", true},
		{"/css/style.css", "sha-css/style.css", true},
		{"/blog/", "sha-blog/index.html", true},
		{"/blog", "sha-blog/index.html", true}, // bare dir name → its index
		{"/blog/index.html", "sha-blog/index.html", true},
		{"/missing.html", "", false},
		{"/blog/missing.css", "", false},
		{"/nope/", "", false}, // dir with no index.html
	}
	for _, c := range cases {
		t.Run(c.req, func(t *testing.T) {
			e, ok := m.Lookup(c.req)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, c.wantOK)
			}
			if ok && e.SHA != c.want {
				t.Fatalf("sha: got %q, want %q", e.SHA, c.want)
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
		wantSHA     string // "" means a miss (404)
		wantHit     bool
		wantViaFall bool
	}{
		// Direct hits resolve exactly like Lookup, never via fallback.
		{"root index", "/", "sha-index.html", true, false},
		{"explicit index", "/index.html", "sha-index.html", true, false},
		{"real css asset", "/css/style.css", "sha-css/style.css", true, false},
		{"nested dir index", "/blog/", "sha-blog/index.html", true, false},
		{"bare dir name", "/blog", "sha-blog/index.html", true, false},

		// Route-shaped misses fall back to the ROOT index.html (200).
		{"no-extension route", "/about", "sha-index.html", true, true},
		{"deep no-ext route", "/users/123", "sha-index.html", true, true},
		{"deeper no-ext route", "/users/123/edit", "sha-index.html", true, true},
		{"html-extension route", "/about.html", "sha-index.html", true, true},
		{"unknown-extension route", "/weird.zzz", "sha-index.html", true, true},

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
			if hit && e.SHA != c.wantSHA {
				t.Fatalf("sha: got %q, want %q", e.SHA, c.wantSHA)
			}
		})
	}
}

// A site with NO root index.html cannot SPA-fall-back: a route-shaped
// miss has nothing to serve, so it 404s rather than serving some other
// index.
func TestManifest_LookupWithSPAFallback_NoRootIndex(t *testing.T) {
	m := mustManifest(map[string]string{
		"blog/index.html": "blog", // only a nested index, no root index.html
	})
	if _, hit, _ := m.LookupWithSPAFallback("/about"); hit {
		t.Fatalf("route miss with no root index.html should 404, got a hit")
	}
	// A direct hit on the nested index still works.
	if _, hit, via := m.LookupWithSPAFallback("/blog/"); !hit || via {
		t.Fatalf("nested index direct hit: hit=%v via=%v, want true,false", hit, via)
	}
}

func TestManifest_HasWebContent(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"index.html", map[string]string{"index.html": "x"}, true},
		{"nested index", map[string]string{"app/index.html": "x"}, true},
		{"css only", map[string]string{"style.css": "x"}, true},
		{"js only", map[string]string{"app.js": "x"}, true},
		{"html in dir", map[string]string{"about/page.html": "x"}, true},
		{"images only", map[string]string{"logo.png": "x", "data.json": "y"}, false},
		{"text only", map[string]string{"readme.txt": "x"}, false},
		{"empty", map[string]string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustManifest(c.files).HasWebContent(); got != c.want {
				t.Fatalf("HasWebContent: got %v, want %v", got, c.want)
			}
		})
	}
}

func TestManifest_DedupedSize(t *testing.T) {
	m := NewManifest()
	m.Add("a.html", ManifestEntry{SHA: "x", Size: 100})
	m.Add("b.html", ManifestEntry{SHA: "x", Size: 100}) // same blob
	m.Add("c.css", ManifestEntry{SHA: "y", Size: 50})
	if got := m.DedupedSize(); got != 150 {
		t.Fatalf("DedupedSize: got %d, want 150", got)
	}
}

func TestManifest_SHASet(t *testing.T) {
	m := NewManifest()
	m.Add("a.html", ManifestEntry{SHA: "x", Size: 1})
	m.Add("b.html", ManifestEntry{SHA: "x", Size: 1})
	m.Add("c.css", ManifestEntry{SHA: "y", Size: 1})
	set := m.SHASet()
	if len(set) != 2 {
		t.Fatalf("SHASet: got %d distinct, want 2 (%v)", len(set), set)
	}
}

func TestContentTypeForPath(t *testing.T) {
	cases := map[string]string{
		"index.html":  "text/html; charset=utf-8",
		"style.css":   "text/css; charset=utf-8",
		"app.js":      "text/javascript; charset=utf-8",
		"mod.mjs":     "text/javascript; charset=utf-8",
		"data.json":   "application/json; charset=utf-8",
		"logo.svg":    "image/svg+xml",
		"pic.png":     "image/png",
		"font.woff2":  "font/woff2",
		"mystery.xyz": "application/octet-stream", // unknown → download, never text/html
		"noext":       "application/octet-stream",
	}
	for p, want := range cases {
		if got := ContentTypeForPath(p); got != want {
			t.Fatalf("%q: got %q, want %q", p, got, want)
		}
	}
}

func TestDetectKind_Archive(t *testing.T) {
	gzipBytes := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	// No hint: gzip magic → KindSite.
	if k, err := DetectKind(gzipBytes, ""); err != nil || k != KindSite {
		t.Fatalf("gzip no hint: got (%q, %v), want (site, nil)", k, err)
	}
	// A text hint must NOT smuggle a gzip stream through as HTML.
	if _, err := DetectKind(gzipBytes, "html"); err == nil {
		t.Fatalf("gzip with html hint should reject")
	}
	// Non-gzip bytes are unaffected by the new branch.
	if k, err := DetectKind([]byte("<!doctype html><h1>x</h1>"), ""); err != nil || k != KindHTML {
		t.Fatalf("html still detected: got (%q, %v)", k, err)
	}
}
