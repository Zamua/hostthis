package domain

import "testing"

// The detection gates run precision-first, and this pins the ORDER rather than
// the individual checks.
//
// An internal test on purpose: the assertion that matters is that the fixture
// SATISFIES BOTH gates, which is only checkable from inside the package. A
// fixture that trips just one gate proves nothing about their order, and an
// earlier version of this test had exactly that defect - reordering the gates
// left it green.
func TestGateOrder_JSONBeatsCSV(t *testing.T) {
	// JSONL with three keys per line: three lines, three consistent
	// comma-separated fields, so it is a well-formed CSV too.
	body := []byte("{\"a\":1,\"b\":2,\"c\":3}\n{\"a\":4,\"b\":5,\"c\":6}\n{\"a\":7,\"b\":8,\"c\":9}\n")

	if !looksLikeJSON(body) {
		t.Fatal("degenerate fixture: it must satisfy the json gate")
	}
	if !looksLikeCSV(body) {
		t.Fatal("degenerate fixture: it must ALSO satisfy the csv gate, or the " +
			"order of the two gates is not what this test observes")
	}

	got, err := DetectKind(body, "", func(b []byte) string { return "text/plain; charset=utf-8" })
	if err != nil {
		t.Fatalf("DetectKind: %v", err)
	}
	if got != KindJSON {
		t.Fatalf("got %q, want %q: json must be tried before csv, since every "+
			"uniform JSONL stream is also a consistent delimiter-separated table", got, KindJSON)
	}
}

// A hunk header inside a fenced block is a QUOTED diff, so the document is
// markdown - which matters because the markdown viewer renders that fence
// through the diff renderer anyway, while the reverse loses all the prose.
func TestGateOrder_FencedDiffIsMarkdown(t *testing.T) {
	doc := []byte("# Review\n\nThe boundary moved:\n\n```diff\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n```\n")
	if !hunkHeaderRe.Match(doc) {
		t.Fatal("degenerate fixture: it must carry a hunk header, or it proves nothing")
	}
	got, err := DetectKind(doc, "", func([]byte) string { return "text/plain; charset=utf-8" })
	if err != nil {
		t.Fatalf("DetectKind: %v", err)
	}
	if got != KindMarkdown {
		t.Fatalf("got %q, want markdown: a design doc quoting a diff must render as "+
			"markdown, or its prose is served as diff noise", got)
	}
}

// The converse still holds: a real diff OF a markdown file carries fences in
// its own content, after the hunk header, and is still a diff.
func TestGateOrder_DiffOfMarkdownIsStillDiff(t *testing.T) {
	doc := []byte("--- a/README.md\n+++ b/README.md\n@@ -1,3 +1,3 @@\n # Title\n-```js\n+```ts\n")
	got, err := DetectKind(doc, "", func([]byte) string { return "text/plain; charset=utf-8" })
	if err != nil {
		t.Fatalf("DetectKind: %v", err)
	}
	if got != KindDiff {
		t.Fatalf("got %q, want diff: the fence is inside the diffed content, not around it", got)
	}
}
