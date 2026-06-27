package domain

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// ContentKind is what a paste is - the spec accepts only HTML and
// Markdown in v1. Anything else is rejected at the upload boundary so
// the sandbox + renderer never see surprising bytes.
type ContentKind string

const (
	KindHTML     ContentKind = "html"
	KindMarkdown ContentKind = "markdown"
	// KindDiff is a unified diff (git diff / diff -u output). It is
	// detected conservatively by a real hunk header in the upload prefix
	// (see looksLikeDiff) and served, like Markdown, as a fixed
	// content-independent shell that renders the raw bytes client-side
	// (diff2html + highlight.js). No server-side diffing.
	KindDiff ContentKind = "diff"
	// KindSite is a gzip-tar archive of a static site (HTML/CSS/JS). It
	// is detected by the gzip magic in the upload prefix; the archive's
	// contents (a tar inside, and at least one piece of web content) are
	// confirmed by the safe-untar, not by the format gate. A site is
	// served as a directory off its slug, not as a single rendered file.
	KindSite ContentKind = "site"
)

// ErrUnsupportedKind is returned when content sniffs to something
// outside the v1 accepted set. The error message is what the user
// sees on stderr.
var ErrUnsupportedKind = errors.New(
	"hostthis only accepts content that needs rendering (html, markdown, diff)")

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

	// Archive branch: a gzip magic prefix routes to the static-site path.
	// This is by content, never by filename (the SSH pipe carries no
	// filename), matching how every other format is recognized. The
	// hint, when given, must not be a text hint - a user can't relabel a
	// gzip stream as HTML to smuggle it through, just as the textual
	// branches reject binary bytes under a text hint. We accept gzip
	// detection only on no hint or an explicit archive hint; the actual
	// "is there a tar inside, and does it hold web content" check happens
	// in the safe-untar, not here.
	if HasGzipMagic(b) && (hint == "" || hint == "tgz" || hint == "tar.gz" ||
		strings.HasPrefix(hint, "application/gzip") || strings.HasPrefix(hint, "application/x-gzip")) {
		return KindSite, nil
	}

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
	case hint == "diff" || hint == "patch" || strings.HasPrefix(hint, "text/x-diff") || strings.HasPrefix(hint, "text/x-patch"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindDiff, nil
	case hint != "":
		// Hint we don't understand → reject without trying sniffing.
		return "", ErrUnsupportedKind
	}

	// No hint - pure sniffing.
	switch {
	case strings.HasPrefix(ct, "text/html"):
		return KindHTML, nil
	case strings.HasPrefix(ct, "text/plain"):
		// Diff detection runs BEFORE the markdown fallback: a unified diff
		// is plain text that a conservative hunk-header check identifies
		// precisely, so a real diff renders as a diff while ordinary prose
		// (which never carries a hunk header) falls through to markdown.
		if looksLikeDiff(b) {
			return KindDiff, nil
		}
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

// hunkHeaderRe matches a unified-diff hunk header: "@@ -<n>[,<n>] +<n>[,<n>] @@".
// The line counts after the comma are optional (a single-line hunk omits
// them). A trailing section heading after the closing "@@" is allowed and
// not matched. This is the load-bearing signal for diff detection - it's
// specific enough that ordinary text never produces it by accident.
var hunkHeaderRe = regexp.MustCompile(`@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@`)

// looksLikeDiff reports whether the input is a unified diff. Detection is
// deliberately conservative: the scanned prefix must contain at least one
// real hunk header (hunkHeaderRe). The `diff --git`, `--- ` / `+++ `, and
// `Index:` markers that accompany a diff are NOT sufficient on their own
// and are not even required - the hunk header alone gates, so a paste that
// merely contains `+`/`-` lines (prose, source code, a markdown list) is
// never mis-detected. A false positive renders normal text through
// diff2html (which looks broken); a false negative just falls through to
// the markdown/HTML path, so we bias toward requiring the strong signal.
//
// The scan is bounded to the first 1 KB, matching looksLikeMarkdown (in
// practice the caller already passes only a ~512-byte upload prefix). One
// accepted consequence of the hunk header gating anywhere in the prefix, and
// of diff running before markdown: a Markdown doc that opens with a fenced
// ```diff block detects as `diff`. That's a deliberate trade (the hunk
// header is the spec'd gate, per docs/SPEC.md); `--type markdown` forces the
// markdown renderer when that's not what you want.
func looksLikeDiff(b []byte) bool {
	if len(b) > 1024 {
		b = b[:1024]
	}
	return hunkHeaderRe.Match(b)
}
