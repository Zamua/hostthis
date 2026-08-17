//go:build e2e

package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// mermaidSource declares exactly four nodes and three edges, with labels
// distinctive enough that finding them cannot be a coincidental match on the
// shell's own chrome.
const mermaidSource = `flowchart TD
    A[Upload over ssh] --> B{Sniff the kind}
    B -->|mermaid| C[Mermaid shell]
    B -->|markdown| D[Markdown shell]
`

var wantNodeLabels = []string{"Upload over ssh", "Sniff the kind", "Mermaid shell", "Markdown shell"}

// renderTimeout bounds the wait for mermaid to settle. Shorter than the browser
// budget so a shell that never settles reports that, rather than spending the
// rest of the test's time on it.
const renderTimeout = 30 * time.Second

// diagram is the shape of the rendered svg, read back out of the page.
type diagram struct {
	Nodes      int      `json:"nodes"`
	Edges      int      `json:"edges"`
	NodeLabels []string `json:"nodeLabels"`
	Width      float64  `json:"width"`
	Height     float64  `json:"height"`
}

// The sharpest case of the blank-page class: mermaid is rendered client-side,
// so a bundle that fails to load leaves the paste unrendered while every status
// code stays 200. An svg carrying this source's nodes and edges exists only if
// mermaid.js actually ran.
func TestMermaidRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(mermaidSource), UploadOpts{Type: "mermaid", Name: "mermaid proof"})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "mermaid-render")
	br.Open(t, paste.URL)

	// Both outcomes end the shell's work, so waiting on the pair reports a
	// failed render as the message the shell wrote instead of as a timeout.
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible("#content svg, #content .err", chromedp.ByQuery),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("shell settled on neither a diagram nor an error: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("rendered")

	var shellErr string
	var d *diagram
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.querySelector("#content .err") || {}).textContent || ""`, &shellErr),
		chromedp.Evaluate(probeDiagram, &d),
	); err != nil {
		t.Fatalf("probe rendered page: %v", err)
	}

	if shellErr != "" {
		t.Fatalf("shell rendered its failure message: %s", shellErr)
	}
	if d == nil {
		t.Fatal("no svg under #content: the mermaid renderer did not run")
	}
	if d.Nodes != len(wantNodeLabels) || d.Edges != 3 {
		t.Errorf("svg has %d node(s) and %d edge(s), want %d and 3: the element is empty or partly drawn",
			d.Nodes, d.Edges, len(wantNodeLabels))
	}
	if !slices.Equal(d.NodeLabels, wantNodeLabels) {
		t.Errorf("node labels = %q, want %q", d.NodeLabels, wantNodeLabels)
	}
	if d.Width <= 0 || d.Height <= 0 {
		t.Errorf("svg lays out to %gx%g: nothing is on screen", d.Width, d.Height)
	}

	br.AssertNoPageErrors(t)
}

// probeDiagram reports the svg's shape. Labels are read off the label elements
// rather than the svg's textContent, which also carries mermaid's injected
// stylesheet and would match anything.
const probeDiagram = `(() => {
  const svg = document.querySelector("#content svg");
  if (!svg) return null;
  const box = svg.getBoundingClientRect();
  return {
    nodes: svg.querySelectorAll("g.node").length,
    edges: svg.querySelectorAll("g.edgePaths path").length,
    nodeLabels: [...svg.querySelectorAll("g.node .nodeLabel")].map(e => e.textContent.trim()),
    width: box.width,
    height: box.height,
  };
})()`

// contentText is what the shell left on screen, for a failure message.
func contentText(t *testing.T, br *Browser) string {
	t.Helper()
	var s string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.getElementById("content") || {}).textContent || ""`, &s),
	); err != nil {
		return "unreadable: " + err.Error()
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
