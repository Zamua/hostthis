//go:build e2e

package e2e

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// logFixture is 20 NDJSON records with errors at known positions (4, 9, 15,
// 19), so the filter assertion can name the exact rows it must keep rather
// than only a count. Timestamps step evenly by 30s, which lands every record
// in its own histogram bucket and makes the nonzero-bucket count exact.
const logFixture = `{"ts":"2026-08-17T10:00:00Z","level":"info","msg":"server listening on :8080","service":"api"}
{"ts":"2026-08-17T10:00:30Z","level":"warn","msg":"config key retries missing, using default","service":"api"}
{"ts":"2026-08-17T10:01:00Z","level":"info","msg":"GET /healthz 200","service":"api"}
{"ts":"2026-08-17T10:01:30Z","level":"error","msg":"connection reset by peer","service":"api"}
{"ts":"2026-08-17T10:02:00Z","level":"info","msg":"GET /p/abc123 200","service":"api"}
{"ts":"2026-08-17T10:02:30Z","level":"info","msg":"cache miss for slug abc123","service":"worker"}
{"ts":"2026-08-17T10:03:00Z","level":"warn","msg":"slow query took 1200ms","service":"worker"}
{"ts":"2026-08-17T10:03:30Z","level":"info","msg":"sweep pass started","service":"worker"}
{"ts":"2026-08-17T10:04:00Z","level":"error","msg":"upstream timeout after 5s","service":"api"}
{"ts":"2026-08-17T10:04:30Z","level":"info","msg":"sweep pass finished","service":"worker"}
{"ts":"2026-08-17T10:05:00Z","level":"info","msg":"POST /upload 201","service":"api"}
{"ts":"2026-08-17T10:05:30Z","level":"warn","msg":"disk usage at 81 percent","service":"worker"}
{"ts":"2026-08-17T10:06:00Z","level":"info","msg":"GET /p/def456 200","service":"api"}
{"ts":"2026-08-17T10:06:30Z","level":"info","msg":"metadata compaction started","service":"worker"}
{"ts":"2026-08-17T10:07:00Z","level":"error","msg":"write failed: disk full","service":"worker"}
{"ts":"2026-08-17T10:07:30Z","level":"info","msg":"metadata compaction finished","service":"worker"}
{"ts":"2026-08-17T10:08:00Z","level":"warn","msg":"retrying flush, attempt 2","service":"worker"}
{"ts":"2026-08-17T10:08:30Z","level":"info","msg":"GET /p/ghi789 200","service":"api"}
{"ts":"2026-08-17T10:09:00Z","level":"error","msg":"tls handshake failed","service":"api"}
{"ts":"2026-08-17T10:09:30Z","level":"info","msg":"shutdown signal ignored, draining","service":"api"}
`

// logQuery matches the four error records and nothing else.
const logQuery = "level=error"

// logSettled is the shell's own end-of-work mark. The static page ships
// aria-busy="true" and every terminal path (rendered, empty file, failed
// fetch) flips it, so a broken load reports the shell's message instead of a
// timeout.
const logSettled = `#log[aria-busy="false"]`

// logRender is the rendered view's shape. Nonzero counts the histogram bars
// whose height the viewer set above 0%, separately for the grey all-records
// layer and the coloured matching layer.
type logRender struct {
	Recs      int      `json:"recs"`
	Meta      string   `json:"meta"`
	Chips     []string `json:"chips"`
	HistShown bool     `json:"histShown"`
	Buckets   int      `json:"buckets"`
	Nonzero   int      `json:"nonzero"`
	Lit       int      `json:"lit"`
}

const probeLogRender = `(() => {
  const hist = document.getElementById("hist");
  const nz = (sel) => [...hist.querySelectorAll(sel)].filter((e) => parseFloat(e.style.height) > 0).length;
  return {
    recs: document.querySelectorAll("#log .rec").length,
    meta: document.getElementById("meta").textContent,
    chips: [...document.querySelectorAll("#levels .chip")].map((e) => e.textContent),
    histShown: !hist.hidden,
    buckets: hist.querySelectorAll(".hb").length,
    nonzero: nz(".hb-all"),
    lit: nz(".hb-lit"),
  };
})()`

// logFiltered reads the narrowed view. Row ids carry the SOURCE record number,
// so they name which records survived the filter, not just how many.
type logFiltered struct {
	IDs       []string `json:"ids"`
	ErrorRows int      `json:"errorRows"`
	Meta      string   `json:"meta"`
	Lit       int      `json:"lit"`
}

const probeLogFiltered = `(() => ({
  ids: [...document.querySelectorAll("#log .rec")].map((e) => e.id),
  errorRows: document.querySelectorAll("#log .rec.lv-error").length,
  meta: document.getElementById("meta").textContent,
  lit: [...document.querySelectorAll("#hist .hb-lit")].filter((e) => parseFloat(e.style.height) > 0).length,
}))()`

// The waits below ride the meta line as well as the row count: meta is written
// in the same render pass, so matching both means the pass for the FINAL query
// finished, not a transient one for a keystroke prefix.
const (
	logFilteredDone = `document.querySelectorAll("#log .rec").length === 4 &&
		document.getElementById("meta").textContent === "4 of 20"`
	logRestoredDone = `document.querySelectorAll("#log .rec").length === 20 &&
		document.getElementById("meta").textContent === "20 records"`
)

// The log shell parses NDJSON into one row per record with level chips and a
// time histogram, and the query box narrows the view to exactly the matching
// records and restores it when cleared.
func TestLogRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(logFixture), UploadOpts{Type: "log", Name: "log proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "log-render")
	br.Open(t, paste.URL)

	// Scoped under the browser cap so a stuck render leaves a live context to
	// screenshot from; waiting on br.Ctx would burn the budget and kill it.
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible(logSettled, chromedp.ByQuery),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("log shell never settled: %v\n#log: %q\npage errors: %v",
			err, logText(t, br), br.Errors())
	}
	flow.Shot("rendered")

	var got logRender
	if err := chromedp.Run(br.Ctx, chromedp.Evaluate(probeLogRender, &got)); err != nil {
		t.Fatalf("probe rendered page: %v", err)
	}
	if got.Recs != 20 {
		t.Fatalf("rendered %d row(s), want 20\n#log: %q", got.Recs, logText(t, br))
	}
	if want := "20 records"; got.Meta != want {
		t.Errorf("meta = %q, want %q", got.Meta, want)
	}
	// Chip order is the viewer's severity order, and each count is a parse
	// result: a chip totals the records it classified into that level.
	if want := []string{"ERROR 4", "WARN 4", "INFO 12"}; !slices.Equal(got.Chips, want) {
		t.Errorf("level chips = %q, want %q", got.Chips, want)
	}
	if !got.HistShown {
		t.Error("histogram is hidden: no timestamps were parsed")
	}
	if got.Buckets != 60 {
		t.Errorf("histogram has %d bucket(s), want 60", got.Buckets)
	}
	// One bucket per record by the fixture's even spacing, in both layers,
	// since with no query every record matches.
	if got.Nonzero != 20 || got.Lit != 20 {
		t.Errorf("nonzero buckets = %d all / %d lit, want 20 / 20", got.Nonzero, got.Lit)
	}

	filterCtx, cancelFilter := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelFilter()
	if err := chromedp.Run(filterCtx,
		chromedp.SendKeys("#q", logQuery, chromedp.ByQuery),
		chromedp.Poll(logFilteredDone, nil),
	); err != nil {
		flow.Shot("stuck-filter")
		t.Fatalf("query %q never narrowed the view to 4 rows: %v\n#log: %q\npage errors: %v",
			logQuery, err, logText(t, br), br.Errors())
	}
	flow.Shot("filtered")

	var f logFiltered
	if err := chromedp.Run(br.Ctx, chromedp.Evaluate(probeLogFiltered, &f)); err != nil {
		t.Fatalf("probe filtered page: %v", err)
	}
	if want := []string{"L4", "L9", "L15", "L19"}; !slices.Equal(f.IDs, want) {
		t.Errorf("filtered rows = %v, want %v", f.IDs, want)
	}
	if f.ErrorRows != len(f.IDs) {
		t.Errorf("%d of %d filtered rows carry lv-error", f.ErrorRows, len(f.IDs))
	}
	if want := "4 of 20"; f.Meta != want {
		t.Errorf("filtered meta = %q, want %q", f.Meta, want)
	}
	// The coloured layer follows the query while the grey layer keeps every
	// record, so a narrowed view lights exactly the matching buckets.
	if f.Lit != 4 {
		t.Errorf("filtered histogram lights %d bucket(s), want 4", f.Lit)
	}

	clearCtx, cancelClear := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelClear()
	if err := chromedp.Run(clearCtx,
		// Real backspaces rather than a scripted value reset: the shell
		// re-renders on the input events a user produces.
		chromedp.SendKeys("#q", strings.Repeat(kb.Backspace, len(logQuery)), chromedp.ByQuery),
		chromedp.Poll(logRestoredDone, nil),
	); err != nil {
		flow.Shot("stuck-clear")
		t.Fatalf("clearing the query never restored 20 rows: %v\nmeta: %q\npage errors: %v",
			err, metaText(t, br), br.Errors())
	}
	flow.Shot("restored")

	br.AssertNoPageErrors(t)
}

// logText is what the shell left in its mount, for a failure message.
func logText(t *testing.T, br *Browser) string {
	t.Helper()
	return elementText(t, br, "log")
}

func metaText(t *testing.T, br *Browser) string {
	t.Helper()
	return elementText(t, br, "meta")
}

func elementText(t *testing.T, br *Browser, id string) string {
	t.Helper()
	var s string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.getElementById("`+id+`") || {}).textContent || ""`, &s),
	); err != nil {
		return "unreadable: " + err.Error()
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
