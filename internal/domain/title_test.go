package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentTitle(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, tc := range []struct {
		name string
		kind ContentKind
		in   string
		want string
	}{
		{"first heading", KindMarkdown, "intro line\n\n## Design notes ##\n# Later\n", "Design notes"},
		{"heading text keeps an inner hash", KindMarkdown, "# C# tips\n", "C# tips"},
		{"empty heading is skipped", KindMarkdown, "#\n# Real\n", "Real"},
		{"hashtag is not a heading", KindMarkdown, "#tag line\n", "#tag line"},
		{"fenced heading is code", KindMarkdown, "```sh\n# not a title\n```\n# Title\n", "Title"},
		{"tilde fence", KindMarkdown, "~~~\n# no\n~~~\n### Yes\n", "Yes"},
		{"indented four is code", KindMarkdown, "    # code\n# Heading\n", "Heading"},
		{"front matter is skipped", KindMarkdown, "---\ntitle: x\n---\n# Doc\n", "Doc"},
		{"no heading falls back to the first line", KindMarkdown, "\n\n  just some *text*\nmore\n", "just some *text*"},
		{"bom and crlf", KindMarkdown, "\ufeff# Windows doc\r\nbody\r\n", "Windows doc"},
		{"text first line", KindText, "\n   \nhello   world\t again\nnext", "hello world again"},
		{"json skips punctuation lines", KindJSON, "{\n  \"name\": \"demo\"\n}\n", `"name": "demo"`},
		{"diff header", KindDiff, "diff --git a/x b/x\n--- a/x\n", "diff --git a/x b/x"},
		{"controls dropped", KindText, "a\x00b\x1bc\n", "abc"},
		{"invalid utf8 dropped", KindText, "ok\xff\xfe go\n", "ok go"},
		{"nothing qualifies", KindText, "\n---\n***\n", ""},
		{"html serves its own title", KindHTML, "<title>x</title>", ""},
		{"pdf has none", KindPDF, "%PDF-1.7\nTitle", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DocumentTitle(tc.kind, []byte(tc.in)); got != tc.want {
				t.Fatalf("DocumentTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("capped with an ellipsis", func(t *testing.T) {
		got := DocumentTitle(KindMarkdown, []byte("# "+long))
		if n := utf8.RuneCountInString(got); n > MaxTitleRunes || !strings.HasSuffix(got, "…") {
			t.Fatalf("got %d runes %q, want at most %d ending in an ellipsis", n, got, MaxTitleRunes)
		}
		if !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
			t.Fatalf("cap changed the text: %q", got)
		}
	})
	t.Run("multibyte cap stays valid", func(t *testing.T) {
		got := DocumentTitle(KindText, []byte(strings.Repeat("é", 200)))
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) != MaxTitleRunes {
			t.Fatalf("got %q", got)
		}
	})
}
