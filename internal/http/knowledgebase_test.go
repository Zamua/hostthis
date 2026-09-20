package http

import (
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

var kbUpdated = time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)

// buildKBServer wires a knowledge base: markdown, an HTML file, a binary asset
// and a file whose name collides with the file-list query, under no root index.
func buildKBServer(t *testing.T) *Server {
	t.Helper()
	m := domain.NewManifest()
	for _, f := range []struct{ path, key string }{
		{"README.md", "key-readme"},
		{"guide/setup.md", "key-setup"},
		{"guide/diagram.png", "key-png"},
		{"report.html", "key-html"},
		{"files.json", "key-files"},
	} {
		m.Add(f.path, domain.ManifestEntry{Key: f.key, Size: 4, ContentType: domain.ContentTypeForPath(f.path)})
	}
	return &Server{
		ApexDomain: "paste.test",
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Identity: "key:test", Status: domain.PasteStatusReady,
			Kind: domain.KindKnowledgeBase, UploadID: "up-kb", UpdatedAt: kbUpdated, Manifest: m,
		}},
		Blobs: stubBlobMap{m: map[string][]byte{
			"key-readme": []byte("# notes\n"),
			"key-setup":  []byte("# setup\n"),
			"key-png":    []byte("\x89PNG"),
			"key-html":   []byte("<h1>report</h1>"),
			"key-files":  []byte(`{"own":"file"}`),
		}},
	}
}

func kbGet(t *testing.T, mux stdhttp.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	r.Host = "abc23456.paste.test"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// The shell answers the root, every .md path and a route-shaped miss; a missing
// asset 404s; every other file serves raw with the type its extension implies.
func TestKnowledgeBase_Serves(t *testing.T) {
	srv := buildKBServer(t)
	mux := srv.Handler()
	shell := shells[domain.KindKnowledgeBase]
	if shell == nil {
		t.Fatal("no shell registered for the knowledge base kind")
	}
	page := string(shell.html(domain.KindKnowledgeBase))

	cases := []struct {
		name, path string
		code       int
		body       string // "" on a shell path: the shell page is asserted instead
		ctype      string
	}{
		{"root", "/", 200, "", "text/html; charset=utf-8"},
		{"markdown at the root", "/README.md", 200, "", "text/html; charset=utf-8"},
		{"nested markdown", "/guide/setup.md", 200, "", "text/html; charset=utf-8"},
		{"route-shaped miss", "/search", 200, "", "text/html; charset=utf-8"},
		{"directory with no index", "/guide/", 200, "", "text/html; charset=utf-8"},
		{"missing markdown is a route", "/guide/absent.md", 200, "", "text/html; charset=utf-8"},
		{"raw markdown", "/guide/setup.md?raw=1", 200, "# setup\n", "text/markdown; charset=utf-8"},
		{"html links out and serves raw", "/report.html", 200, "<h1>report</h1>", "text/html; charset=utf-8"},
		{"binary file serves raw", "/guide/diagram.png", 200, "\x89PNG", "image/png"},
		{"a file named like the query", "/files.json", 200, `{"own":"file"}`, "application/json; charset=utf-8"},
		{"missing script", "/app.js", 404, "", ""},
		{"missing image", "/guide/absent.png", 404, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := kbGet(t, mux, c.path)
			if w.Code != c.code {
				t.Fatalf("code: got %d, want %d", w.Code, c.code)
			}
			if c.code != 200 {
				return
			}
			if ct := w.Header().Get("Content-Type"); ct != c.ctype {
				t.Fatalf("content-type: got %q, want %q", ct, c.ctype)
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatalf("missing sandbox headers")
			}
			if c.body != "" {
				if w.Body.String() != c.body {
					t.Fatalf("body: got %q, want %q", w.Body.String(), c.body)
				}
				if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
					t.Fatalf("a raw file carries a CSP: %q", csp)
				}
				return
			}
			if w.Body.String() != page {
				t.Fatalf("body is not the shell page: %q", w.Body.String())
			}
			if csp := w.Header().Get("Content-Security-Policy"); csp != shell.policy() {
				t.Fatalf("shell CSP: got %q, want %q", csp, shell.policy())
			}
			if etag := w.Header().Get("ETag"); etag != `"`+shell.version+`"` {
				t.Fatalf("shell ETag: got %q, want the shell version", etag)
			}
		})
	}
}

// ?files=1 answers with the whole base's paths whichever of its paths carries
// the query, and a file named like the query does not shadow it.
func TestKnowledgeBase_FileList(t *testing.T) {
	mux := buildKBServer(t).Handler()
	want := []string{"README.md", "files.json", "guide/diagram.png", "guide/setup.md", "report.html"}

	for _, target := range []string{"/?files=1", "/guide/setup.md?files=1", "/files.json?files=1", "/search?files=1"} {
		w := kbGet(t, mux, target)
		if w.Code != 200 {
			t.Fatalf("%s: code %d, want 200", target, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("%s: content-type %q, want json", target, ct)
		}
		var got []string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: body is not a JSON array of paths: %v (%q)", target, err, w.Body.String())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: files %v, want %v", target, got, want)
		}
	}
}

// The shell is a page plus its assets. Every script and stylesheet it names
// must be whitelisted - an unlisted one 404s while the page still serves 200,
// so nothing else fails - and every file shipped under the shell's directory
// must be reachable or it is dead weight in the binary.
func TestKnowledgeBaseShell_AssetsAreWiredAndServable(t *testing.T) {
	sh := shells[domain.KindKnowledgeBase]
	if sh == nil {
		t.Fatal("no shell registered for the knowledge base kind")
	}
	page := string(sh.html(domain.KindKnowledgeBase))

	// marked and DOMPurify are the markdown shell's and deeplink.js is shared;
	// the flat asset namespace is what lets this page load them by name.
	for _, name := range []string{
		"kb.css", "kb.js", "kb-tree.js", "kb-search.js",
		"deeplink.js", "marked.min.js", "purify.min.js",
	} {
		if !strings.Contains(page, "/_hostthis/"+name+"?v=") {
			t.Errorf("the shell page does not load %s", name)
		}
		if _, ok := assetSource[name]; !ok {
			t.Errorf("the page loads %s but no shell whitelists it", name)
		}
	}

	entries, err := sh.fs.ReadDir(sh.dir)
	if err != nil {
		t.Fatalf("read %s: %v", sh.dir, err)
	}
	for _, e := range entries {
		// shell.html IS the page; it is never served as an asset.
		if e.Name() == "shell.html" {
			continue
		}
		if _, ok := sh.assets[e.Name()]; !ok {
			t.Errorf("%s/%s is embedded but not whitelisted, so the browser 404s on it", sh.dir, e.Name())
		}
	}
}

// The interface hangs off these elements: the tree, the document, the table of
// contents, the breadcrumbs and the search surface. They are the handles the
// browser suite targets, so a rename has to be a deliberate change here too.
func TestKnowledgeBaseShell_StructuralHooks(t *testing.T) {
	page := string(shells[domain.KindKnowledgeBase].html(domain.KindKnowledgeBase))
	for _, hook := range []string{
		`id="kb-tree"`, `id="kb-doc"`, `id="kb-main"`, `id="kb-crumbs"`,
		`id="kb-toc"`, `id="kb-toc-list"`, `id="kb-search"`, `id="kb-results"`,
		`id="kb-hits"`, `id="kb-index-note"`, `id="kb-raw"`,
		`id="kb-files-toggle"`, `id="kb-toc-toggle"`,
	} {
		if !strings.Contains(page, hook) {
			t.Errorf("the shell page is missing %s", hook)
		}
	}
}

// A site ignores the query: ?files=1 is a knowledge base's surface, and a site
// path must keep serving its file.
func TestSite_IgnoresFileListQuery(t *testing.T) {
	mux := buildSiteServer(t).Handler()
	w := siteGet(t, mux, false, "/index.html?files=1")
	if w.Code != 200 || w.Body.String() != "<h1>root</h1>" {
		t.Fatalf("site file under ?files=1: code %d body %q", w.Code, w.Body.String())
	}
}
