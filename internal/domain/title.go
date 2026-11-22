package domain

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleRunes caps a derived document title. A link preview shows one line,
// and the title rides in every rendered shell of the paste.
const MaxTitleRunes = 80

// DocumentTitle is the title a rendered document shows a link unfurler, which
// runs no script: derived from the upload's leading bytes (the same prefix
// DetectKind classifies), so it costs no second read. Markdown takes its first
// heading outside code fences and front matter; every textual kind otherwise
// takes its first line with a letter or digit in it. An HTML paste serves its
// own bytes, title included, and a PDF or an archive has no text prefix, so
// those yield "". The result is one line of printable text, at most
// MaxTitleRunes runes, or "" when nothing qualifies.
func DocumentTitle(kind ContentKind, prefix []byte) string {
	switch kind {
	case KindHTML, KindPDF, KindSite, KindKnowledgeBase:
		return ""
	}
	lines := strings.Split(string(bytes.TrimPrefix(prefix, []byte("\ufeff"))), "\n")
	if kind == KindMarkdown {
		if heading := firstHeading(lines); heading != "" {
			return heading
		}
	}
	for _, line := range lines {
		if title := cleanTitle(line); hasLetterOrDigit(title) {
			return title
		}
	}
	return ""
}

// firstHeading is the text of the first ATX heading (# to ######) outside a
// fenced code block and a leading front matter block.
func firstHeading(lines []string) string {
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				start = i + 1
				break
			}
		}
	}
	fence := ""
	for _, line := range lines[start:] {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 {
			continue
		}
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		if hashes == 0 || hashes > 6 {
			continue
		}
		rest := trimmed[hashes:]
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '\r' {
			continue
		}
		// An optional closing sequence of #s is not part of the text.
		text := strings.TrimSpace(rest)
		if closing := strings.TrimRight(text, "#"); closing != text && (closing == "" || strings.HasSuffix(closing, " ")) {
			text = closing
		}
		if title := cleanTitle(text); title != "" {
			return title
		}
	}
	return ""
}

// cleanTitle is line as one line of printable text: invalid UTF-8 and
// control characters dropped, whitespace runs collapsed to one space, and
// capped at MaxTitleRunes with an ellipsis.
func cleanTitle(line string) string {
	line = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(line, ""))
	line = strings.Join(strings.Fields(line), " ")
	if utf8.RuneCountInString(line) <= MaxTitleRunes {
		return line
	}
	runes := []rune(line)
	return strings.TrimSpace(string(runes[:MaxTitleRunes-1])) + "…"
}

func hasLetterOrDigit(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}
