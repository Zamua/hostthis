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
