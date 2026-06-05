package domain

import (
	"errors"
	"net/http"
	"strings"
)

// ContentKind is what a paste is — the spec accepts only HTML and
// Markdown in v1. Anything else is rejected at the upload boundary so
// the sandbox + renderer never see surprising bytes.
type ContentKind string

const (
	KindHTML     ContentKind = "html"
	KindMarkdown ContentKind = "markdown"
)

// ErrUnsupportedKind is returned when content sniffs to something
// outside the v1 accepted set. The error message is what the user
// sees on stderr.
var ErrUnsupportedKind = errors.New(
	"hostthis only accepts content that needs rendering (html, markdown)")

// MaxPasteBytes is the universal per-paste size cap. Per SPEC.md, this
// is hardcoded — never raised by config or by tier.
const MaxPasteBytes = 5 << 20 // 5 MiB

// DetectKind classifies up to the first 512 bytes of an upload as one
// of the supported ContentKinds, or returns ErrUnsupportedKind.
//
// HTML detection: standard net/http content-type sniffing. Anything
// that comes back as text/html (regardless of charset) is HTML.
//
// Markdown detection: text/plain (no embedded magic bytes) + at least
// one common markdown structural cue in the first 1 KB. Pure plain
// text gets rejected — markdown without any structure is just text,
// and unrendered text isn't a hostthis use case.
//
// The hint argument is an optional explicit content-type the caller
// supplies (e.g. from a `--type` flag); when present it overrides
// sniffing. Pass "" to sniff.
func DetectKind(b []byte, hint string) (ContentKind, error) {
	hint = strings.ToLower(strings.TrimSpace(hint))
	switch {
	case hint == "html" || strings.HasPrefix(hint, "text/html"):
		return KindHTML, nil
	case hint == "md" || hint == "markdown" || strings.HasPrefix(hint, "text/markdown"):
		return KindMarkdown, nil
	case hint != "":
		// Caller gave an explicit hint we don't recognize → reject up
		// front; don't silently fall through to sniffing.
		return "", ErrUnsupportedKind
	}

	// No hint — sniff.
	sniff := b
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ct := http.DetectContentType(sniff)
	switch {
	case strings.HasPrefix(ct, "text/html"):
		return KindHTML, nil
	case strings.HasPrefix(ct, "text/plain"):
		if looksLikeMarkdown(b) {
			return KindMarkdown, nil
		}
		return "", ErrUnsupportedKind
	default:
		return "", ErrUnsupportedKind
	}
}

// looksLikeMarkdown returns true when the input contains at least one
// markdown structural cue in its first 1 KB. The cue list is deliberately
// modest — false positives let a plain-text file render as markdown, which
// is harmless (the renderer just emits a <p>); false negatives reject a
// real markdown doc, which is worse.
func looksLikeMarkdown(b []byte) bool {
	head := b
	if len(head) > 1024 {
		head = head[:1024]
	}
	s := string(head)
	switch {
	case strings.Contains(s, "\n# "), strings.HasPrefix(s, "# "):
		return true // ATX heading
	case strings.Contains(s, "\n## "), strings.HasPrefix(s, "## "):
		return true
	case strings.Contains(s, "\n```"), strings.HasPrefix(s, "```"):
		return true // fenced code block
	case strings.Contains(s, "\n- "), strings.HasPrefix(s, "- "):
		return true // bullet list
	case strings.Contains(s, "\n* "), strings.HasPrefix(s, "* "):
		return true
	case strings.Contains(s, "\n> "), strings.HasPrefix(s, "> "):
		return true // blockquote
	case strings.Contains(s, "](http"):
		return true // inline link
	case strings.Contains(s, "\n---\n"):
		return true // setext/horizontal rule
	}
	return false
}
