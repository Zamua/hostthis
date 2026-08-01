package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ContentKind is what a paste is. Anything outside this set is rejected at the
// upload boundary, so the sandbox and renderer never see surprising bytes.
type ContentKind string

const (
	KindHTML     ContentKind = "html"
	KindMarkdown ContentKind = "markdown"
	// KindDiff is a unified diff, detected conservatively by a real hunk
	// header in the upload prefix (looksLikeDiff) and served like Markdown:
	// a content-independent shell that renders the raw bytes client-side. No
	// server-side diffing.
	KindDiff ContentKind = "diff"
	// KindSite is a gzip-tar archive of a static site, detected by the gzip
	// magic in the upload prefix. Whether a tar with web content is actually
	// inside is confirmed by the safe-untar, not by the format gate. Served
	// as a directory off its slug, not as one rendered file.
	KindSite ContentKind = "site"
	// KindMermaid is Mermaid diagram source, detected by an opening diagram
	// keyword. Mermaid also renders inside a markdown paste's fenced blocks;
	// this kind is for a bare diagram uploaded on its own.
	KindMermaid ContentKind = "mermaid"
	// KindCSV is delimiter-separated tabular text (comma or tab), detected by
	// a consistent delimiter count across the prefix's lines.
	KindCSV ContentKind = "csv"
	// KindJSON is a JSON value or a JSONL stream, detected by parsing the
	// prefix rather than by punctuation heuristics.
	KindJSON ContentKind = "json"
	// KindFlamegraph is a profile in folded stack format: one line per unique
	// stack, frames joined by ';', then a sample count. Detected by that shape
	// rather than by any profiler's own format, because every profiler can
	// emit it and none of them agree on anything else.
	KindFlamegraph ContentKind = "flamegraph"
	// KindPDF is a PDF document, detected by the %PDF- signature. The only
	// accepted kind whose bytes are not text; it clears the same explicit-magic
	// bar KindSite does rather than being a fallback for unclassifiable bytes.
	KindPDF ContentKind = "pdf"
)

// ErrUnsupportedKind is returned when content sniffs outside the accepted set.
// The message is what the user sees on stderr.
var ErrUnsupportedKind = errors.New(
	"hostthis only accepts content it can render (html, markdown, diff, mermaid, pdf, csv, json, flamegraph)")

// MaxPasteBytes is the per-paste size cap, measured in COMPRESSED bytes
// (post-zstd, as written to the blob store). A single upload is staged in RAM
// before it is written, so this is what stops one request exhausting a small
// node. Typical HTML/Markdown compresses 5-10x, so ~50-100 MiB of raw text
// fits under it.
const MaxPasteBytes = 10 << 20 // 10 MiB

// UserQuotaBytes caps the total compressed size of an identity's active pastes,
// counting every non-deleted version. "Identity" is the ssh key fingerprint for
// keyed uploads or the client IP subnet for anonymous ones.
//
// Deliberately larger than MaxPasteBytes: this is a fairness limit on
// accumulated storage that costs nothing at request time, so raising it does
// not imply raising the per-request ceiling.
//
// A var, not a const, so tests can shrink it: driving a total over the real
// limit would otherwise mean compressing 100+ MiB of high-entropy data per
// test. Production never writes to it.
var UserQuotaBytes = 100 << 20 // 100 MiB

// HardRawByteCap fast-fails on RAW input bytes: the server stops reading after
// this many uncompressed bytes regardless of how well they would compress.
// Without it an attacker can stream an arbitrarily large payload to discover
// its compression ratio, and request memory is unbounded.
const HardRawByteCap = 100 << 20 // 100 MiB

// MIMESniffLen is how many leading bytes are enough to classify content. The
// sniffing algorithms this feeds are all defined over a bounded prefix.
const MIMESniffLen = 512

// MIMESniffer reports a media type for a byte prefix, e.g. "text/plain;
// charset=utf-8" or "application/octet-stream".
//
// A PORT, not an implementation: the one obvious implementation lives in
// net/http, a transport package the domain must not depend on. What belongs
// here is the RULE the sniff feeds - content must sniff as some flavour of
// text, so a binary payload is rejected even when labelled "html". That rule is
// the security control that stops a type hint short-circuiting detection; the
// algorithm behind it is not.
//
// Adapters supply http.DetectContentType; a test supplies whatever exercises
// its branch, without having to construct bytes that sniff a particular way.
type MIMESniffer func(b []byte) string

// DetectKind classifies an upload prefix as a ContentKind, or returns
// ErrUnsupportedKind.
//
// HTML is whatever sniffs as text/html. Markdown is text/plain plus at least
// one structural cue: unstructured plain text is rejected, since unrendered
// text is not a hostthis use case.
//
// hint is an optional caller-supplied content type (e.g. a `--type` flag), ""
// to rely purely on sniffing. It biases the classifier but never bypasses the
// textual-content check, so a binary cannot be labelled `html` to smuggle it
// through and have it served as `Content-Type: text/html`.
func DetectKind(b []byte, hint string, sniffMIME MIMESniffer) (ContentKind, error) {
	hint = strings.ToLower(strings.TrimSpace(hint))

	// Binary branches, by explicit format signature: the SSH pipe carries no
	// filename. A text hint disqualifies them, so a gzip or PDF stream cannot
	// be relabelled as HTML, mirroring the textual branches rejecting binary
	// bytes under a text hint. These are magic-gated, never a fallback for
	// bytes that failed to classify.
	//
	// Whether a tar with web content is inside a gzip stream is the
	// safe-untar's question, not this gate's.
	if HasGzipMagic(b) && (hint == "" || hint == "tgz" || hint == "tar.gz" ||
		strings.HasPrefix(hint, "application/gzip") || strings.HasPrefix(hint, "application/x-gzip")) {
		return KindSite, nil
	}
	if HasPDFMagic(b) && (hint == "" || hint == "pdf" || strings.HasPrefix(hint, "application/pdf")) {
		return KindPDF, nil
	}
	// A pdf hint without the signature is a rejection, not a relabelling: the
	// viewer would fail on the bytes and the Content-Type would be a lie.
	if hint == "pdf" || strings.HasPrefix(hint, "application/pdf") {
		return "", ErrUnsupportedKind
	}

	sniff := b
	if len(sniff) > MIMESniffLen {
		sniff = sniff[:MIMESniffLen]
	}
	ct := sniffMIME(sniff)

	// Hint path: the label picks the renderer, but the bytes must still
	// sniff as some flavour of text. Binary is rejected even under a
	// "html" hint.
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
	case hint == "mermaid" || hint == "mmd" || strings.HasPrefix(hint, "text/vnd.mermaid"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindMermaid, nil
	case hint == "csv" || hint == "tsv" || strings.HasPrefix(hint, "text/csv") || strings.HasPrefix(hint, "text/tab-separated-values"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindCSV, nil
	case hint == "json" || hint == "jsonl" || hint == "ndjson" || strings.HasPrefix(hint, "application/json"):
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindJSON, nil
	case hint == "flamegraph" || hint == "flame" || hint == "folded":
		if !strings.HasPrefix(ct, "text/") {
			return "", ErrUnsupportedKind
		}
		return KindFlamegraph, nil
	case hint != "":
		// An unrecognized hint rejects without falling back to sniffing.
		return "", ErrUnsupportedKind
	}

	// No hint - pure sniffing. Ordered precision-first, because the cheap
	// checks are the imprecise ones: each gate below is specific enough that
	// ordinary prose never trips it, and markdown (the loosest, satisfied by a
	// single structural cue) must therefore run last.
	switch {
	case strings.HasPrefix(ct, "text/html"):
		return KindHTML, nil
	case strings.HasPrefix(ct, "text/plain"):
		if looksLikeDiff(b) {
			return KindDiff, nil
		}
		if looksLikeMermaid(b) {
			return KindMermaid, nil
		}
		if looksLikeFolded(b) {
			return KindFlamegraph, nil
		}
		if looksLikeJSON(b) {
			return KindJSON, nil
		}
		if looksLikeCSV(b) {
			return KindCSV, nil
		}
		if looksLikeMarkdown(b) {
			return KindMarkdown, nil
		}
		return "", ErrUnsupportedKind
	default:
		return "", ErrUnsupportedKind
	}
}

// pdfMagic opens every PDF document. The version digits that follow are not
// checked: a viewer that cannot read the version will say so, and rejecting on
// it here would turn an unusual-but-valid file into "unsupported type".
var pdfMagic = []byte("%PDF-")

// HasPDFMagic reports whether the prefix opens with the PDF signature.
func HasPDFMagic(b []byte) bool {
	return len(b) >= len(pdfMagic) && string(b[:len(pdfMagic)]) == string(pdfMagic)
}

// mermaidOpeners are the diagram keywords Mermaid accepts on the opening line.
// Matching the opener is what keeps this gate off prose: no English sentence
// begins with "sequenceDiagram".
var mermaidOpeners = []string{
	"graph ", "graph\n", "flowchart ", "sequenceDiagram", "classDiagram",
	"stateDiagram-v2", "stateDiagram", "erDiagram", "journey", "gantt",
	"pie ", "pie\n", "gitGraph", "mindmap", "timeline", "quadrantChart",
	"requirementDiagram", "C4Context", "sankey-beta", "xychart-beta",
	"block-beta", "packet-beta", "architecture-beta", "kanban", "radar-beta",
}

// looksLikeMermaid reports whether the first non-blank line opens a diagram.
// Mermaid's own front-matter and directive prefixes are skipped first, since
// both legally precede the opener.
func looksLikeMermaid(b []byte) bool {
	s := string(b)
	if len(s) > 1024 {
		s = s[:1024]
	}
	for line := range strings.SplitSeq(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t == "---" || strings.HasPrefix(t, "%%") || strings.HasPrefix(t, "config:") {
			continue
		}
		for _, op := range mermaidOpeners {
			// The opener may be the whole line, so compare against the line
			// plus a newline to let space-suffixed openers match it.
			if strings.HasPrefix(t+"\n", op) {
				return true
			}
		}
		return false
	}
	return false
}

// looksLikeFolded reports whether the content is a folded stack profile:
// every line "frame;frame;frame <count>".
//
// Runs before the CSV gate. A C++ or Rust frame carries commas inside its
// argument list, so a profile of such a binary presents a consistent comma
// count per line and would otherwise sniff as CSV.
//
// The gate is EVERY line, not most: a profile is machine-generated and
// perfectly uniform, so one prose line is enough to prove it is not one. The
// semicolon requirement is what keeps an ordinary numbered list out, since
// "item 1 / item 2" also ends every line in a count.
func looksLikeFolded(b []byte) bool {
	s := string(b)
	truncated := len(s) > 8192
	if truncated {
		s = s[:8192]
	}
	lines := strings.Split(s, "\n")
	// Only a line the WINDOW cut is discarded: its count is chopped and would
	// fail a whole-file gate that is otherwise satisfied. A file that merely
	// ends without a newline is complete, and dropping its last line would
	// push a short profile under the two-line floor.
	if truncated && len(lines) > 1 {
		lines = lines[:len(lines)-1]
	}
	var n, withSep int
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Frames may contain spaces (a C++ signature does), so the count is
		// the LAST field, never the second.
		i := strings.LastIndexAny(line, " \t")
		if i < 0 {
			return false
		}
		stack := strings.TrimRight(line[:i], " \t")
		if !isPositiveInt(line[i+1:]) || stack == "" {
			return false
		}
		n++
		if strings.Contains(stack, ";") {
			withSep++
		}
	}
	// Two lines is the floor: a single "word 12" is not evidence of anything.
	// Half carrying a separator allows the flat top-level frames a real
	// profile also contains without admitting a list that has none.
	return n >= 2 && withSep*2 >= n
}

func isPositiveInt(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksLikeJSON reports whether the content is a JSON value or a JSONL stream.
// Parsing is the gate rather than punctuation: `{` opens a JSON object and also
// a C function body, and only one of those unmarshals.
//
// The prefix may be truncated mid-value, so a failed parse of the whole input
// falls back to the FIRST LINE, which is complete for JSONL and for any
// pretty-printed document whose opening line is a bare bracket.
func looksLikeJSON(b []byte) bool {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return false
	}
	if c := trimmed[0]; c != '{' && c != '[' {
		// A bare scalar is valid JSON but indistinguishable from plain text,
		// and rendering `42` as a tree helps nobody.
		return false
	}
	if json.Valid([]byte(trimmed)) {
		return true
	}
	first, _, _ := strings.Cut(trimmed, "\n")
	first = strings.TrimSpace(first)
	if first == "{" || first == "[" {
		return true // pretty-printed, truncated by the sniff window
	}
	return json.Valid([]byte(first)) // JSONL: one complete value per line
}

// csvDelimiters are the separators looksLikeCSV will consider. Pipe is
// deliberately absent: a markdown table is pipe-separated and consistent, and
// it is markdown.
var csvDelimiters = []rune{',', '\t'}

// looksLikeCSV reports whether the prefix is delimiter-separated tabular text.
//
// The gate is a CONSISTENT field count of at least 3 across at least 3 lines.
// Two-column data is given up deliberately: prose wraps at punctuation, so
// "Hello, world" over two lines is a consistent 2-field table, and a false
// positive renders a paragraph as a spreadsheet. Three fields across three
// lines effectively never occurs in prose, and a real 2-column CSV still
// renders as a table under `--type csv`.
func looksLikeCSV(b []byte) bool {
	s := string(b)
	if len(s) > 4096 {
		s = s[:4096]
	}
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	// The sniff window can truncate the final line mid-field, which would show
	// a short count and fail an otherwise consistent file.
	if len(lines) > 3 && !strings.HasSuffix(s, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 3 {
		return false
	}
	for _, d := range csvDelimiters {
		want := countFields(lines[0], d)
		if want < 3 {
			continue
		}
		consistent := true
		for _, line := range lines[1:] {
			if countFields(line, d) != want {
				consistent = false
				break
			}
		}
		if consistent {
			return true
		}
	}
	return false
}

// countFields counts delimiter-separated fields in one line, honouring RFC 4180
// double-quoted fields so an address like "Springfield, IL" counts as one.
func countFields(line string, delim rune) int {
	fields, inQuotes := 1, false
	for _, r := range line {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == delim && !inQuotes:
			fields++
		}
	}
	return fields
}

// looksLikeMarkdown reports whether the first 1 KB carries a markdown
// structural cue. The cue list is deliberately modest, because the failure
// modes are asymmetric: a false positive renders plain text as markdown
// (harmless, one <p>), a false negative rejects a real markdown doc.
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
// The counts after the comma are optional (a single-line hunk omits them); a
// trailing section heading is allowed and not matched. The load-bearing signal
// for diff detection: specific enough that ordinary text never produces it.
var hunkHeaderRe = regexp.MustCompile(`@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@`)

// looksLikeDiff reports whether the first 1 KB is a unified diff. The hunk
// header alone gates: `diff --git`, `--- ` / `+++ `, and `Index:` are neither
// sufficient nor required, so prose, source, or a markdown list carrying
// `+`/`-` lines is never mis-detected. The bias is deliberate, since a false
// positive renders normal text through diff2html (visibly broken) while a
// false negative just falls through to markdown/HTML.
//
// A hunk header that appears AFTER a markdown code fence is QUOTED, not the
// document's own format: that is a design doc showing a diff, and it must
// render as markdown so its prose renders too. The markdown viewer draws such
// a fence through the same diff renderer, so nothing is lost by classifying it
// as markdown.
//
// The ordering test is what keeps a real diff OF a markdown file working: its
// hunk header comes first and the fence is part of the diffed content.
func looksLikeDiff(b []byte) bool {
	if len(b) > 1024 {
		b = b[:1024]
	}
	loc := hunkHeaderRe.FindIndex(b)
	if loc == nil {
		return false
	}
	fence := bytes.Index(b, []byte("```"))
	return fence < 0 || fence > loc[0]
}
