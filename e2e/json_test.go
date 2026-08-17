//go:build e2e

package e2e

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// jsonFixture reuses the csv fixture's trick: the scores order differently
// under numeric and lexicographic comparison, so a console that read score as
// text would return a different permutation from ORDER BY, not the same one.
const jsonFixture = `[
  {"name": "delta", "score": 42, "region": "west"},
  {"name": "alpha", "score": 7, "region": "east"},
  {"name": "charlie", "score": 113, "region": "north"},
  {"name": "bravo", "score": 29, "region": "south"},
  {"name": "echo", "score": 88, "region": "east"}
]`

// jsonTree is the rendered tree's shape, read in one pass so a failure names
// every mismatch rather than only the first.
type jsonTree struct {
	Root    string     `json:"root"`
	Items   []string   `json:"items"`
	Keys    [][]string `json:"keys"`
	Numbers []string   `json:"numbers"`
	Strings []string   `json:"strings"`
	Summary string     `json:"summary"`
}

// readJSONTree walks direct children at each level, so a nested value cannot
// stand in for a top-level one.
const readJSONTree = `(() => {
  const root = document.querySelector("#content .tree > details");
  if (!root) return null;
  const items = [...root.querySelectorAll(":scope > details")];
  const text = (el) => el.textContent;
  return {
    root: text(root.querySelector(":scope > summary span")),
    items: items.map((d) => text(d.querySelector(":scope > summary span"))),
    keys: items.map((d) => [...d.querySelectorAll(":scope > div span.k")].map(text)),
    numbers: [...root.querySelectorAll("span.num")].map(text),
    strings: [...root.querySelectorAll("span.s")].map(text),
    summary: document.getElementById("summary").textContent,
  };
})()`

// filterHitJS is the viewer's own report that the filter matched exactly the
// one row holding charlie's name.
const filterHitJS = `document.getElementById("summary").textContent === "5 items · 1 matches"`

// sortQuery is typed at the editor's initial cursor (position 0), so the
// trailing comment marker disarms the default query it lands in front of.
const sortQuery = `SELECT name, score FROM data ORDER BY score; --`

// jsonSQLResult is the DuckDB result table, read in one pass.
type jsonSQLResult struct {
	Err          string   `json:"err"`
	Cols         []string `json:"cols"`
	Names        []string `json:"names"`
	Scores       []string `json:"scores"`
	ScoreClasses []string `json:"scoreClasses"`
	Tab          string   `json:"tab"`
}

// readJSONSQLResult can read the result table unambiguously because the json
// data view is a tree: any table under #content is the console's.
const readJSONSQLResult = `(() => {
  const err = document.querySelector("#content .err");
  if (err) return { err: err.textContent };
  const rows = [...document.querySelectorAll("#content table tbody tr")];
  return {
    err: "",
    cols: [...document.querySelectorAll("#content table thead th")].map((e) => e.textContent),
    names: rows.map((r) => r.cells[0].textContent),
    scores: rows.map((r) => r.cells[1].textContent),
    scoreClasses: rows.map((r) => r.cells[1].className),
    tab: document.querySelector('#views [data-view="result"]').textContent,
  };
})()`

// The json viewer renders a collapsible tree with type-classed leaves and a
// live filter, and its SQL console loads the paste through read_json_auto,
// where a numeric ORDER BY proves score was typed as a number rather than text.
func TestJSONRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(jsonFixture), UploadOpts{Type: "json", Name: "json proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "json-render")
	br.Open(t, paste.URL)

	// Both outcomes end the shell's work: renderTree is the only producer of
	// .tree and the boot failure path is the only producer of .err, while the
	// unrendered page holds a lone div.status. Scoped under the browser cap so
	// a stuck render leaves a live context to screenshot from.
	var tree *jsonTree
	var shellErr string
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible(`#content .tree, #content .err`, chromedp.ByQuery),
		chromedp.Evaluate(`(document.querySelector("#content .err") || {}).textContent || ""`, &shellErr),
		chromedp.Evaluate(readJSONTree, &tree),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("shell settled on neither a tree nor an error: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("tree")

	if shellErr != "" {
		t.Fatalf("shell rendered its failure message: %s", shellErr)
	}
	if tree == nil {
		t.Fatal("no root details under #content .tree: the json renderer did not run")
	}
	if tree.Summary != "5 items" {
		t.Errorf("summary = %q, want %q", tree.Summary, "5 items")
	}
	if tree.Root != "[5]" {
		t.Errorf("root label = %q, want %q", tree.Root, "[5]")
	}
	if want := []string{"{3}", "{3}", "{3}", "{3}", "{3}"}; !slices.Equal(tree.Items, want) {
		t.Errorf("item labels = %q, want %q", tree.Items, want)
	}
	keyRow := []string{`"name": `, `"score": `, `"region": `}
	if want := [][]string{keyRow, keyRow, keyRow, keyRow, keyRow}; !reflect.DeepEqual(tree.Keys, want) {
		t.Errorf("item keys = %q, want %q", tree.Keys, want)
	}
	// Leaves are classed by the value's runtime type, so five unquoted span.num
	// scores exist only if the document's numbers survived parsing as numbers;
	// a string score would render quoted under span.s instead.
	if want := []string{"42", "7", "113", "29", "88"}; !slices.Equal(tree.Numbers, want) {
		t.Errorf("number leaves = %q, want %q", tree.Numbers, want)
	}
	wantStrings := []string{
		`"delta"`, `"west"`, `"alpha"`, `"east"`, `"charlie"`,
		`"north"`, `"bravo"`, `"south"`, `"echo"`, `"east"`,
	}
	if !slices.Equal(tree.Strings, wantStrings) {
		t.Errorf("string leaves = %q, want %q", tree.Strings, wantStrings)
	}

	// The filter rebuilds the tree per keystroke, marks the match, and reports
	// the hit count, so a viewer that painted once and died fails here.
	var marks []string
	filterCtx, cancelFilter := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelFilter()
	if err := chromedp.Run(filterCtx,
		chromedp.SendKeys("#filter", "charlie", chromedp.ByQuery),
		chromedp.Poll(filterHitJS, nil),
		chromedp.Evaluate(`[...document.querySelectorAll("#content mark")].map((e) => e.textContent)`, &marks),
	); err != nil {
		flow.Shot("stuck-filter")
		t.Fatalf("filter never reported its match: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("filter-charlie")
	if want := []string{"charlie"}; !slices.Equal(marks, want) {
		t.Errorf("filter marks = %q, want %q", marks, want)
	}

	// The console's editor mounts asynchronously after the click.
	consoleCtx, cancelConsole := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelConsole()
	if err := chromedp.Run(consoleCtx,
		chromedp.Click("#sqlbtn", chromedp.ByQuery),
		chromedp.WaitVisible("#editor .cm-content", chromedp.ByQuery),
		chromedp.SendKeys("#editor .cm-content", sortQuery, chromedp.ByQuery),
		// Escape closes the completion popup so it cannot sit over the Run
		// button when the click lands.
		chromedp.KeyEvent(kb.Escape),
	); err != nil {
		flow.Shot("stuck-console")
		t.Fatalf("open sql console: %v\npage errors: %v", err, br.Errors())
	}

	// The keystrokes went through CodeMirror, so what the editor holds is what
	// Run will execute; checking it first makes a mistyped query fail as one.
	var typed string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`document.querySelector("#editor .cm-content").textContent`, &typed),
	); err != nil {
		t.Fatalf("read editor: %v", err)
	}
	if !strings.HasPrefix(typed, sortQuery) {
		flow.Shot("stuck-typing")
		t.Fatalf("editor holds %q, want prefix %q", typed, sortQuery)
	}
	flow.Shot("sql-console")

	// The first run also fetches and instantiates duckdb, all served locally.
	var res jsonSQLResult
	resultCtx, cancelResult := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelResult()
	if err := chromedp.Run(resultCtx,
		chromedp.Click("#run", chromedp.ByQuery),
		chromedp.WaitVisible(`#content table, #content .err`, chromedp.ByQuery),
		chromedp.Evaluate(readJSONSQLResult, &res),
	); err != nil {
		flow.Shot("stuck-result")
		t.Fatalf("sql run settled on neither a result nor an error: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("result-sorted")

	if res.Err != "" {
		t.Fatalf("query failed: %s", res.Err)
	}
	if want := []string{"name", "score"}; !slices.Equal(res.Cols, want) {
		t.Errorf("result columns = %q, want %q", res.Cols, want)
	}
	// Ascending by value: 7, 29, 42, 88, 113. A text-typed score would order
	// lexicographically as charlie, bravo, delta, alpha, echo, and neither
	// matches the source order, so an unsorted table cannot pass either.
	if want := []string{"alpha", "bravo", "delta", "echo", "charlie"}; !slices.Equal(res.Names, want) {
		t.Errorf("names after ORDER BY score = %q, want %q", res.Names, want)
	}
	if want := []string{"7", "29", "42", "88", "113"}; !slices.Equal(res.Scores, want) {
		t.Errorf("scores after ORDER BY score = %q, want %q", res.Scores, want)
	}
	// A cell is classed "n" only for a number or bigint value, so the engine's
	// own schema typed the column numeric.
	if want := []string{"n", "n", "n", "n", "n"}; !slices.Equal(res.ScoreClasses, want) {
		t.Errorf("score cell classes = %q, want %q", res.ScoreClasses, want)
	}
	if res.Tab != "Result · 5" {
		t.Errorf("result tab = %q, want %q", res.Tab, "Result · 5")
	}

	br.AssertNoPageErrors(t)
}
