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
