package domain

import (
	"errors"
	"net/http"
	"strings"
)

// ContentKind is what a paste is - the spec accepts only HTML and
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

// MaxPasteBytes is the universal per-paste size cap, measured in
// COMPRESSED bytes (post-zstd, as written to the blob store). Equals
// the per-identity quota (UserQuotaBytes) - there's only ever one
// number to reason about: an identity has 10 MiB of stored content
// total. Highly redundant text (typical HTML/Markdown) compresses
// 5–10× so users can upload ~50–100 MiB of raw text under this cap.
const MaxPasteBytes = 10 << 20 // 10 MiB

// UserQuotaBytes is the cap on the total compressed size of an
// identity's active pastes (counting every non-deleted version).
// "Identity" is the ssh key fingerprint for keyed uploads or the
// client IP subnet for anonymous ones; either way, the same cap.
const UserQuotaBytes = 10 << 20 // 10 MiB

// HardRawByteCap is the hard fast-fail cap on RAW input bytes - the
// server stops reading after this many uncompressed bytes regardless
// of how well they'd compress. Bounds the upload read so an attacker
// can't stream an arbitrarily large payload to discover its
// compression ratio. Generous enough that no legitimate text payload
// hits it; tight enough to keep request memory bounded.
const HardRawByteCap = 100 << 20 // 100 MiB

// DetectKind classifies up to the first 512 bytes of an upload as one
// of the supported ContentKinds, or returns ErrUnsupportedKind.
//
// HTML detection: standard net/http content-type sniffing. Anything
// that comes back as text/html (regardless of charset) is HTML.
//
// Markdown detection: text/plain (no embedded magic bytes) + at least
// one common markdown structural cue in the first 1 KB. Pure plain
// text gets rejected - markdown without any structure is just text,
// and unrendered text isn't a hostthis use case.
//
// The hint argument is an optional explicit content-type the caller
// supplies (e.g. from a `--type` flag). A hint biases the classifier
// but does NOT bypass the textual-content check: a hint of `html`
// applied to binary bytes (zip, image, etc.) still rejects. This
// prevents a user from labelling a binary as HTML to smuggle it
// through and have it served with `Content-Type: text/html`.
//
// Pass "" to skip the hint and rely purely on sniffing.
func DetectKind(b []byte, hint string) (ContentKind, error) {
	hint = strings.ToLower(strings.TrimSpace(hint))
	sniff := b
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ct := http.DetectContentType(sniff)

	// Hint path: trust the user's labelling for *which* renderer to
	// use, but require the bytes to sniff as some flavor of text. If
	// they sniff as binary (image/zip/audio/etc.) we reject even if
	// the hint says "html".
	switch {
	case hint == "html" || strings.HasPrefix(hint, "text/html"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindHTML, nil
	case hint == "md" || hint == "markdown" || strings.HasPrefix(hint, "text/markdown"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindMarkdown, nil
	case hint != "":
		// Hint we don't understand → reject without trying sniffing.
		return "", ErrUnsupportedKind
	}

	// No hint - pure sniffing.
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
// modest - false positives let a plain-text file render as markdown, which
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
