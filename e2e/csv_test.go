//go:build e2e

package e2e

import (
	"context"
	"slices"
	"testing"

	"github.com/chromedp/chromedp"
)

// csvFixture is shaped so the sort assertion can fail. Source order is not
// sorted by any column, and the scores order differently under numeric and
// lexicographic comparison, so a viewer that sorted text would produce a
// different permutation rather than the same one.
const csvFixture = `name,score,region
delta,42,west
alpha,7,east
charlie,113,north
bravo,29,south
echo,88,east
`

// scoreHeader addresses the score column. The first header cell is the
// row-number gutter, which carries no sort, so column i is cell i+2.
const scoreHeader = `#content table thead th:nth-child(3)`

// headerNamesJS reads the column names only: a header cell also carries the
// sort arrow, the type and the facts line, so its own text is all four joined.
const headerNamesJS = `Array.from(document.querySelectorAll("#content table thead th .name"), e => e.textContent)`

// rowOrderJS reads the rendered order. Row ids carry the SOURCE row number, so
// they stay stable under a sort and name the permutation the viewer applied.
const rowOrderJS = `Array.from(document.querySelectorAll("#content table tbody tr"), tr => tr.id)`

// sortedIndicatorJS is the header's own report that it sorted ascending.
const sortedIndicatorJS = `document.querySelector("` + scoreHeader + ` .arrow")?.textContent === "▲"`

// textContentJS reads a node's textContent. Rendered text would pin the
// stylesheet too: the column type is displayed through text-transform:
// uppercase, so it reads back as NUMBER.
func textContentJS(sel string) string {
	return `document.querySelector("` + sel + `").textContent`
}

// The csv viewer renders a real table and sorts it on a header click. Numeric
// columns sort by value, and the source order is recoverable from row ids.
func TestCSVRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(csvFixture), UploadOpts{Type: "csv", Name: "csv proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "csv-render")
	br.Open(t, paste.URL)

	var cols []string
	var scoreType, summary string
	var sourceOrder []string
	// Scoped under the browser cap so a stuck render leaves a live context to
	// screenshot from; waiting on br.Ctx would burn the budget and kill it.
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		// The table is built detached and attached in one statement, so a
		// queryable table is a finished one.
		chromedp.WaitVisible(`#content table`, chromedp.ByQuery),
		chromedp.Evaluate(headerNamesJS, &cols),
		chromedp.Evaluate(rowOrderJS, &sourceOrder),
		chromedp.Evaluate(textContentJS(scoreHeader+" .type"), &scoreType),
		chromedp.Evaluate(textContentJS("#summary"), &summary),
	); err != nil {
		t.Fatalf("wait for rendered table: %v", err)
	}
	flow.Shot("table")

	if want := []string{"name", "score", "region"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if want := []string{"row-1", "row-2", "row-3", "row-4", "row-5"}; !slices.Equal(sourceOrder, want) {
		t.Fatalf("source order = %v, want %v", sourceOrder, want)
	}
	// The sort below is numeric only because the column was typed numeric.
	if scoreType != "number" {
		t.Errorf("score column type = %q, want %q", scoreType, "number")
	}
	if want := "5 rows · 3 cols"; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}

	var sortedOrder []string
	if err := chromedp.Run(br.Ctx,
		chromedp.Click(scoreHeader, chromedp.ByQuery),
		// Waiting on the header's own indicator, so a click that never reached
		// the handler fails as a timeout here rather than as an order mismatch.
		chromedp.Poll(sortedIndicatorJS, nil),
		chromedp.Evaluate(rowOrderJS, &sortedOrder),
	); err != nil {
		t.Fatalf("sort by score: %v", err)
	}
	flow.Shot("sorted-by-score")

	if slices.Equal(sortedOrder, sourceOrder) {
		t.Fatalf("order unchanged after clicking the score header: %v", sortedOrder)
	}
	// Ascending by value: 7, 29, 42, 88, 113. Sorting the same column as text
	// would give row-3, row-4, row-1, row-2, row-5.
	if want := []string{"row-2", "row-4", "row-1", "row-5", "row-3"}; !slices.Equal(sortedOrder, want) {
		t.Errorf("sorted order = %v, want %v", sortedOrder, want)
	}

	br.AssertNoPageErrors(t)
}
