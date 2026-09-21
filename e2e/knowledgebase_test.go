//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// kbFiller pads a section so the reading pane scrolls, which scroll-spy needs.
var kbFiller = strings.Repeat("Filler text that pads this section so the reading pane scrolls.\n\n", 8)

// knowledgeBaseFiles is a base with nested directories, a README, several
// headings per document, relative links both down into a subdirectory and back
// up out of one, an HTML file and a binary asset. "quokka" appears in one
// document's BODY and in no path or heading, so a search for it can only match
// through the text index. The README carries one of every link kind the shell
// tells apart: markdown in this base, a file it cannot render, another origin,
// a non-web target, and a link the sanitizer strips the target from.
var knowledgeBaseFiles = map[string]string{
	"README.md": "# Knowledge base root\n\nStart with the [setup guide](guides/setup.md) " +
		"or the [api reference](reference/api.md).\n\n" +
		"Also [the project site](https://example.com/docs), the [raw page](notes/page.html), " +
		"[write in](mailto:nobody@example.com) and <a href=\"javascript:void(0)\">a stripped link</a>.\n\n" +
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

// deepNames are ten nested folder names, long enough that the full path cannot
// fit the breadcrumb bar at any viewport this suite uses.
var deepNames = []string{
	"architecture-decisions", "platform-services", "identity-and-access",
	"session-management", "token-lifecycle", "refresh-strategies",
	"rotation-policies", "failure-modes", "observability-hooks", "runbooks",
}

var (
	deepDir  = strings.Join(deepNames, "/")
	deepFile = deepDir + "/level.md"
)

// deepBaseFiles is a base ten folders deep, one document per level and two at
// the bottom, so navigating it means walking a level at a time and switching
// documents at the same depth.
var deepBaseFiles = func() map[string]string {
	files := map[string]string{"README.md": "# Deep base\n\nTen folders below this one.\n"}
	for i := range deepNames {
		dir := strings.Join(deepNames[:i+1], "/")
		files[dir+"/level.md"] = fmt.Sprintf("# Level %d\n\nThis document lives at %s.\n", i+1, dir)
	}
	files[deepDir+"/notes.md"] = "# Deep notes\n\nA second document at the bottom.\n"
	return files
}()

// wideContentFiles is what a reading pane has to CONTAIN rather than be widened
// by: a twelve-column table and a three-hundred-character code line.
var wideContentFiles = func() map[string]string {
	head := make([]string, 12)
	rule := make([]string, 12)
	cells := make([]string, 12)
	for i := range head {
		head[i] = fmt.Sprintf("column %d header", i+1)
		rule[i] = "---"
		cells[i] = fmt.Sprintf("row value %d", i+1)
	}
	table := "| " + strings.Join(head, " | ") + " |\n" +
		"| " + strings.Join(rule, " | ") + " |\n" +
		"| " + strings.Join(cells, " | ") + " |\n"
	return map[string]string{
		"README.md": "# Wide content\n\n" + table + "\n```\n" + strings.Repeat("x", 300) + "\n```\n",
	}
}()

// kbView is the shell's whole visible state, read in one pass so a failure
// names every signal rather than only the first.
type kbView struct {
	Path          string   `json:"path"`
	DocPath       string   `json:"docPath"`
	Busy          string   `json:"busy"`
	Heading       string   `json:"heading"`
	Crumbs        []string `json:"crumbs"`
	CrumbsFit     bool     `json:"crumbsFit"`
	Ellipsis      bool     `json:"ellipsis"`
	Expanded      string   `json:"expanded"`
	TreeReady     string   `json:"treeReady"`
	TreeFiles     string   `json:"treeFiles"`
	Here          string   `json:"here"`
	HasParent     bool     `json:"hasParent"`
	Parent        string   `json:"parent"`
	FilePaths     []string `json:"filePaths"`
	DirPaths      []string `json:"dirPaths"`
	Current       []string `json:"current"`
	TOCHidden     bool     `json:"tocHidden"`
	TOCLinks      []string `json:"tocLinks"`
	TOCActive     []string `json:"tocActive"`
	Marker        string   `json:"marker"`
	HistoryLength int      `json:"historyLength"`
	PageFits      bool     `json:"pageFits"`
	DocLinks      []string `json:"docLinks"`
}

const readKBView = `(() => {
  const doc = document.getElementById("kb-doc");
  const tree = document.getElementById("kb-tree");
  const crumbs = document.getElementById("kb-crumbs");
  const here = tree.querySelector(".kb-tree-here");
  const up = tree.querySelector("button.kb-up");
  const more = crumbs.querySelector(".kb-crumb-more");
  const text = (el) => (el ? el.textContent.trim() : "");
  const all = (sel) => Array.from(document.querySelectorAll(sel));
  return {
    path: location.pathname + location.hash,
    docPath: doc.dataset.path || "",
    busy: doc.getAttribute("aria-busy") || "",
    heading: text(doc.querySelector("h1")),
    crumbs: all("#kb-crumbs .kb-crumb").filter((c) => !c.hidden)
      .map((c) => c.dataset.crumbKind + ":" + text(c)),
    crumbsFit: crumbs.scrollWidth <= crumbs.clientWidth,
    ellipsis: !!more,
    expanded: more ? more.getAttribute("aria-expanded") : "",
    treeReady: tree.dataset.treeReady || "",
    treeFiles: tree.dataset.files || "",
    here: here ? here.dataset.dir : "",
    hasParent: !!up,
    parent: up ? up.dataset.parent : "",
    filePaths: all("#kb-tree a.kb-file").map((a) => a.dataset.path).sort(),
    dirPaths: all("#kb-tree button.kb-dir").map((b) => b.dataset.dir).sort(),
    current: all("#kb-tree a.kb-file.current").map((a) => a.dataset.path + ":" + a.getAttribute("aria-current")),
    tocHidden: document.getElementById("kb-toc").hidden,
    tocLinks: all("#kb-toc-list a.kb-toc-link").map((a) => a.dataset.targetId),
    tocActive: all("#kb-toc-list a.kb-toc-link[aria-current='true']").map((a) => a.dataset.targetId),
    marker: window.__kbMarker || "",
    historyLength: history.length,
    pageFits: document.documentElement.scrollWidth <= window.innerWidth,
    // One line per link in the document: where it points, whether the shell
    // marked it as leaving the base, and how it opens.
    docLinks: all("#kb-doc a").map((a) => [
      a.getAttribute("href") || "",
      a.dataset.external || "",
      a.target || "",
      a.rel || "",
      a.querySelector(".kb-ico-ext") ? "mark" : "",
    ].join("|")),
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

// atFolder is the selector for the sidebar showing one folder's level.
func atFolder(dir string) string {
	return `#kb-tree .kb-tree-here[data-dir="` + dir + `"]`
}

// drill walks the sidebar down one folder at a time, which is the only way it
// reaches a nested folder: the sidebar shows one level, never a whole tree.
func drill(dirs []string) []chromedp.Action {
	acts := make([]chromedp.Action, 0, len(dirs)*2)
	for i := range dirs {
		path := strings.Join(dirs[:i+1], "/")
		acts = append(acts,
			clickSel(`#kb-tree button.kb-dir[data-dir="`+path+`"]`),
			chromedp.WaitVisible(atFolder(path), chromedp.ByQuery),
		)
	}
	return acts
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
// root's README, navigates by relative link, by sidebar and by direct URL, and
// resolves a heading fragment on load and on hashchange.
func TestKnowledgeBase(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base")
	br.Open(t, paste.URL)

	// -- the root renders README.md, the sidebar shows the root level ---------
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
	// The deploy prints the base with no trailing slash in path mode, which
	// would resolve a relative link one level ABOVE the base.
	if !strings.HasSuffix(root.Path, "/p/"+paste.Slug+"/") {
		t.Errorf("root URL = %q, want it normalised to end in /p/%s/", root.Path, paste.Slug)
	}
	// The sidebar shows ONE level: the root's own children, never the whole
	// tree, so nothing below the top level is listed yet.
	if want := []string{"README.md"}; !slices.Equal(root.FilePaths, want) {
		t.Errorf("sidebar files at the root = %q, want %q", root.FilePaths, want)
	}
	if want := []string{"assets", "guides", "notes", "reference"}; !slices.Equal(root.DirPaths, want) {
		t.Errorf("sidebar folders at the root = %q, want the top level %q", root.DirPaths, want)
	}
	if root.Here != "" || root.HasParent {
		t.Errorf("the root level names folder %q and offers a parent (%v); the root has none", root.Here, root.HasParent)
	}
	if root.TreeFiles != "6" {
		t.Errorf("tree data-files = %q, want 6 (the whole base's count)", root.TreeFiles)
	}
	if root.Ellipsis {
		t.Errorf("a two-segment trail collapsed: %q", root.Crumbs)
	}
	if want := []string{"root:root", "file:README.md"}; !slices.Equal(root.Crumbs, want) {
		t.Errorf("root crumbs = %q, want %q", root.Crumbs, want)
	}
	if want := []string{"knowledge-base-root", "overview", "what-is-inside"}; !slices.Equal(root.TOCLinks, want) {
		t.Errorf("root TOC = %q, want the README's headings %q", root.TOCLinks, want)
	}

	// -- a relative link in the ROOT document opens in the shell --------------
	// The case a missing trailing slash breaks: the href is relative to the
	// base, so it only resolves inside it when the URL names the base itself.
	var rel kbView
	flow.settle("relative-from-root", "kb-doc",
		// The marker dies with the document, so its survival is what separates
		// a pushState swap from a reload that happens to land on the same URL.
		chromedp.Evaluate(`window.__kbMarker = "alive"`, nil),
		clickSel(`#kb-doc a[href="guides/setup.md"]`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &rel),
	)
	flow.Shot("document")

	if rel.Marker != "alive" {
		t.Errorf("a relative link from the root left the shell: marker = %q, want %q", rel.Marker, "alive")
	}
	if rel.HistoryLength <= root.HistoryLength {
		t.Errorf("history length %d did not grow from %d, so the navigation did not pushState",
			rel.HistoryLength, root.HistoryLength)
	}
	if !strings.HasSuffix(rel.Path, "/guides/setup.md") {
		t.Errorf("URL = %q, want it to end in /guides/setup.md", rel.Path)
	}
	if rel.Heading != "Setup" {
		t.Errorf("h1 = %q, want %q", rel.Heading, "Setup")
	}
	// The sidebar follows the reader into the opened document's folder.
	if rel.Here != "guides" {
		t.Errorf("sidebar folder = %q, want guides, the opened document's own", rel.Here)
	}
	if w := []string{"guides/setup.md:page"}; !slices.Equal(rel.Current, w) {
		t.Errorf("sidebar current entry = %q, want %q", rel.Current, w)
	}
	want := []string{"root:root", "dir:guides", "file:setup.md"}
	if !slices.Equal(rel.Crumbs, want) {
		t.Errorf("crumbs = %q, want %q", rel.Crumbs, want)
	}
	wantTOC := []string{"setup", "install-the-daemon", "configure-the-listener", "verify-the-install"}
	if !slices.Equal(rel.TOCLinks, wantTOC) {
		t.Errorf("TOC = %q, want this document's headings %q", rel.TOCLinks, wantTOC)
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
	if down.Here != "guides/advanced" {
		t.Errorf("sidebar folder = %q, want guides/advanced", down.Here)
	}
	flow.settle("relative-up", "kb-doc",
		clickSel(`#kb-doc a[href="../setup.md"]`),
		chromedp.WaitVisible(settled("guides/setup.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &up),
	)
	if up.Heading != "Setup" {
		t.Errorf("relative link out of a subdirectory opened %q, want Setup", up.Heading)
	}

	// -- the sidebar navigates folders without touching the document ----------
	var upOne, atRoot, inNotes kbView
	flow.settle("sidebar-up", "kb-tree",
		clickSel(`#kb-tree button.kb-up[data-parent=""]`),
		chromedp.WaitVisible(atFolder(""), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &upOne),
	)
	if upOne.Here != "" || upOne.HasParent {
		t.Errorf("the parent control did not land on the root: here %q parent %v", upOne.Here, upOne.HasParent)
	}
	if upOne.DocPath != "guides/setup.md" || upOne.Marker != "alive" {
		t.Errorf("moving the sidebar changed the document: %q (marker %q)", upOne.DocPath, upOne.Marker)
	}
	atRoot = upOne
	if want := []string{"assets", "guides", "notes", "reference"}; !slices.Equal(atRoot.DirPaths, want) {
		t.Errorf("sidebar folders back at the root = %q, want %q", atRoot.DirPaths, want)
	}

	flow.settle("sidebar-folder", "kb-tree",
		clickSel(`#kb-tree button.kb-dir[data-dir="notes"]`),
		chromedp.WaitVisible(atFolder("notes"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &inNotes),
	)
	if inNotes.Here != "notes" || inNotes.Parent != "" {
		t.Errorf("entering notes/ = here %q parent %q, want notes and the root", inNotes.Here, inNotes.Parent)
	}
	if want := []string{"notes/page.html"}; !slices.Equal(inNotes.FilePaths, want) {
		t.Errorf("notes/ lists %q, want %q", inNotes.FilePaths, want)
	}
	if inNotes.DocPath != "guides/setup.md" {
		t.Errorf("entering a folder changed the document to %q", inNotes.DocPath)
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
	if ext.Count != 1 {
		t.Errorf("%d entries in notes/ marked external, want 1 (the html file)", ext.Count)
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
	if direct.Heading != rel.Heading || !slices.Equal(direct.Crumbs, rel.Crumbs) ||
		!slices.Equal(direct.TOCLinks, rel.TOCLinks) || !slices.Equal(direct.Current, rel.Current) ||
		direct.Here != rel.Here {
		t.Errorf("a typed URL renders differently from clicking to it:\n direct %+v\n clicked %+v", direct, rel)
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
	if idle.NoteText != "" {
		t.Errorf("the hidden note still carries text: %q", idle.NoteText)
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

	// -- a term matches whatever its case ------------------------------------
	// The index keeps one copy of each document, so matching is the pattern's
	// job rather than a lowercased twin's.
	var upper kbSearch
	flow.settle("search-case", "kb-hits",
		clearSearch,
		chromedp.SendKeys("#kb-search", "QUOKKA", chromedp.ByQuery),
		chromedp.WaitVisible("#kb-results .kb-hit", chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &upper),
	)
	if len(upper.Hits) != 1 || upper.Hits[0].Path != "guides/setup.md" {
		t.Errorf("an upper-case query returned %+v, want the same one text hit", upper.Hits)
	}

	// -- the hit opens the file it named --------------------------------------
	var opened kbView
	flow.settle("search-open", "kb-doc",
		clearSearch,
		chromedp.SendKeys("#kb-search", "quokka", chromedp.ByQuery),
		chromedp.WaitVisible("#kb-hits .kb-hit", chromedp.ByQuery),
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
	// A hit reaches any file at any depth; the sidebar follows it there, which
	// is what makes navigating one level at a time enough.
	if opened.Here != "guides" {
		t.Errorf("sidebar folder after opening a hit = %q, want guides", opened.Here)
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

// blockRawFetch fails ONE document's raw fetch and nothing else, so the index
// falls short for a reason that is not the size bound.
const blockRawFetch = `(() => {
  const orig = window.fetch;
  window.fetch = function (input) {
    const url = typeof input === "string" ? input : (input && input.url) || "";
    if (url.indexOf("guides/setup.md?raw=1") >= 0) {
      return Promise.reject(new Error("blocked by the e2e fixture"));
    }
    return orig.apply(this, arguments);
  };
})()`

// A partial index names what actually stopped it. A document that could not be
// read is not the size bound, and this base is nowhere near either limit.
func TestKnowledgeBasePartialIndex(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base partial index"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-index")
	if err := chromedp.Run(br.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(blockRawFetch).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("install the fetch block: %v", err)
	}
	br.Open(t, paste.URL)

	var got kbSearch
	flow.settle("partial", "kb-index-note",
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.WaitReady(`body[data-kb-index="partial"]`, chromedp.ByQuery),
		// The note lives in the results panel, which a query opens.
		chromedp.SendKeys("#kb-search", "setup", chromedp.ByQuery),
		chromedp.WaitVisible("#kb-index-note", chromedp.ByQuery),
		chromedp.Evaluate(readKBSearch, &got),
	)
	flow.Shot("partial-index")

	if got.Indexed != "3" || got.Total != "4" {
		t.Errorf("index covers %s of %s documents, want 3 of 4 with one unreadable", got.Indexed, got.Total)
	}
	if !strings.Contains(got.NoteText, "1 document(s) could not be read") {
		t.Errorf("the note does not name the failed read: %q", got.NoteText)
	}
	if strings.Contains(got.NoteText, "MiB of markdown") {
		t.Errorf("the note blames the size bound for a failed read: %q", got.NoteText)
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

// Content wider than the reading pane is CONTAINED by it: the table and the
// code block scroll inside their own boxes and the page never scrolls sideways.
func TestKnowledgeBaseWideContent(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, wideContentFiles, UploadOpts{Name: "knowledge base wide content"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-wide")
	br.Open(t, paste.URL)

	type wide struct {
		PageWidth   float64 `json:"pageWidth"`
		InnerWidth  float64 `json:"innerWidth"`
		TableScroll bool    `json:"tableScroll"`
		PreScroll   bool    `json:"preScroll"`
	}
	const readWide = `(() => {
	  const t = document.querySelector("#kb-doc table");
	  const p = document.querySelector("#kb-doc pre");
	  return {
	    pageWidth: document.documentElement.scrollWidth,
	    innerWidth: window.innerWidth,
	    tableScroll: !!t && t.scrollWidth > t.clientWidth,
	    preScroll: !!p && p.scrollWidth > p.clientWidth,
	  };
	})()`

	var desktop wide
	flow.settle("wide-desktop", "kb-doc",
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.WaitVisible("#kb-doc table", chromedp.ByQuery),
		chromedp.Evaluate(readWide, &desktop),
	)
	flow.Shot("wide-desktop")
	if desktop.PageWidth > desktop.InnerWidth {
		t.Errorf("the page scrolls sideways at %.0fpx: scrollWidth %.0f", desktop.InnerWidth, desktop.PageWidth)
	}
	if !desktop.TableScroll || !desktop.PreScroll {
		t.Errorf("wide content is not scrolling within its own box: %+v", desktop)
	}

	var phone wide
	if err := chromedp.Run(br.Ctx, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatalf("phone viewport: %v", err)
	}
	flow.settle("wide-phone", "kb-doc",
		chromedp.Poll(`window.innerWidth === 390`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readWide, &phone),
	)
	flow.Shot("wide-phone")
	if phone.PageWidth > phone.InnerWidth {
		t.Errorf("the page scrolls sideways at 390px: scrollWidth %.0f, innerWidth %.0f",
			phone.PageWidth, phone.InnerWidth)
	}
	if !phone.TableScroll || !phone.PreScroll {
		t.Errorf("wide content is not scrolling within its own box on a phone: %+v", phone)
	}

	br.AssertNoPageErrors(t)
}

// Ten folders deep, the sidebar still shows one level and the breadcrumb trail
// still fits: the walk down, the file at the bottom, and the walk back up by
// the parent control and by a crumb.
func TestKnowledgeBaseDeepNavigation(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, deepBaseFiles, UploadOpts{Name: "knowledge base deep"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-deep")
	br.Open(t, paste.URL)

	var root kbView
	flow.settle("deep-root", "kb-tree",
		chromedp.WaitVisible(`#kb-tree[data-tree-ready="1"]`, chromedp.ByQuery),
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &root),
	)
	flow.Shot("sidebar-root")
	if want := []string{deepNames[0]}; !slices.Equal(root.DirPaths, want) {
		t.Errorf("the root level lists %q, want only the top folder %q", root.DirPaths, want)
	}
	if root.Ellipsis {
		t.Errorf("the root document's two-segment trail collapsed: %q", root.Crumbs)
	}

	var deep kbView
	flow.settle("deep-walk", "kb-tree",
		chromedp.Tasks(drill(deepNames)),
		chromedp.Evaluate(readKBView, &deep),
	)
	if deep.Here != deepDir {
		t.Fatalf("after drilling ten levels the sidebar is at %q, want %q", deep.Here, deepDir)
	}
	if deep.Parent != strings.Join(deepNames[:len(deepNames)-1], "/") {
		t.Errorf("the parent control points at %q, want the ninth level", deep.Parent)
	}
	if want := []string{deepDir + "/level.md", deepDir + "/notes.md"}; !slices.Equal(deep.FilePaths, want) {
		t.Errorf("the deepest level lists %q, want %q", deep.FilePaths, want)
	}
	if len(deep.DirPaths) != 0 {
		t.Errorf("the deepest level lists folders %q, want none", deep.DirPaths)
	}
	// Ten levels of sidebar navigation, and the document never moved.
	if deep.DocPath != "README.md" {
		t.Errorf("walking the sidebar changed the document to %q", deep.DocPath)
	}

	var opened kbView
	flow.settle("deep-open", "kb-doc",
		clickSel(`#kb-tree a.kb-file[data-path="`+deepFile+`"]`),
		chromedp.WaitVisible(settled(deepFile), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &opened),
	)
	flow.Shot("sidebar-depth-10")
	if opened.Heading != "Level 10" {
		t.Errorf("the deepest document rendered %q, want Level 10", opened.Heading)
	}
	if w := []string{deepFile + ":page"}; !slices.Equal(opened.Current, w) {
		t.Errorf("the current file is not marked at depth ten: %q", opened.Current)
	}
	// The trail is one line: the middle gave way, the ends did not.
	if !opened.CrumbsFit {
		t.Errorf("the breadcrumb trail overflows its bar: %q", opened.Crumbs)
	}
	if !opened.Ellipsis {
		t.Errorf("a ten-segment trail did not collapse: %q", opened.Crumbs)
	}
	if len(opened.Crumbs) == 0 || opened.Crumbs[0] != "root:root" {
		t.Errorf("the trail lost its first segment: %q", opened.Crumbs)
	}
	if last := opened.Crumbs[len(opened.Crumbs)-1]; last != "file:level.md" {
		t.Errorf("the trail's last segment = %q, want the current file", last)
	}

	// -- the ellipsis expands the whole path, and collapses again -------------
	var expanded kbView
	flow.settle("crumbs-expanded", "kb-crumbs",
		clickSel(`#kb-crumbs .kb-crumb-more`),
		chromedp.WaitVisible(`#kb-crumbs.expanded`, chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &expanded),
	)
	flow.Shot("crumbs-expanded")
	// root + ten folders + the file + the control itself.
	if len(expanded.Crumbs) != len(deepNames)+3 {
		t.Errorf("expanded trail shows %d crumbs (%q), want every segment", len(expanded.Crumbs), expanded.Crumbs)
	}
	if expanded.Expanded != "true" {
		t.Errorf("the control's aria-expanded = %q, want true", expanded.Expanded)
	}
	if !expanded.PageFits {
		t.Error("the expanded trail made the page scroll sideways rather than wrap")
	}

	var recollapsed kbView
	flow.settle("crumbs-recollapsed", "kb-crumbs",
		clickSel(`#kb-crumbs .kb-crumb-more`),
		chromedp.Evaluate(readKBView, &recollapsed),
	)
	if recollapsed.Expanded != "false" || !recollapsed.CrumbsFit {
		t.Errorf("a second click did not collapse the trail: expanded %q fits %v (%q)",
			recollapsed.Expanded, recollapsed.CrumbsFit, recollapsed.Crumbs)
	}

	// Opening another document collapses an expanded trail.
	var switched kbView
	flow.settle("crumbs-after-open", "kb-doc",
		clickSel(`#kb-crumbs .kb-crumb-more`),
		chromedp.WaitVisible(`#kb-crumbs.expanded`, chromedp.ByQuery),
		clickSel(`#kb-tree a.kb-file[data-path="`+deepDir+`/notes.md"]`),
		chromedp.WaitVisible(settled(deepDir+"/notes.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &switched),
	)
	if switched.Expanded != "false" || !switched.CrumbsFit {
		t.Errorf("the trail stayed expanded across a document change: expanded %q fits %v",
			switched.Expanded, switched.CrumbsFit)
	}

	// -- back up, by the parent control and by a crumb ------------------------
	var upOne kbView
	ninth := strings.Join(deepNames[:len(deepNames)-1], "/")
	flow.settle("deep-up", "kb-tree",
		clickSel(`#kb-tree button.kb-up[data-parent="`+ninth+`"]`),
		chromedp.WaitVisible(atFolder(ninth), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &upOne),
	)
	if upOne.Here != ninth {
		t.Errorf("the parent control landed at %q, want %q", upOne.Here, ninth)
	}
	if upOne.DocPath != deepDir+"/notes.md" {
		t.Errorf("going up changed the document to %q", upOne.DocPath)
	}

	var byCrumb kbView
	flow.settle("deep-crumb", "kb-tree",
		clickSel(`#kb-crumbs button.kb-crumb[data-crumb-kind="dir"]`),
		chromedp.WaitVisible(atFolder(deepNames[0]), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &byCrumb),
	)
	if byCrumb.Here != deepNames[0] {
		t.Errorf("the first folder crumb moved the sidebar to %q, want %q", byCrumb.Here, deepNames[0])
	}
	if byCrumb.DocPath != deepDir+"/notes.md" {
		t.Errorf("a crumb click changed the document to %q", byCrumb.DocPath)
	}

	br.AssertNoPageErrors(t)
}

// The same depth on a phone: the drawer, the walk down, and a trail that
// collapses to one line and expands without widening the page.
func TestKnowledgeBaseDeepNavigationOnAPhone(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, deepBaseFiles, UploadOpts{Name: "knowledge base deep phone"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-deep-mobile")
	if err := chromedp.Run(br.Ctx, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatalf("phone viewport: %v", err)
	}
	br.Open(t, paste.URL)

	var deep kbView
	flow.settle("phone-walk", "kb-tree",
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.Click("#kb-files-toggle", chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("kb-tree").getBoundingClientRect().left >= 0`, nil,
			chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Tasks(drill(deepNames)),
		chromedp.Evaluate(readKBView, &deep),
	)
	flow.Shot("sidebar-phone")
	if deep.Here != deepDir {
		t.Fatalf("the phone sidebar is at %q after ten levels, want %q", deep.Here, deepDir)
	}
	// A folder is sidebar navigation, so the drawer stays open for the next tap.
	if !deep.PageFits {
		t.Error("walking the sidebar on a phone made the page scroll sideways")
	}

	var opened kbView
	flow.settle("phone-open", "kb-doc",
		clickSel(`#kb-tree a.kb-file[data-path="`+deepFile+`"]`),
		chromedp.WaitVisible(settled(deepFile), chromedp.ByQuery),
		// The drawer slides shut behind the opened file; reading or
		// photographing mid-slide captures a half-open sidebar.
		chromedp.Poll(`document.getElementById("kb-tree").getBoundingClientRect().right <= 0`, nil,
			chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(readKBView, &opened),
	)
	flow.Shot("phone-crumbs-collapsed")
	if !opened.PageFits {
		t.Errorf("the page scrolls sideways at 390px with a ten-deep document open: %q", opened.Crumbs)
	}
	if !opened.CrumbsFit || !opened.Ellipsis {
		t.Errorf("the trail does not fit one phone-width line: fits %v ellipsis %v (%q)",
			opened.CrumbsFit, opened.Ellipsis, opened.Crumbs)
	}
	if opened.Crumbs[0] != "root:root" || opened.Crumbs[len(opened.Crumbs)-1] != "file:level.md" {
		t.Errorf("the collapsed trail lost an end: %q", opened.Crumbs)
	}

	var expanded kbView
	flow.settle("phone-crumbs", "kb-crumbs",
		clickSel(`#kb-crumbs .kb-crumb-more`),
		chromedp.WaitVisible(`#kb-crumbs.expanded`, chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &expanded),
	)
	flow.Shot("phone-crumbs-expanded")
	if len(expanded.Crumbs) != len(deepNames)+3 {
		t.Errorf("the expanded trail shows %d crumbs (%q), want every segment", len(expanded.Crumbs), expanded.Crumbs)
	}
	if !expanded.PageFits {
		t.Error("the expanded trail widened the page instead of wrapping")
	}

	br.AssertNoPageErrors(t)
}

// On a phone both sidebars are drawers over the document: closed until asked
// for, closed again by opening a file, and never adding scrollable width.
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
	// An off-canvas drawer is parked outside the page, not beside it: unclipped
	// it would add its own width to the page and scroll the phone sideways.
	if closed.PageWidth > closed.Width {
		t.Errorf("the page is %.0fpx wide in a %.0fpx viewport with both drawers closed", closed.PageWidth, closed.Width)
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
	if open.PageWidth > open.Width {
		t.Errorf("the open files drawer widened the page to %.0fpx in a %.0fpx viewport", open.PageWidth, open.Width)
	}

	// The sidebar walks folders on a phone the same way, and the drawer stays
	// open across it: a folder is navigation within the drawer, not a choice of
	// document that closes it.
	var drilled kbPanes
	flow.settle("phone-folder", "kb-tree",
		clickSel(`#kb-tree button.kb-dir[data-dir="guides"]`),
		chromedp.WaitVisible(atFolder("guides"), chromedp.ByQuery),
		chromedp.Evaluate(readKBPanes, &drilled),
	)
	if !strings.Contains(drilled.BodyClasses, "tree-open") {
		t.Errorf("entering a folder closed the drawer: %+v", drilled)
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
	if tocOpen.PageWidth > tocOpen.Width {
		t.Errorf("the open contents drawer widened the page to %.0fpx in a %.0fpx viewport", tocOpen.PageWidth, tocOpen.Width)
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
	PageWidth    float64 `json:"pageWidth"`
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
    pageWidth: document.documentElement.scrollWidth,
  };
})()`

// kbLinkColors compares a link the sanitizer disarmed against a live one and
// against the document's own text, which is what "renders as plain text" means.
type kbLinkColors struct {
	Doc  string `json:"doc"`
	Dead string `json:"dead"`
	Live string `json:"live"`
}

const readKBLinkColors = `(() => {
  const doc = document.getElementById("kb-doc");
  const links = Array.from(doc.querySelectorAll("a"));
  const dead = links.find((a) => !a.hasAttribute("href"));
  const live = links.find((a) => a.getAttribute("href") === "guides/setup.md");
  return {
    doc: getComputedStyle(doc).color,
    dead: dead ? getComputedStyle(dead).color : "",
    live: live ? getComputedStyle(live).color : "",
  };
})()`

// A document's links divide into those that stay in this base and those that
// leave it. The shell must say which before the click: what leaves is marked
// and opens in a new tab, what stays opens in place, and what the sanitizer
// disarmed stops looking like a link at all.
func TestKnowledgeBaseLinks(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.UploadDir(t, knowledgeBaseFiles, UploadOpts{Name: "knowledge base links"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "knowledge-base-links")
	br.Open(t, paste.URL)

	var view kbView
	var colors kbLinkColors
	flow.settle("root", "kb-doc",
		chromedp.WaitVisible(`#kb-tree[data-tree-ready="1"]`, chromedp.ByQuery),
		chromedp.WaitVisible(settled("README.md"), chromedp.ByQuery),
		chromedp.Evaluate(readKBView, &view),
		chromedp.Evaluate(readKBLinkColors, &colors),
	)
	flow.Shot("links")

	// href | data-external | target | rel | external mark, in document order.
	want := []string{
		"guides/setup.md||||",
		"reference/api.md||||",
		"https://example.com/docs|1|_blank|noopener noreferrer|mark",
		"notes/page.html|1|_blank|noopener noreferrer|mark",
		"mailto:nobody@example.com|1|||mark",
		"||||",
	}
	if !slices.Equal(view.DocLinks, want) {
		t.Errorf("document links =\n  %q\nwant\n  %q", view.DocLinks, want)
	}

	if colors.Dead != colors.Doc {
		t.Errorf("a disarmed link renders in %q, want the document's own %q", colors.Dead, colors.Doc)
	}
	if colors.Dead == colors.Live {
		t.Errorf("a disarmed link renders in %q, the same colour as a live link", colors.Dead)
	}

	// A link that leaves the base keeps the shell out of it: the click handler
	// skips anything marked external, so the page it is on does not change.
	var after kbView
	flow.settle("external-click-does-not-navigate", "kb-doc",
		chromedp.Evaluate(`window.__kbMarker = "alive"`, nil),
		// target="_blank" would open a tab this session does not drive, so the
		// anchor is neutralised first: what is under test is the shell's own
		// handler declining it, not the browser's tab handling.
		chromedp.Evaluate(`(() => {
		  const a = document.querySelector('#kb-doc a[href="https://example.com/docs"]');
		  a.removeAttribute("target");
		  a.addEventListener("click", (ev) => ev.preventDefault(), { once: true });
		  a.click();
		})()`, nil),
		chromedp.Evaluate(readKBView, &after),
	)
	if after.DocPath != "README.md" || after.Marker != "alive" {
		t.Errorf("clicking a link out of the base moved the shell to %q (marker %q), want it to stay on README.md",
			after.DocPath, after.Marker)
	}

	// The raw page the document links to is the same bytes the server serves at
	// its own URL, which is why linking out rather than rendering is correct.
	body, ctype := get(t, paste.URL+"/notes/page.html")
	if !strings.Contains(body, "served raw") {
		t.Errorf("linked HTML file served %q, want the file's own bytes", body)
	}
	if !strings.HasPrefix(ctype, "text/html") {
		t.Errorf("linked HTML file content-type = %q, want text/html", ctype)
	}
}

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
