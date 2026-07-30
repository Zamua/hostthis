package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// UNTAGGED on purpose: the reconcilers that use corruptTally are behind the
// slatedb build tag, and CI runs `go test ./...` with no tags, so a tagged pin
// would never execute.

// A clean pass must be SILENT. If the summary printed unconditionally, the log
// would carry a line on every reconcile tick forever, which buries the
// transition from clean to not - the only thing the line exists to reveal.
func TestCorruptTally_CleanPassIsSilent(t *testing.T) {
	var tally corruptTally
	if line, ok := tally.summary("pastes"); ok {
		t.Fatalf("a pass with no corrupt rows must not log; got %q", line)
	}
}

// Either class alone must report. A summary gated on both being non-zero would
// hide a pass finding only unrepairable rows, the more serious class.
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

// Counts must be accurate and the classes must not bleed together: the number
// replaces the per-row detail, so a wrong one reads as authoritative.
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

// Samples are bounded, or the summary line itself grows with the debris.
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

// The scan name must reach the line: both reconcilers share this type, and an
// operator needs to know which index is accumulating debris.
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

// No log call inside a record loop: log volume per pass must be O(1) in the
// number of corrupt rows.
//
// Source-level because the property is structural and the reconcilers are
// behind the slatedb tag, so a behavioural version would not run in CI. It
// checks EVERY log call in EVERY record loop: a guard that inspects only
// already-fixed code cannot report the absence of the fix.
//
// Transient-failure logs are legitimately per-record, since their volume tracks
// failures rather than debris; those opt out explicitly, which makes the
// exemption reviewable.
func TestReconcilersDoNotLogPerRecord(t *testing.T) {
	const exempt = "transient-failure-log"
	for _, file := range []string{"shale_reconcile.go", "shale_site_repo.go"} {
		t.Run(file, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}

			// Lines carrying (or immediately preceded by) the opt-out marker.
			exemptLines := map[int]bool{}
			for _, cg := range f.Comments {
				if !strings.Contains(cg.Text(), exempt) {
					continue
				}
				end := fset.Position(cg.End()).Line
				// The marker covers its own line and the statement it precedes.
				exemptLines[end] = true
				exemptLines[end+1] = true
			}

			var loops, checked int
			ast.Inspect(f, func(n ast.Node) bool {
				rng, ok := n.(*ast.RangeStmt)
				if !ok {
					return true
				}
				loops++
				ast.Inspect(rng.Body, func(m ast.Node) bool {
					sel, ok := m.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "repoLog" {
						return true
					}
					checked++
					line := fset.Position(sel.Pos()).Line
					if exemptLines[line] {
						return true
					}
					t.Errorf("%s:%d logs once per record inside a loop, with no %q marker. A corrupt or "+
						"otherwise persistently-bad row is bad on EVERY pass, so a log here costs one line per "+
						"row per pass forever: observed at ~20k rows producing ~1,770 lines/sec, starving the "+
						"request path (a read went 0.45s -> 19s) while every behavioural check stayed green. "+
						"Tally it and summarise once per pass (see corruptTally). If this genuinely fires only "+
						"on a TRANSIENT failure, add the %q comment above it saying why.",
						file, line, exempt, exempt)
					return true
				})
				return true
			})

			// Without this the guard silently passes the moment the loops move
			// or are rewritten into a shape the walk stops recognising.
			if loops == 0 {
				t.Fatalf("%s: found no range loops at all, so this guard checked NOTHING; re-point it "+
					"rather than leaving it green", file)
			}
			t.Logf("%s: inspected %d loops, %d repoLog call sites", file, loops, checked)
		})
	}
}
