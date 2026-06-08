package render

import (
	"strings"
	"testing"
)

func TestMarkdown_RendersBasicBlocks(t *testing.T) {
	src := []byte(`# Hello

This is a paragraph with **bold** and _italic_ and ` + "`inline code`" + `.

## A list

- item one
- item two

` + "```go\nfunc main() { fmt.Println(\"hi\") }\n```\n")

	out, err := Markdown(src)
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"<!doctype html>",
		"<title>Hello</title>",
		"<h1",
		"Hello",
		"<strong>bold</strong>",
		"<em>italic</em>",
		"<code>inline code</code>",
		"<h2",
		"<ul>",
		"<li>item one</li>",
		"<pre>",
		"func main",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, s)
		}
	}
}

func TestMarkdown_StripsScriptTag(t *testing.T) {
	src := []byte(`# safe

before
<script>alert('xss')</script>
after`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "<script") {
		t.Fatalf("output contains <script>: %s", s)
	}
	if strings.Contains(s, "alert(") {
		t.Fatalf("output contains alert(: %s", s)
	}
}

func TestMarkdown_StripsJavascriptURL(t *testing.T) {
	src := []byte(`[click me](javascript:alert(1))`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(strings.ToLower(s), "javascript:") {
		t.Fatalf("output contains javascript: URL: %s", s)
	}
}

func TestMarkdown_StripsOnclickHandlers(t *testing.T) {
	src := []byte(`<button onclick="alert(1)">click</button>`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "onclick") {
		t.Fatalf("output contains onclick: %s", out)
	}
}

func TestMarkdown_PreservesSafeLink(t *testing.T) {
	src := []byte(`See [hostthis](https://paste.test/).`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `href="https://paste.test/"`) {
		t.Fatalf("safe link missing: %s", s)
	}
}

func TestMarkdown_PreservesImage(t *testing.T) {
	src := []byte(`![alt text](https://example.com/pic.png)`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `<img`) || !strings.Contains(s, `src="https://example.com/pic.png"`) {
		t.Fatalf("img missing or wrong: %s", s)
	}
}

func TestMarkdown_GFMTable(t *testing.T) {
	src := []byte(`| a | b |
| --- | --- |
| 1 | 2 |`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "<table") {
		t.Fatalf("table not rendered: %s", s)
	}
	if !strings.Contains(s, "<th>a</th>") {
		t.Fatalf("th missing: %s", s)
	}
}

func TestMarkdown_TaskList(t *testing.T) {
	src := []byte(`- [x] done
- [ ] todo`)
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `type="checkbox"`) {
		t.Fatalf("task list checkboxes missing: %s", s)
	}
	if !strings.Contains(s, "checked") {
		t.Fatalf("checked attr missing on done item: %s", s)
	}
}

func TestMarkdown_AutoHeadingID(t *testing.T) {
	src := []byte("# A Title")
	out, err := Markdown(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `id="a-title"`) {
		t.Fatalf("auto heading id missing: %s", s)
	}
}

func TestMarkdown_TitleFallback(t *testing.T) {
	out, err := Markdown([]byte("no h1 here, just text"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "<title>markdown</title>") {
		t.Fatalf("fallback title missing: %s", s)
	}
}

func TestMarkdown_TitleEscapesHTML(t *testing.T) {
	out, err := Markdown([]byte(`# Tom & Jerry <3`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// & should be escaped, < should be escaped
	if !strings.Contains(s, "<title>Tom &amp; Jerry &lt;3</title>") {
		t.Fatalf("title escaping wrong: %s", s[:300])
	}
}
