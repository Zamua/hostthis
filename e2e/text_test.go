//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// textFixtureLines is what the row-count assertion pins.
const textFixtureLines = 30

// wantLine17 is spelled out rather than generated, so a fixture loop that
// misnumbers its lines cannot misnumber the expectation with it.
const wantLine17 = "line 17 of 30 in the plain text fixture"

func textFixture() []byte {
	var b strings.Builder
	for i := 1; i <= textFixtureLines; i++ {
		fmt.Fprintf(&b, "line %d of 30 in the plain text fixture\n", i)
	}
	return []byte(b.String())
}

// textRender is the rendered shape, read in one pass so a failure names every
// wrong signal rather than only the first.
type textRender struct {
	Rows        int    `json:"rows"`
	BadAnchors  int    `json:"badAnchors"`
	PreSelected int    `json:"preSelected"`
	Line17      string `json:"line17"`
	Meta        string `json:"meta"`
}

// readTextRender checks each row's anchor id and data-line against its
// position, so a gutter that numbered rows wrong fails as a count rather than
// passing on mere presence.
const readTextRender = `(() => {
  const rows = Array.from(document.querySelectorAll("#text .row"));
  const bad = rows.filter((r, i) => r.id !== "L" + (i + 1) || r.dataset.line !== String(i + 1));
  const l17 = document.querySelector("#L17 .lg-code");
  return {
    rows: rows.length,
    badAnchors: bad.length,
    preSelected: document.querySelectorAll("#text .row.lg-sel").length,
    line17: l17 ? l17.textContent : "",
    meta: document.getElementById("meta").textContent,
  };
})()`

// lineSelectedJS reports that the gutter click landed: exactly line 5 carries
// the selection mark and the address bar cites it.
const lineSelectedJS = `location.hash === "#L5" &&
  Array.from(document.querySelectorAll("#text .row.lg-sel"), r => r.dataset.line).join() === "5"`

// anchorSettledJS is polled, not snapshotted: scrollIntoView animates, so the
// scroll offset is not readable on the line after the selection appears. The
// in-view arm is what a page too short to scroll settles on.
const anchorSettledJS = `window.scrollY > 0 || (() => {
  const a = document.getElementById("L10");
  if (!a) return false;
  const r = a.getBoundingClientRect();
  return r.top >= 0 && r.bottom <= window.innerHeight;
})()`

// deepLinkState is the viewer's state after arriving on a #L10-L20 URL.
type deepLinkState struct {
	Selected  []string `json:"selected"`
	Arrived   int      `json:"arrived"`
	Hash      string   `json:"hash"`
	ScrollY   float64  `json:"scrollY"`
	AnchorTop float64  `json:"anchorTop"`
	AnchorBot float64  `json:"anchorBot"`
	ViewportH float64  `json:"viewportH"`
}

const readDeepLink = `(() => {
  const a = document.getElementById("L10");
  const r = a ? a.getBoundingClientRect() : { top: -1, bottom: -1 };
  return {
    selected: Array.from(document.querySelectorAll("#text .row.lg-sel"), row => row.dataset.line),
    arrived: document.querySelectorAll("#text .row.lg-arrive").length,
    hash: location.hash,
    scrollY: window.scrollY,
    anchorTop: r.top,
    anchorBot: r.bottom,
    viewportH: window.innerHeight,
  };
})()`

// The text shell renders a numbered, line-addressable document: every line an
// anchor, a gutter click that cites a line in the fragment, and a #L10-L20
// arrival that selects the range and brings it into view.
func TestTextRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, textFixture(), UploadOpts{Type: "txt", Name: "text proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "text-render")
	br.Open(t, paste.URL)

	// aria-busy flips false only after the rows are appended, and the error
	// path writes no .row at all, so the pair matches nothing but a finished
	// render. Scoped under the browser cap so a stuck render leaves a live
	// context to screenshot from; waiting on br.Ctx would burn the budget and
	// kill it.
	var got textRender
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible(`#text[aria-busy="false"] .row`, chromedp.ByQuery),
		chromedp.Evaluate(readTextRender, &got),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("read rendered text: %v\n#text: %q\npage errors: %v",
			err, textShellText(t, br), br.Errors())
	}
	flow.Shot("plain")

	if got.Rows != textFixtureLines {
		t.Errorf("rendered %d line row(s), want %d", got.Rows, textFixtureLines)
	}
	if got.BadAnchors != 0 {
		t.Errorf("%d row(s) whose anchor id or data-line disagrees with their position", got.BadAnchors)
	}
	if got.PreSelected != 0 {
		t.Errorf("%d row(s) selected before any interaction", got.PreSelected)
	}
	if got.Line17 != wantLine17 {
		t.Errorf("line 17 = %q, want %q", got.Line17, wantLine17)
	}
	if want := "30 lines"; got.Meta != want {
		t.Errorf("meta = %q, want %q", got.Meta, want)
	}

	// A live gutter, not a painted one: clicking line 5's number must mark the
	// row and write the citation into the fragment.
	selectCtx, cancelSelect := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelSelect()
	if err := chromedp.Run(selectCtx,
		chromedp.Click("#L5 .lg-num", chromedp.ByQuery),
		chromedp.Poll(lineSelectedJS, nil),
	); err != nil {
		flow.Shot("select-stuck")
		t.Fatalf("click line 5's gutter: %v\npage errors: %v", err, br.Errors())
	}
	flow.Shot("line-selected")

	// Through about:blank so the fragment arrives on a cross-document load,
	// the cited-link case deeplink.js exists for: the browser's own fragment
	// attempt runs against an empty document, and the shell must resolve it
	// after the rows exist. A fragment-only navigation would instead exercise
	// the hashchange path on an already-rendered page.
	br.Open(t, "about:blank")
	br.Open(t, paste.URL+"#L10-L20")

	var dl deepLinkState
	deepCtx, cancelDeep := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelDeep()
	if err := chromedp.Run(deepCtx,
		chromedp.WaitVisible("#text .row.lg-sel", chromedp.ByQuery),
		chromedp.Poll(anchorSettledJS, nil),
		chromedp.Evaluate(readDeepLink, &dl),
	); err != nil {
		flow.Shot("deeplink-stuck")
		t.Fatalf("resolve #L10-L20 on arrival: %v\n#text: %q\npage errors: %v",
			err, textShellText(t, br), br.Errors())
	}
	flow.Shot("deep-linked")

	wantSel := []string{"10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20"}
	if !slices.Equal(dl.Selected, wantSel) {
		t.Errorf("selected lines = %v, want %v", dl.Selected, wantSel)
	}
	if dl.Arrived != len(wantSel) {
		t.Errorf("%d row(s) carry the arrival flash, want %d", dl.Arrived, len(wantSel))
	}
	if dl.Hash != "#L10-L20" {
		t.Errorf("fragment after resolve = %q, want %q", dl.Hash, "#L10-L20")
	}
	if dl.ScrollY <= 0 && !(dl.AnchorTop >= 0 && dl.AnchorBot <= dl.ViewportH) {
		t.Errorf("anchor L10 neither scrolled to nor in view: scrollY=%g top=%g bottom=%g viewport=%g",
			dl.ScrollY, dl.AnchorTop, dl.AnchorBot, dl.ViewportH)
	}

	br.AssertNoPageErrors(t)
}

// textShellText is what the text shell left on screen, for a failure message.
func textShellText(t *testing.T, br *Browser) string {
	t.Helper()
	var s string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.getElementById("text") || {}).textContent || ""`, &s),
	); err != nil {
		return "unreadable: " + err.Error()
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
