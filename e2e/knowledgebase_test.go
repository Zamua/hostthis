//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// kbFiller pads a section so the reading pane scrolls, which scroll-spy needs.
var kbFiller = strings.Repeat("Filler text that pads this section so the reading pane scrolls.\n\n", 8)

// knowledgeBaseFiles is a base with nested directories, a README, several
// headings per document, relative links both down into a subdirectory and back
// up out of one, an HTML file and a binary asset. "quokka" appears in one
// document's BODY and in no path or heading, so a search for it can only match
// through the text index.
var knowledgeBaseFiles = map[string]string{
	"README.md": "# Knowledge base root\n\nStart with the [setup guide](guides/setup.md) " +
		"or the [api reference](reference/api.md).\n\n" +
		"## Overview\n\n" + kbFiller +
		"## What is inside\n\n" + kbFiller,
	"guides/setup.md": "# Setup\n\n[Tuning](advanced/tuning.md) goes deeper.\n\n" +
		"## Install the daemon\n\n" + kbFiller +
		"## Configure the listener\n\nThe quokka setting is documented here.\n\n" + kbFiller +
		"## Verify the install\n\n" + kbFiller,
	"guides/advanced/tuning.md": "# Tuning\n\n[Back to the setup guide](../setup.md).\n\n" +
		"## Cache sizing\n\n" + kbFiller,
	"reference/api.md": "# API reference\n\n## Endpoints\n\n" + kbFiller,
	"notes/page.html":  "<!doctype html><title>raw page</title><p>served raw</p>",
	"assets/logo.png":  "\x89PNG\r\n\x1a\n\x00not really a png",
}

// kbPaths is every path in knowledgeBaseFiles, sorted.
var kbPaths = []string{
	"README.md", "assets/logo.png", "guides/advanced/tuning.md",
	"guides/setup.md", "notes/page.html", "reference/api.md",
}

// kbView is the shell's whole visible state, read in one pass so a failure
// names every signal rather than only the first.
type kbView struct {
	Path          string   `json:"path"`
	DocPath       string   `json:"docPath"`
	Busy          string   `json:"busy"`
	Heading       string   `json:"heading"`
	Crumbs        []string `json:"crumbs"`
	TreeReady     string   `json:"treeReady"`
	TreeFiles     string   `json:"treeFiles"`
	FilePaths     []string `json:"filePaths"`
	DirPaths      []string `json:"dirPaths"`
	Current       []string `json:"current"`
	TOCHidden     bool     `json:"tocHidden"`
	TOCLinks      []string `json:"tocLinks"`
	TOCActive     []string `json:"tocActive"`
	Marker        string   `json:"marker"`
	HistoryLength int      `json:"historyLength"`
}

const readKBView = `(() => {
  const doc = document.getElementById("kb-doc");
  const tree = document.getElementById("kb-tree");
  const text = (el) => (el ? el.textContent.trim() : "");
  const all = (sel) => Array.from(document.querySelectorAll(sel));
  return {
    path: location.pathname + location.hash,
    docPath: doc.dataset.path || "",
    busy: doc.getAttribute("aria-busy") || "",
    heading: text(doc.querySelector("h1")),
    crumbs: all("#kb-crumbs .kb-crumb").map((c) => c.dataset.crumbKind + ":" + text(c)),
    treeReady: tree.dataset.treeReady || "",
    treeFiles: tree.dataset.files || "",
    filePaths: all("#kb-tree a.kb-file").map((a) => a.dataset.path).sort(),
    dirPaths: all("#kb-tree button.kb-dir").map((b) => b.dataset.dir).sort(),
    current: all("#kb-tree a.kb-file.current").map((a) => a.dataset.path + ":" + a.getAttribute("aria-current")),
    tocHidden: document.getElementById("kb-toc").hidden,
    tocLinks: all("#kb-toc-list a.kb-toc-link").map((a) => a.dataset.targetId),
    tocActive: all("#kb-toc-list a.kb-toc-link[aria-current='true']").map((a) => a.dataset.targetId),
    marker: window.__kbMarker || "",
    historyLength: history.length,
  };
})()`

// kbTarget is where the fragment's heading sits relative to the reading pane.
type kbTarget struct {
	Hash   string `json:"hash"`
	ID     string `json:"id"`
	Found  bool   `json:"found"`
	InView bool   `json:"inView"`
}

const readKBTarget = `(() => {
  const main = document.getElementById("kb-main");
  const id = decodeURIComponent(location.hash.replace(/^#/, ""));
  const el = id ? document.getElementById(id) : null;
  const mr = main.getBoundingClientRect();
  const er = el ? el.getBoundingClientRect() : null;
  return {
    hash: location.hash, id: id, found: !!el,
    inView: !!er && er.top >= mr.top && er.bottom <= mr.bottom,
  };
})()`

// settled is the selector for a document the shell has finished painting:
// openDoc sets the path and marks the article busy, and paint clears busy.
func settled(path string) string {
	return `#kb-doc[data-path="` + path + `"]:not([aria-busy])`
}

// clearSearch empties the search box. Assigned directly because chromedp's
// SetValue refuses a search input, and the shell reads the field rather than
// the keystrokes.
var clearSearch = chromedp.Evaluate(`document.getElementById("kb-search").value = ""`, nil)

// clickSel clicks through the DOM rather than at a coordinate: the shell's own
// handlers key on an unmodified left button, which a synthesized click is, and
// no assertion here is about hit-testing.
func clickSel(sel string) chromedp.Action {
	return chromedp.Evaluate(`(() => {
	  const el = document.querySelector(`+"`"+sel+"`"+`);
	  if (!el) throw new Error("no element for " + `+"`"+sel+"`"+`);
	  el.click();
	})()`, nil)
}

// The knowledge base shell browses a directory of documents: it opens the
// root's README, navigates by tree, by relative link and by direct URL, and
// resolves a heading fragment on load and on hashchange.
func TestKnowledgeBase(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base")
	br.Open(t, paste.URL)

	// -- the root renders README.md, listing the whole base -------------------
	var root kbView
	flow.settle("root", "kb-doc",
		chromedp.WaitVisible(`#kb-tree[data-tree-ready="1"]`, chromedp.ByQuery),
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &root),
	)
	flow.Shot("root")

	if root.Heading != "Knowledge base root" {
		t.Errorf("root h1 = %q, want the README's heading %q", root.Heading, "Knowledge base root")
	}
	if root.DocPath != "README.md" {
		t.Errorf("root document = %q, want README.md", root.DocPath)
	}
	if !slices.Equal(root.FilePaths, kbPaths) {
		t.Errorf("file tree lists %q, want every file %q", root.FilePaths, kbPaths)
	}
	if root.TreeFiles != "6" {
		t.Errorf("tree data-files = %q, want 6", root.TreeFiles)
	}
	if want := []string{"assets", "guides", "guides/advanced", "notes", "reference"}; !slices.Equal(root.DirPaths, want) {
		t.Errorf("tree directories = %q, want %q", root.DirPaths, want)
	}
	// The root names the document it chose to render, so a reader can tell
	// which file the root landed on.
	if want := []string{"root:root", "file:README.md"}; !slices.Equal(root.Crumbs, want) {
		t.Errorf("root crumbs = %q, want %q", root.Crumbs, want)
	}
	if want := []string{"knowledge-base-root", "overview", "what-is-inside"}; !slices.Equal(root.TOCLinks, want) {
		t.Errorf("root TOC = %q, want the README's headings %q", root.TOCLinks, want)
	}

	// -- a tree click swaps the document in place -----------------------------
	var doc kbView
	flow.settle("document", "kb-doc",
		// The marker dies with the document, so its survival is what separates
		// a pushState swap from a reload that happens to land on the same URL.
		chromedp.Evaluate(`window.__kbMarker = "alive"`, nil),
		clickSel(`#kb-tree a.kb-file[data-path="guides/setup.md"]`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &doc),
	)
	flow.Shot("document")

	if doc.Marker != "alive" {
		t.Errorf("opening a document reloaded the page: marker = %q, want %q", doc.Marker, "alive")
	}
	if doc.HistoryLength <= root.HistoryLength {
		t.Errorf("history length %d did not grow from %d, so the navigation did not pushState",
			doc.HistoryLength, root.HistoryLength)
	}
	if !strings.HasSuffix(doc.Path, "/guides/setup.md") {
		t.Errorf("URL = %q, want it to end in /guides/setup.md", doc.Path)
	}
	if doc.Heading != "Setup" {
		t.Errorf("h1 = %q, want %q", doc.Heading, "Setup")
	}
	want := []string{"root:root", "dir:guides", "file:setup.md"}
	if !slices.Equal(doc.Crumbs, want) {
		t.Errorf("crumbs = %q, want %q", doc.Crumbs, want)
	}
	wantTOC := []string{"setup", "install-the-daemon", "configure-the-listener", "verify-the-install"}
	if !slices.Equal(doc.TOCLinks, wantTOC) {
		t.Errorf("TOC = %q, want this document's headings %q", doc.TOCLinks, wantTOC)
	}
	if w := []string{"guides/setup.md:page"}; !slices.Equal(doc.Current, w) {
		t.Errorf("tree current entry = %q, want %q", doc.Current, w)
	}

	// -- relative links between documents, down and back up -------------------
	var down, up kbView
	flow.settle("relative-down", "kb-doc",
		clickSel(`#kb-doc a[href="advanced/tuning.md"]`),
		chromedp.WaitVisible(settled("guides/advanced/tuning.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &down),
	)
	if down.Heading != "Tuning" {
		t.Errorf("relative link into a subdirectory opened %q, want Tuning", down.Heading)
	}
	if down.Marker != "alive" {
		t.Error("following a relative link reloaded the page")
	}
	flow.settle("relative-up", "kb-doc",
		clickSel(`#kb-doc a[href="../setup.md"]`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &up),
	)
	if up.Heading != "Setup" {
		t.Errorf("relative link out of a subdirectory opened %q, want Setup", up.Heading)
	}

	// -- a direct load of the same nested path renders the same document ------
	var direct kbView
	br.Open(t, paste.URL+"/guides/setup.md")
	flow.settle("direct", "kb-doc",
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &direct),
	)
	if direct.Marker != "" {
		t.Error("the direct load did not reload, so it proves nothing about a typed URL")
	}
	if direct.Heading != doc.Heading || !slices.Equal(direct.Crumbs, doc.Crumbs) ||
		!slices.Equal(direct.TOCLinks, doc.TOCLinks) || !slices.Equal(direct.Current, doc.Current) {
		t.Errorf("a typed URL renders differently from clicking to it:\n direct %+v\n clicked %+v", direct, doc)
	}

	// -- a heading fragment resolves on load, and again on hashchange ---------
	br.Open(t, paste.URL+"/guides/setup.md#configure-the-listener")
	var onLoad, onChange kbTarget
	flow.settle("deep-link", "kb-doc",
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		// The browser resolved the fragment against an empty document at parse
		// time, so the shell resolving it is an event later than the paint.
		chromedp.Poll(`(() => {
		  const main = document.getElementById("kb-main");
		  const el = document.getElementById("configure-the-listener");
		  return !!el && el.getBoundingClientRect().top >= main.getBoundingClientRect().top;
		})()`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBTarget, &onLoad),
	)
	flow.Shot("deep-link")
	if !onLoad.Found || !onLoad.InView {
		t.Errorf("fragment %q did not resolve into the reading pane: %+v", "#configure-the-listener", onLoad)
	}

	flow.settle("hashchange", "kb-doc",
		chromedp.Evaluate(`location.hash = "#verify-the-install"`, nil),
		chromedp.Poll(`(() => {
		  const main = document.getElementById("kb-main");
		  const el = document.getElementById("verify-the-install");
		  return !!el && el.getBoundingClientRect().top >= main.getBoundingClientRect().top;
		})()`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBTarget, &onChange),
	)
	if !onChange.Found || !onChange.InView {
		t.Errorf("hashchange to %q did not resolve: %+v", "#verify-the-install", onChange)
	}

	// -- scroll-spy marks the heading the reader is under ---------------------
	var spied kbView
	flow.settle("scroll-spy", "kb-toc-list",
		// Anchored at the top of the pane: the heading the reader has reached,
		// not whichever one is merely on screen.
		chromedp.Evaluate(`document.getElementById("install-the-daemon").scrollIntoView({block: "start"})`, nil),
		chromedp.Poll(`(() => {
		  const a = document.querySelector("#kb-toc-list a.kb-toc-link[aria-current='true']");
		  return !!a && a.dataset.targetId === "install-the-daemon";
		})()`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBView, &spied),
	)
	if w := []string{"install-the-daemon"}; !slices.Equal(spied.TOCActive, w) {
		t.Errorf("scroll-spy marked %q active, want %q", spied.TOCActive, w)
	}

	// -- a non-markdown entry links out instead of rendering in the shell -----
	var ext struct {
		Count    int    `json:"count"`
		Target   string `json:"target"`
		Rel      string `json:"rel"`
		External string `json:"external"`
		Href     string `json:"href"`
	}
	if err := chromedp.Run(br.Ctx, chromedp.Evaluate(`(() => {
	  const links = document.querySelectorAll('#kb-tree a.kb-ext[data-external="1"]');
	  const a = document.querySelector('#kb-tree a.kb-file[data-path="notes/page.html"]');
	  return {count: links.length, target: a.target, rel: a.rel,
	    external: a.dataset.external || "", href: a.href};
	})()`, &ext)); err != nil {
		t.Fatalf("read the external entry: %v", err)
	}
	if ext.Count != 2 {
		t.Errorf("%d entries marked external, want 2 (the html file and the png)", ext.Count)
	}
	if ext.Target != "_blank" || ext.External != "1" || !strings.Contains(ext.Rel, "noopener") {
		t.Errorf("notes/page.html entry = %+v, want an external link opening in a new tab", ext)
	}
	// The other half of "does not render inside the shell": its own URL serves
	// the file's bytes, never the shell that would render them.
	body, ctype := get(t, ext.Href)
	if !strings.Contains(body, "served raw") || strings.Contains(body, "/_hostthis/kb.js") {
		t.Errorf("GET %s served %q (%s), want the raw file rather than the shell", ext.Href, body, ctype)
	}

	br.AssertNoPageErrors(t)
}

// Search matches a term that occurs only in a document's body, opens the file
// it found, matches on path alone, and reports what the index covers.
func TestKnowledgeBaseSearch(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base search"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-search")
	br.Open(t, paste.URL)

	var idle kbSearch
	flow.settle("indexed", "kb-index-note",
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		// Bodies are indexed in the background, so a search before this is a
		// race against the fetch rather than a test of matching.
		chromedp.WaitReady(`body[data-kb-index="complete"]`, chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &idle),
	)
	if idle.Indexed != "4" || idle.Total != "4" {
		t.Errorf("index note covers %s of %s documents, want 4 of 4 markdown files", idle.Indexed, idle.Total)
	}
	// A complete index has nothing to warn about, so the note stays out of the
	// way; a partial one is what has to speak up.
	if !idle.NoteHidden {
		t.Errorf("index note is visible on a complete index: %q", idle.NoteText)
	}

	var text kbSearch
	flow.settle("search-text", "kb-hits",
		chromedp.SendKeys("#kb-search", "quokka", chromedp.ByQuery),
		chromedp.WaitVisible("#kb-results .kb-hit", chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &text),
	)
	flow.Shot("search-results")
	if len(text.Hits) != 1 {
		t.Fatalf("searching a body-only term returned %d hits, want 1: %+v", len(text.Hits), text.Hits)
	}
	if text.Hits[0].Path != "guides/setup.md" || text.Hits[0].Kind != "text" {
		t.Errorf("body hit = %+v, want a text match in guides/setup.md", text.Hits[0])
	}
	if !strings.Contains(text.Hits[0].Text, "quokka") {
		t.Errorf("hit snippet %q does not show the match", text.Hits[0].Text)
	}
	if text.State != "complete" {
		t.Errorf("index state = %q, want complete", text.State)
	}

	// -- the hit opens the file it named --------------------------------------
	var opened kbView
	flow.settle("search-open", "kb-doc",
		clickSel(`#kb-hits .kb-hit`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &opened),
	)
	if opened.Heading != "Setup" {
		t.Errorf("the hit opened %q, want Setup", opened.Heading)
	}
	if !strings.HasSuffix(opened.Path, "/guides/setup.md") {
		t.Errorf("URL after opening the hit = %q, want it to end in /guides/setup.md", opened.Path)
	}

	// -- a path-only term still matches, because paths never need the index ---
	var byPath kbSearch
	flow.settle("search-path", "kb-hits",
		clearSearch,
		chromedp.SendKeys("#kb-search", "logo", chromedp.ByQuery),
		chromedp.WaitVisible(`#kb-hits .kb-hit[data-kind="path"]`, chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &byPath),
	)
	if len(byPath.Hits) != 1 || byPath.Hits[0].Path != "assets/logo.png" {
		t.Fatalf("path search returned %+v, want the one png", byPath.Hits)
	}
	// A binary file is not rendered by the shell, so its hit leaves too.
	if byPath.Hits[0].External != "1" {
		t.Errorf("path hit for a binary file = %+v, want an external link", byPath.Hits[0])
	}

	var none kbSearch
	flow.settle("search-empty", "kb-results-empty",
		clearSearch,
		chromedp.SendKeys("#kb-search", "nosuchterm", chromedp.ByQuery),
		chromedp.WaitVisible("#kb-results-empty", chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &none),
	)
	if len(none.Hits) != 0 || none.EmptyHidden {
		t.Errorf("a term matching nothing = %+v, want no hits and the empty note shown", none)
	}

	br.AssertNoPageErrors(t)
}

// A base holding no markdown at all renders the shell's generated listing
// rather than an error or a blank page.
func TestKnowledgeBaseWithoutMarkdown(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, map[string]string{
		"notes/page.html":  "<!doctype html><title>raw page</title><p>served raw</p>",
		"data/values.json": `{"answer":42}`,
		"assets/logo.png":  "\x89PNG\r\n\x1a\n\x00not really a png",
	}, UploadOpts{Name: "knowledge base listing"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-listing")
	br.Open(t, paste.URL)

	var got struct {
		Heading  string   `json:"heading"`
		Listing  []string `json:"listing"`
		External int      `json:"external"`
		Error    bool     `json:"error"`
		DocText  string   `json:"docText"`
		TOC      bool     `json:"tocHidden"`
	}
	flow.settle("listing", "kb-doc",
		chromedp.WaitVisible(`#kb-tree[data-tree-ready="1"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#kb-listing", chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
		  const doc = document.getElementById("kb-doc");
		  const items = Array.from(document.querySelectorAll("#kb-listing a"));
		  return {
		    heading: (doc.querySelector("h1")||{}).textContent.trim(),
		    listing: items.map((a) => a.dataset.path).sort(),
		    external: items.filter((a) => a.dataset.external === "1" && a.target === "_blank").length,
		    error: !!document.getElementById("kb-error"),
		    docText: doc.textContent.trim().slice(0, 80),
		    tocHidden: document.getElementById("kb-toc").hidden,
		  };
		})()`, &got),
	)
	flow.Shot("listing")

	if got.Error {
		t.Errorf("a base with no markdown rendered an error panel: %q", got.DocText)
	}
	if got.Heading != "Files" {
		t.Errorf("listing heading = %q, want Files", got.Heading)
	}
	want := []string{"assets/logo.png", "data/values.json", "notes/page.html"}
	if !slices.Equal(got.Listing, want) {
		t.Errorf("listing = %q, want every file %q", got.Listing, want)
	}
	// Nothing here renders in the shell, so every entry leaves to its own URL.
	if got.External != len(want) {
		t.Errorf("%d of %d listing entries link out, want all of them", got.External, len(want))
	}
	if !got.TOC {
		t.Error("the table of contents is shown for a document with no headings")
	}

	br.AssertNoPageErrors(t)
}

// On a phone both sidebars are drawers over the document: closed until asked
// for, and closed again by opening a file.
func TestKnowledgeBaseOnAPhone(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base phone"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-mobile")
	if err := chromedp.Run(br.Ctx, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatalf("phone viewport: %v", err)
	}
	br.Open(t, paste.URL)

	var closed, open, afterOpenFile, tocOpen kbPanes
	flow.settle("phone-root", "kb-doc",
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBPanes, &closed),
	)
	flow.Shot("phone-root")
	// Off-canvas rather than removed: the drawer slides in, so it stays laid
	// out and only its position says whether it is showing.
	if closed.BodyClasses != "" || closed.ScrimDisplay != "none" {
		t.Errorf("a phone-width page did not start with both drawers closed: %+v", closed)
	}
	if closed.TreeRight > 0 || closed.TOCLeft < closed.Width {
		t.Errorf("a drawer is on screen before it was asked for: %+v", closed)
	}

	// Each drawer is waited for by its POSITION: the class lands when the
	// transition starts, so polling on it would photograph a half-open drawer
	// and read its transform mid-slide.
	flow.settle("files-drawer", "kb-tree",
		chromedp.Click("#kb-files-toggle", chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("kb-tree").getBoundingClientRect().left >= 0`, nil,
			chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBPanes, &open),
	)
	flow.Shot("files-drawer")
	if !strings.Contains(open.BodyClasses, "tree-open") || open.ScrimDisplay != "block" {
		t.Errorf("the files drawer did not open over the document: %+v", open)
	}

	flow.settle("phone-document", "kb-doc",
		clickSel(`#kb-tree a.kb-file[data-path="guides/setup.md"]`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("kb-tree").getBoundingClientRect().right <= 0`, nil,
			chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBPanes, &afterOpenFile),
	)
	flow.Shot("phone-document")
	// Opening a file closes the drawer: leaving it over the document would hide
	// what the tap just asked for.
	if afterOpenFile.BodyClasses != "" || afterOpenFile.ScrimDisplay != "none" {
		t.Errorf("the drawer outlived the file it opened: %+v", afterOpenFile)
	}

	flow.settle("toc-drawer", "kb-toc",
		chromedp.Click("#kb-toc-toggle", chromedp.ByQuery),
		chromedp.Poll(`(() => {
		  const r = document.getElementById("kb-toc").getBoundingClientRect();
		  return r.right <= window.innerWidth;
		})()`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBPanes, &tocOpen),
	)
	flow.Shot("toc-drawer")
	if !strings.Contains(tocOpen.BodyClasses, "toc-open") || tocOpen.ScrimDisplay != "block" {
		t.Errorf("the contents drawer did not open over the document: %+v", tocOpen)
	}
	if tocOpen.TOCLeft >= tocOpen.Width {
		t.Errorf("the contents drawer is still off screen: %+v", tocOpen)
	}

	br.AssertNoPageErrors(t)
}

// kbSearch is the search surface: the results, and what the index says it
// covers.
type kbSearch struct {
	State       string  `json:"state"`
	Hidden      bool    `json:"hidden"`
	NoteHidden  bool    `json:"noteHidden"`
	NoteText    string  `json:"noteText"`
	Indexed     string  `json:"indexed"`
	Total       string  `json:"total"`
	EmptyHidden bool    `json:"emptyHidden"`
	Truncated   bool    `json:"truncated"`
	Hits        []kbHit `json:"hits"`
}

type kbHit struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Hash     string `json:"hash"`
	External string `json:"external"`
	Text     string `json:"text"`
}

const readKBSearch = `(() => {
  const note = document.getElementById("kb-index-note");
  return {
    state: document.body.dataset.kbIndex || "",
    hidden: document.getElementById("kb-results").hidden,
    noteHidden: note.hidden,
    noteText: note.textContent.trim(),
    indexed: note.dataset.indexed || "",
    total: note.dataset.total || "",
    emptyHidden: document.getElementById("kb-results-empty").hidden,
    truncated: !!document.getElementById("kb-results-truncated"),
    hits: Array.from(document.querySelectorAll("#kb-hits .kb-hit")).map((a) => ({
      path: a.dataset.path, kind: a.dataset.kind, hash: a.dataset.hash || "",
      external: a.dataset.external || "", text: a.textContent.trim(),
    })),
  };
})()`

// kbPanes is the state of the two sidebars, which are columns on a wide screen
// and drawers on a narrow one. Positions rather than transforms: a drawer is
// open when it is on screen, which is true whatever animates it there.
type kbPanes struct {
	BodyClasses  string  `json:"bodyClasses"`
	ScrimDisplay string  `json:"scrimDisplay"`
	TreeLeft     float64 `json:"treeLeft"`
	TreeRight    float64 `json:"treeRight"`
	TOCLeft      float64 `json:"tocLeft"`
	TOCRight     float64 `json:"tocRight"`
	Width        float64 `json:"width"`
}

const readKBPanes = `(() => {
  const rect = (id) => document.getElementById(id).getBoundingClientRect();
  const tree = rect("kb-tree"), toc = rect("kb-toc");
  return {
    bodyClasses: document.body.className,
    scrimDisplay: getComputedStyle(document.getElementById("kb-scrim")).display,
    treeLeft: tree.left, treeRight: tree.right,
    tocLeft: toc.left, tocRight: toc.right,
    width: window.innerWidth,
  };
})()`

// get fetches a URL the page linked to, for an assertion about what the server
// serves rather than about what the shell drew.
func get(t *testing.T, url string) (body, contentType string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b), resp.Header.Get("Content-Type")
}
