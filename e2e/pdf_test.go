//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// pdfFixtureText is the only string the document draws, distinctive enough
// that finding it in the text layer cannot match the shell's own chrome. It
// is spliced into a PDF literal string, so it must stay free of ( ) and \.
const pdfFixtureText = "hostthis pdf proof"

// pdfFixture is a complete one-page PDF drawing pdfFixtureText in Helvetica.
// The stream /Length and the xref offsets are literal byte counts of THIS
// layout; an edit to any object body must recompute them.
const pdfFixture = "%PDF-1.4\n" +
	"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R " +
	"/Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n" +
	"4 0 obj\n<< /Length 49 >>\nstream\n" +
	"BT /F1 24 Tf 72 720 Td (" + pdfFixtureText + ") Tj ET\n" +
	"endstream\nendobj\n" +
	"5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n" +
	"xref\n0 6\n" +
	"0000000000 65535 f \n" +
	"0000000009 00000 n \n" +
	"0000000058 00000 n \n" +
	"0000000115 00000 n \n" +
	"0000000241 00000 n \n" +
	"0000000340 00000 n \n" +
	"trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n410\n%%EOF\n"

// pdfSettledJS is true once the shell reached an end state. The text layer is
// written only after pdf.js painted the page's canvas, so the fixture text
// appearing in it proves the worker parsed the document and page 1 rendered;
// before the script runs #content holds only the loading .status div, which
// matches neither branch.
var pdfSettledJS = `!!document.querySelector("#content .err") || ` +
	`[...document.querySelectorAll("#content .textLayer span")]` +
	`.some((s) => s.textContent.includes("` + pdfFixtureText + `"))`

// pdfRender is every signal the rendered viewer should produce, read in one
// pass so a failure names all of them rather than only the first.
type pdfRender struct {
	Err          string  `json:"err"`
	Pages        int     `json:"pages"`
	Pos          string  `json:"pos"`
	CanvasW      int     `json:"canvasW"`
	CanvasH      int     `json:"canvasH"`
	BoxW         float64 `json:"boxW"`
	BoxH         float64 `json:"boxH"`
	LayerText    string  `json:"layerText"`
	PrevDisabled bool    `json:"prevDisabled"`
	NextDisabled bool    `json:"nextDisabled"`
}

const readPDFRender = `(() => {
  const text = (el) => (el ? el.textContent.trim() : "");
  const canvas = document.querySelector("#content .page canvas");
  const box = canvas ? canvas.getBoundingClientRect() : { width: 0, height: 0 };
  return {
    err: text(document.querySelector("#content .err")),
    pages: document.querySelectorAll("#content .page").length,
    pos: text(document.getElementById("pos")),
    canvasW: canvas ? canvas.width : 0,
    canvasH: canvas ? canvas.height : 0,
    boxW: box.width,
    boxH: box.height,
    layerText: [...document.querySelectorAll("#content .textLayer span")].map((s) => s.textContent).join(" "),
    prevDisabled: document.getElementById("prev").disabled,
    nextDisabled: document.getElementById("next").disabled,
  };
})()`

// The pdf shell parses the document on a worker, paints page 1 to a canvas
// with a selectable text layer over it, reports the page count, and leaves
// both paging controls disabled on a one-page document.
func TestPDFRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(pdfFixture), UploadOpts{Type: "pdf", Name: "pdf proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "pdf-render")
	br.Open(t, paste.URL)

	// Scoped under the browser cap so a stuck render leaves a live context to
	// screenshot from; waiting on br.Ctx would burn the budget and kill it.
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	var got pdfRender
	if err := chromedp.Run(renderCtx,
		chromedp.Poll(pdfSettledJS, nil),
		chromedp.Evaluate(readPDFRender, &got),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("shell settled on neither a page nor an error: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("rendered")

	if got.Err != "" {
		t.Fatalf("shell rendered its failure message: %s", got.Err)
	}
	if got.Pages != 1 {
		t.Errorf("page holders = %d, want 1", got.Pages)
	}
	// Pre-render the counter reads the en-dash placeholder, so this value only
	// exists once the document's page count came back from the worker.
	if got.Pos != "1 / 1" {
		t.Errorf("page counter = %q, want %q", got.Pos, "1 / 1")
	}
	// The paint proof is the text layer above; the dimensions pin that the
	// painted canvas also lays out on screen rather than being collapsed away.
	if got.CanvasW <= 0 || got.CanvasH <= 0 {
		t.Errorf("canvas backing store is %dx%d: nothing was painted", got.CanvasW, got.CanvasH)
	}
	if got.BoxW <= 0 || got.BoxH <= 0 {
		t.Errorf("canvas lays out to %gx%g: nothing is on screen", got.BoxW, got.BoxH)
	}
	if !strings.Contains(got.LayerText, pdfFixtureText) {
		t.Errorf("text layer %q does not contain %q", got.LayerText, pdfFixtureText)
	}
	// Both buttons exist enabled in the static shell; setPos disabling both is
	// the paging control's correct one-page state and only the renderer sets it.
	if !got.PrevDisabled || !got.NextDisabled {
		t.Errorf("paging on a 1-page doc: prev disabled=%v next disabled=%v, want both true",
			got.PrevDisabled, got.NextDisabled)
	}

	// The page counter doubles as a copy-link control wired only after render.
	// Both clipboard outcomes end in a flash, so the .on class is deterministic
	// while a dead handler times out here instead of passing vacuously.
	var flashed string
	copyCtx, cancelCopy := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelCopy()
	if err := chromedp.Run(copyCtx,
		chromedp.Click("#pos", chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("copied").classList.contains("on")`, nil),
		chromedp.Evaluate(`document.getElementById("copied").textContent`, &flashed),
	); err != nil {
		flow.Shot("copy-stuck")
		t.Fatalf("copy-link control never flashed: %v\npage errors: %v", err, br.Errors())
	}
	flow.Shot("copy-flash")

	if flashed != "link copied" && flashed != "link is in the address bar" {
		t.Errorf("copy flash = %q, want one of the shell's two confirmations", flashed)
	}

	br.AssertNoPageErrors(t)
}
