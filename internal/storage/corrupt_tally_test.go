package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// UNTAGGED on purpose. The reconcilers that use corruptTally are behind the
// slatedb build tag, and CI runs `go test ./...` with no tags, so a tagged pin
// would never execute in CI - which is how the original defect survived: the
// behaviour looked fine because nothing was measuring the thing that was wrong.

// A clean pass must be SILENT. If the summary printed unconditionally, the log
// would carry a line on every reconcile tick forever, which buries the
// transition from clean to not - the only thing the line exists to reveal.
func TestCorruptTally_CleanPassIsSilent(t *testing.T) {
	var tally corruptTally
	if line, ok := tally.summary("pastes"); ok {
		t.Fatalf("a pass with no corrupt rows must not log; got %q", line)
	}
}

// Either class alone must still speak up. Guards against a summary gated on
// both counters being non-zero, which would hide a pass that found only
// unrepairable rows - the more serious of the two, since those stay invisible
// to the owner's quota scan.
func TestCorruptTally_EitherClassAloneStillReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		note func(*corruptTally)
	}{
		{"unrepairable only", func(tl *corruptTally) { tl.noteUnrepairable("a") }},
		{"placeholder only", func(tl *corruptTally) { tl.notePlaceholder("a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tally corruptTally
			tc.note(&tally)
			if _, ok := tally.summary("pastes"); !ok {
				t.Fatalf("%s must still produce a summary line", tc.name)
			}
		})
	}
}

// The counts must be ACCURATE and the two classes must not bleed into each
// other. The whole point of summarising is that the number replaces the
// per-row detail, so a wrong number is worse than no number: it would read as
// authoritative while misstating how much debris exists.
func TestCorruptTally_CountsAreAccurateAndClassesStaySeparate(t *testing.T) {
	var tally corruptTally
	for range 7 {
		tally.noteUnrepairable("u")
	}
	for range 3 {
		tally.notePlaceholder("p")
	}
	if tally.unrepairable != 7 {
		t.Fatalf("unrepairable count: want 7, got %d", tally.unrepairable)
	}
	if tally.placeholder != 3 {
		t.Fatalf("placeholder count: want 3, got %d", tally.placeholder)
	}
	line, _ := tally.summary("pastes")
	if !strings.Contains(line, "7 unrepairable") {
		t.Fatalf("summary must state the unrepairable count; got %q", line)
	}
	if !strings.Contains(line, "3 projected") {
		t.Fatalf("summary must state the placeholder count; got %q", line)
	}
}

// Samples are bounded. They exist so a reader can go inspect one real row; if
// they were unbounded the summary line itself would grow with the debris and
// reintroduce the cost this whole change removes, just on one line instead of
// many.
func TestCorruptTally_SamplesAreBounded(t *testing.T) {
	var tally corruptTally
	for range 500 {
		tally.noteUnrepairable("slug")
		tally.notePlaceholder("slug")
	}
	if got := len(tally.unrepairableSample); got != corruptSampleLimit {
		t.Fatalf("unrepairable sample must cap at %d, got %d", corruptSampleLimit, got)
	}
	if got := len(tally.placeholderSample); got != corruptSampleLimit {
		t.Fatalf("placeholder sample must cap at %d, got %d", corruptSampleLimit, got)
	}
	// The line must not grow with row count either - the samples are the only
	// part that could, so pin the whole rendered length as a byte budget.
	line, _ := tally.summary("pastes")
	if len(line) > 600 {
		t.Fatalf("summary line is %d bytes; it must stay bounded regardless of debris volume", len(line))
	}
}

// The scan name must reach the line. Both reconcilers share this type, so
// without it a pastes summary and a sites summary are indistinguishable in the
// log and an operator cannot tell which index is accumulating debris.
func TestCorruptTally_SummaryNamesTheScan(t *testing.T) {
	for _, scan := range []string{"pastes", "sites"} {
		var tally corruptTally
		tally.noteUnrepairable("a")
		line, _ := tally.summary(scan)
		if !strings.Contains(line, scan) {
			t.Fatalf("summary must name the scan %q so the debris is attributable; got %q", scan, line)
		}
	}
}

// THE property, pinned where it actually lives: neither reconciler may log
// inside the loop that walks records. Log volume per pass must be O(1) in the
// number of corrupt rows, not O(N).
//
// This is a source-level guard because the property is structural and the
// reconcilers it constrains are behind the slatedb tag, needing a live cluster
// and a real object store to drive. A behavioural version of this test would
// therefore not run in CI, which is exactly how the original defect survived
// review: it was a pure cost regression with no behavioural symptom, so every
// functional check stayed green while a read went from 0.45s to 19s. Precedent
// for inspecting the package graph rather than the runtime lives in
// layering_test.go.
//
// A tally-level test cannot substitute. summary() returns one string by
// construction, so asserting it has one line is vacuous; the regression is a
// Printf being reasonably re-added next to a note() call, and only the
// reconciler source shows that.
func TestReconcilersDoNotLogPerRecord(t *testing.T) {
	for _, file := range []string{"shale_reconcile.go", "shale_site_repo.go"} {
		t.Run(file, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}

			calls := func(n ast.Node, name string) []token.Pos {
				var out []token.Pos
				ast.Inspect(n, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
						out = append(out, sel.Pos())
					}
					return true
				})
				return out
			}

			// Find every record-walking loop: one that tallies a corrupt row.
			// Anchoring on the tally calls rather than on a line range keeps the
			// guard correct as the surrounding code moves.
			var checked int
			ast.Inspect(f, func(n ast.Node) bool {
				body, ok := n.(*ast.RangeStmt)
				if !ok {
					return true
				}
				tallies := append(calls(body, "noteUnrepairable"), calls(body, "notePlaceholder")...)
				if len(tallies) == 0 {
					return true
				}
				checked++
				if logs := calls(body, "repoLog"); len(logs) > 0 {
					t.Errorf("%s: the record loop at %s logs via repoLog() at %s. A corrupt row is corrupt on "+
						"EVERY pass and the unrepairable class can never be repaired by a retry, so a log call here "+
						"costs one line per row per pass forever: observed at 19,764 rows producing ~60 lines/sec and "+
						"~1.5 CPU cores, starving the request path (a read went 0.45s -> 19s) while every behavioural "+
						"check stayed green. Tally it and summarise once per pass instead - see corruptTally.",
						file, fset.Position(body.Pos()), fset.Position(logs[0]))
				}
				return true
			})

			// Without this the test passes vacuously the moment the loop is
			// refactored into a shape the walk above stops recognising - the same
			// class of silent-pass bug this whole change exists to fix.
			if checked == 0 {
				t.Fatalf("%s: found no record loop containing a corruptTally call, so this guard checked NOTHING. "+
					"Either the corrupt-row handling moved or it stopped tallying; re-point the guard rather than "+
					"leaving it green.", file)
			}
		})
	}
}
