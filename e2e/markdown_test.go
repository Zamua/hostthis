//go:build e2e

package e2e

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// Tilde fences are used because a Go raw string cannot contain a backtick.
const markdownFixture = `# Renderer proof

A paragraph with **bold text** and *italic text*.

> Rendered in the browser, never on the server.

| kind     | renderer   |
| -------- | ---------- |
| markdown | marked     |
| mermaid  | mermaid.js |
| csv      | duckdb     |

~~~go
func main() {
	fmt.Println("fenced")
}
~~~

- [x] upload over ssh
- [ ] render in the browser
`

// markdownRender is every signal the fixture should produce, read in one pass
// so a failure names all of them rather than only the first.
type markdownRender struct {
	Heading    string     `json:"heading"`
	HeadingID  string     `json:"headingID"`
	Strong     []string   `json:"strong"`
	Em         []string   `json:"em"`
	Blockquote string     `json:"blockquote"`
	TableHead  []string   `json:"tableHead"`
	TableRows  [][]string `json:"tableRows"`
	CodeClass  string     `json:"codeClass"`
	CodeText   string     `json:"codeText"`
	Tasks      int        `json:"tasks"`
	TasksDone  int        `json:"tasksDone"`
}

const readMarkdownRender = `(() => {
  const root = document.getElementById("content");
  const text = (el) => (el ? el.textContent.trim() : "");
  const all = (sel) => Array.from(root.querySelectorAll(sel));
  const h1 = root.querySelector("h1");
  const code = root.querySelector("pre > code");
  const boxes = all('li input[type="checkbox"]');
  return {
    heading: text(h1),
    headingID: h1 ? h1.id : "",
    strong: all("strong").map(text),
    em: all("em").map(text),
    blockquote: text(root.querySelector("blockquote")),
    tableHead: all("table thead th").map(text),
    tableRows: all("table tbody tr").map((tr) => Array.from(tr.cells).map(text)),
    codeClass: code ? code.className : "",
    codeText: code ? code.textContent : "",
    tasks: boxes.length,
    tasksDone: boxes.filter((b) => b.checked).length,
  };
})()`

// The markdown shell turns an uploaded document into a rendered DOM: heading,
// inline emphasis, table, blockquote, language-tagged fence and task list.
func TestMarkdownRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(markdownFixture), UploadOpts{
		Type: "md",
		Name: "markdown render",
	})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "markdown-render")
	br.Open(t, paste.URL)

	// The document arrives in one innerHTML assignment and heading ids are added
	// after it, so an anchored h1 means every other block is already in the DOM.
	// Scoped under the browser cap so a stuck render leaves a live context to
	// screenshot from. Waiting on br.Ctx directly would burn the whole budget
	// and then kill the context, so the blank-page case this test exists to
	// catch would publish a filmstrip with no frames.
	var got markdownRender
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible("#content h1.anchored", chromedp.ByQuery),
		chromedp.Evaluate(readMarkdownRender, &got),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("read rendered markdown: %v\n#content: %q\npage errors: %v",
			err, contentText(t, br), br.Errors())
	}
	flow.Shot("rendered")

	if got.Heading != "Renderer proof" {
		t.Errorf("h1 text = %q, want %q", got.Heading, "Renderer proof")
	}
	if got.HeadingID != "renderer-proof" {
		t.Errorf("h1 id = %q, want %q", got.HeadingID, "renderer-proof")
	}
	if want := []string{"bold text"}; !reflect.DeepEqual(got.Strong, want) {
		t.Errorf("strong = %q, want %q", got.Strong, want)
	}
	if want := []string{"italic text"}; !reflect.DeepEqual(got.Em, want) {
		t.Errorf("em = %q, want %q", got.Em, want)
	}
	if want := "Rendered in the browser, never on the server."; got.Blockquote != want {
		t.Errorf("blockquote = %q, want %q", got.Blockquote, want)
	}
	if want := []string{"kind", "renderer"}; !reflect.DeepEqual(got.TableHead, want) {
		t.Errorf("table header cells = %q, want %q", got.TableHead, want)
	}
	wantRows := [][]string{
		{"markdown", "marked"},
		{"mermaid", "mermaid.js"},
		{"csv", "duckdb"},
	}
	if !reflect.DeepEqual(got.TableRows, wantRows) {
		t.Errorf("table body rows = %q, want %q", got.TableRows, wantRows)
	}
	if got.CodeClass != "language-go" {
		t.Errorf("fence class = %q, want %q", got.CodeClass, "language-go")
	}
	if want := `fmt.Println("fenced")`; !strings.Contains(got.CodeText, want) {
		t.Errorf("fence text %q does not contain %q", got.CodeText, want)
	}
	if got.Tasks != 2 || got.TasksDone != 1 {
		t.Errorf("task list = %d boxes with %d checked, want 2 with 1", got.Tasks, got.TasksDone)
	}

	br.AssertNoPageErrors(t)
}
