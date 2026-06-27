package domain

import (
	"errors"
	"testing"
)

func TestDetectKind(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		hint  string
		want  ContentKind
		isErr bool
	}{
		{name: "doctype html sniffed", body: "<!doctype html><html><body>hi</body></html>", want: KindHTML},
		{name: "html tag sniffed", body: "<html><body>hi</body></html>", want: KindHTML},
		{name: "markdown heading sniffed", body: "# Title\n\nbody", want: KindMarkdown},
		{name: "markdown fenced code sniffed", body: "intro\n```go\nfn()\n```\n", want: KindMarkdown},
		{name: "markdown bullet list sniffed", body: "stuff\n- one\n- two\n", want: KindMarkdown},
		{name: "markdown link sniffed", body: "see [docs](https://example.com)", want: KindMarkdown},
		{name: "plain text rejected", body: "just some plain text", isErr: true},
		{name: "binary rejected", body: "\x89PNG\r\n\x1a\n... png bytes", isErr: true},
		{name: "explicit html hint over textual", body: "anything textual here", hint: "html", want: KindHTML},
		{name: "explicit md hint over textual", body: "anything textual here", hint: "md", want: KindMarkdown},
		{name: "text/html hint", body: "anything textual here", hint: "text/html; charset=utf-8", want: KindHTML},
		{name: "text/markdown hint", body: "anything textual here", hint: "text/markdown", want: KindMarkdown},
		{name: "unknown hint rejected", body: "<!doctype html>", hint: "application/pdf", isErr: true},
		{name: "html hint with png bytes rejected", body: "\x89PNG\r\n\x1a\n...png bytes here padded to be long...", hint: "html", isErr: true},
		{name: "md hint with zip bytes rejected", body: "PK\x03\x04zip-archive-bytes-here-padded-to-be-long-enough", hint: "md", isErr: true},
		{name: "html hint with elf bytes rejected", body: "\x7fELF\x02\x01\x01\x00binary-elf-here-padded-to-be-long-enough", hint: "html", isErr: true},

		// --- diff detection ---
		{name: "git diff --git detected", body: "diff --git a/foo.go b/foo.go\nindex 1234567..89abcde 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n func main() {\n-\tx := 1\n+\tx := 2\n+\ty := 3\n }\n", want: KindDiff},
		{name: "plain diff -u detected", body: "--- old.txt\t2026-01-01\n+++ new.txt\t2026-01-02\n@@ -1,2 +1,2 @@\n-hello\n+goodbye\n world\n", want: KindDiff},
		{name: "single-line hunk header detected", body: "--- a\n+++ b\n@@ -1 +1 @@\n-one\n+two\n", want: KindDiff},
		{name: "hunk header with section heading detected", body: "diff --git a/x.c b/x.c\n--- a/x.c\n+++ b/x.c\n@@ -10,7 +10,7 @@ int compute(void) {\n-\treturn 1;\n+\treturn 2;\n", want: KindDiff},
		{name: "explicit diff hint forces kind", body: "this is not really a diff at all, just prose", hint: "diff", want: KindDiff},
		{name: "text/x-diff hint forces kind", body: "anything textual here", hint: "text/x-diff", want: KindDiff},
		{name: "prose with plus and minus lines is markdown not diff", body: "# Changelog\n\n- added a feature\n+ this is just a plus sign in prose\n- removed a thing\n", want: KindMarkdown},
		{name: "source code with plus minus operators is not diff", body: "function diff(a, b) {\n  const result = a + b - 1;\n  return result;\n}\n# also has a heading-like line", want: KindMarkdown},
		{name: "fake hunk header missing plus side is not diff", body: "some notes about @@ markers\n@@ -1,3 @@ not a real hunk\njust text here\nmore text", isErr: true},
		{name: "diff hint with png bytes rejected", body: "\x89PNG\r\n\x1a\n...png bytes here padded to be long enough...", hint: "diff", isErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DetectKind([]byte(c.body), c.hint)
			if c.isErr {
				if !errors.Is(err, ErrUnsupportedKind) {
					t.Fatalf("err: got %v, want ErrUnsupportedKind", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Fatalf("kind: got %q, want %q", got, c.want)
			}
		})
	}
}
