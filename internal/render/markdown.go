// Package render turns user-uploaded source into safe-to-serve HTML. Each
// renderer is a pure function: source bytes in, HTML bytes out. No I/O, no
// caching, no global state.
//
// Not on the production serve path (the serve surface renders client-side);
// used by the render-md tool and the markdown memory benchmark.
package render

import (
	"bytes"
	"fmt"
	stdhtml "html"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// Markdown renders source markdown into a self-contained sanitized HTML
// document: a complete <!doctype html> page with inline default styling, ready
// to serve as-is.
//
// goldmark parses with GFM extensions and unsafe HTML PRESERVED, then
// bluemonday's UGCPolicy strips the dangerous tags and attributes (`<script>`,
// `javascript:` URLs, `onclick=`, dangerous form actions) from the intermediate
// HTML. That order is what lets inline HTML the user wrote survive parsing and
// still be sanitized. The wrapper template is server-controlled and trusted;
// only the body is user content.
func Markdown(src []byte) ([]byte, error) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Footnote,
			extension.DefinitionList,
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			gmhtml.WithUnsafe(), // keep inline HTML; bluemonday will sanitize after
		),
	)

	var rendered bytes.Buffer
	if err := md.Convert(src, &rendered); err != nil {
		return nil, fmt.Errorf("markdown convert: %w", err)
	}

	sanitized := sanitizer().SanitizeBytes(rendered.Bytes())
	title := extractTitle(src)

	var page bytes.Buffer
	page.Grow(len(sanitized) + len(defaultStyles) + 1024)
	page.WriteString("<!doctype html>\n")
	page.WriteString(`<meta charset="utf-8">` + "\n")
	page.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">` + "\n")
	page.WriteString(`<meta name="color-scheme" content="light dark">` + "\n")
	page.WriteString("<title>" + stdhtml.EscapeString(title) + "</title>\n")
	page.WriteString(defaultStyles)
	page.WriteString("<article>\n")
	page.Write(sanitized)
	page.WriteString("\n</article>\n")
	return page.Bytes(), nil
}

// sanitizer returns a bluemonday policy that is strict-by-default for uploaded
// HTML embedded in markdown. UGCPolicy already strips scripts, event handlers,
// javascript: URLs, etc. Extensions on top of it:
//   - `id` on headings (goldmark auto-generates them; anchors break otherwise)
//   - `class` for syntax highlighting if the source set it
//   - `<input type=checkbox disabled checked>` so GFM task lists render
//     (UGCPolicy strips <input> entirely by default)
func sanitizer() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("id").OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowAttrs("class").OnElements("code", "pre", "span", "div", "table", "th", "td")
	// goldmark emits <input checked="" disabled="" type="checkbox"> for
	// `- [x] item`. UGCPolicy strips <input> entirely; allow it only with the
	// safe attrs the task list extension uses.
	p.AllowElements("input")
	p.AllowAttrs("type").Matching(bluemonday.SpaceSeparatedTokens).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	return p
}

// extractTitle scans the source for the first ATX h1 (`# ...`) line, falling
// back to "markdown". Used only for the <title> tag; the rendered body still
// carries the heading.
func extractTitle(src []byte) string {
	for line := range bytes.SplitSeq(src, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("# ")) {
			return strings.TrimSpace(string(trimmed[2:]))
		}
	}
	return "markdown"
}

// defaultStyles is the inline stylesheet for rendered markdown pages: monospace
// headings, serif body, neutral palette matching the apex landing page,
// mobile-first.
const defaultStyles = `<style>
  :root {
    --bg:#ffffff; --ink:#111111; --soft:#555555; --accent:#111111;
    --rule:#dddddd; --code-bg:#f4f4f4;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg:#0a0a0a; --ink:#f0f0f0; --soft:#999999; --accent:#f0f0f0;
      --rule:#2a2a2a; --code-bg:#1a1a1a;
    }
  }
  * { box-sizing: border-box; }
  html, body { background: var(--bg); color: var(--ink); }
  body {
    font: 16px/1.6 ui-serif, Georgia, Cambria, "Times New Roman", serif;
    max-width: 70ch;
    margin: 0 auto;
    padding: 2rem 1.4rem 4rem;
  }
  article > *:first-child { margin-top: 0; }
  h1, h2, h3, h4, h5, h6 {
    font-family: ui-monospace, SFMono-Regular, "JetBrains Mono", Menlo, Consolas, monospace;
    font-weight: 700;
    letter-spacing: -0.01em;
    line-height: 1.25;
    margin: 2rem 0 0.8rem;
  }
  h1 { font-size: 2rem; margin-top: 0; }
  h2 { font-size: 1.4rem; padding-bottom: 0.3rem; border-bottom: 1px solid var(--rule); }
  h3 { font-size: 1.15rem; }
  h4, h5, h6 { font-size: 1rem; color: var(--soft); text-transform: uppercase; letter-spacing: 0.05em; }
  p { margin: 0.6rem 0 1rem; }
  a { color: var(--ink); text-decoration: underline; text-underline-offset: 3px; text-decoration-color: var(--soft); }
  a:hover { text-decoration-color: var(--ink); text-decoration-thickness: 2px; }
  hr { border: 0; border-top: 1px solid var(--rule); margin: 2rem 0; }
  blockquote {
    margin: 1rem 0;
    padding: 0.4rem 1rem;
    border-left: 3px solid var(--accent);
    color: var(--soft);
    background: var(--code-bg);
  }
  ul, ol { padding-left: 1.4rem; margin: 0.6rem 0 1rem; }
  li { margin: 0.2rem 0; }
  li > p { margin: 0.2rem 0; }
  code {
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 0.92em;
    background: var(--code-bg);
    padding: 0.1em 0.35em;
    border-radius: 3px;
  }
  pre {
    background: var(--code-bg);
    border: 1px solid var(--rule);
    border-radius: 4px;
    padding: 0.9rem 1.1rem;
    overflow-x: auto;
    line-height: 1.5;
    margin: 1rem 0;
  }
  pre code {
    background: transparent;
    padding: 0;
    font-size: 0.9rem;
  }
  img { max-width: 100%; height: auto; border-radius: 4px; }
  table {
    border-collapse: collapse;
    margin: 1rem 0;
    width: 100%;
    font-size: 0.95rem;
  }
  th, td {
    border: 1px solid var(--rule);
    padding: 0.5rem 0.8rem;
    text-align: left;
  }
  th { background: var(--code-bg); }
  /* GFM task list checkboxes */
  input[type="checkbox"] { margin-right: 0.4rem; }
  /* Footnotes */
  .footnotes { margin-top: 2rem; padding-top: 1rem; border-top: 1px solid var(--rule); font-size: 0.9rem; color: var(--soft); }
  /* Inline blockquote-of-code spacing */
  pre + p, blockquote + p { margin-top: 1.2rem; }
</style>
`
