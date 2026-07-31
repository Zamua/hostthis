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
	for _, file := range []string{"shale_reconcile.go", "shale_site_repo.go"} {
		t.Run(file, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			sites, loops, calls := scanPerRecordLogs(fset, f)
			for _, s := range sites {
				t.Errorf("%s:%d logs once per record inside a loop (%s), with no %q marker. A corrupt or "+
					"otherwise persistently-bad row is bad on EVERY pass, so a log here costs one line per "+
					"row per pass forever: observed at ~20k rows producing ~1,770 lines/sec, starving the "+
					"request path (a read went 0.45s -> 19s) while every behavioural check stayed green. "+
					"Tally it and summarise once per pass (see corruptTally). If this genuinely fires only "+
					"on a TRANSIENT failure, add the %q comment above it saying why.",
					file, s.line, s.call, perRecordLogExemptMarker, perRecordLogExemptMarker)
			}

			// Without this the guard silently passes the moment the loops move
			// or are rewritten into a shape the walk stops recognising.
			if loops == 0 {
				t.Fatalf("%s: found no loops at all, so this guard checked NOTHING; re-point it "+
					"rather than leaving it green", file)
			}
			t.Logf("%s: inspected %d loops, %d logging call sites in loop bodies", file, loops, calls)
		})
	}
}

// perRecordLogExemptMarker opts one call site out of the per-record-log ban.
const perRecordLogExemptMarker = "transient-failure-log"

// loggingMethods are the *log.Logger emit methods. Keying on the METHOD rather
// than on a receiver spelling is what survives a rename: `r.repoLog().Printf`,
// a hoisted `lg := r.repoLog()` then `lg.Printf`, and a package-level
// `log.Printf` all cost the same one line per record, and a matcher that knows
// only the first name reads green on the other two.
var loggingMethods = map[string]bool{"Print": true, "Printf": true, "Println": true}

type perRecordLogSite struct {
	line int
	call string
}

// scanPerRecordLogs returns every unexempted logging call reachable from a loop
// body in f, plus how many loops were walked and how many logging calls sat
// inside one. Both loop forms count: rewriting a `for range` as a counted `for`
// must not silently retire the guard.
func scanPerRecordLogs(fset *token.FileSet, f *ast.File) (sites []perRecordLogSite, loops, calls int) {
	// Lines carrying (or immediately preceded by) the opt-out marker.
	exemptLines := map[int]bool{}
	for _, cg := range f.Comments {
		if !strings.Contains(cg.Text(), perRecordLogExemptMarker) {
			continue
		}
		end := fset.Position(cg.End()).Line
		// The marker covers its own line and the statement it precedes.
		exemptLines[end] = true
		exemptLines[end+1] = true
	}

	seen := map[int]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		var body *ast.BlockStmt
		switch loop := n.(type) {
		case *ast.RangeStmt:
			body = loop.Body
		case *ast.ForStmt:
			body = loop.Body
		default:
			return true
		}
		loops++
		ast.Inspect(body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !loggingMethods[sel.Sel.Name] {
				return true
			}
			calls++
			line := fset.Position(call.Pos()).Line
			if exemptLines[line] || seen[line] {
				return true
			}
			seen[line] = true
			sites = append(sites, perRecordLogSite{line: line, call: sel.Sel.Name})
			return true
		})
		return true
	})
	return sites, loops, calls
}

// The detector itself, over the loop and logger shapes a refactor can produce.
// Applied only to already-correct files it can report nothing about what it
// fails to recognise, which is how a source-level guard goes quietly vacuous.
func TestScanPerRecordLogs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		src       string
		wantSites int
		wantLoops int
	}{
		{
			name: "counted for loop with a hoisted logger",
			src: `package p
func f(items []int, lg logger) {
	for i := 0; i < len(items); i++ {
		lg.Printf("row %d", i)
	}
}`,
			wantSites: 1, wantLoops: 1,
		},
		{
			name: "range loop with a package-level logger",
			src: `package p
func f(items []int) {
	for _, it := range items {
		log.Println(it)
	}
}`,
			wantSites: 1, wantLoops: 1,
		},
		{
			name: "range loop with the repo logger",
			src: `package p
func (r *R) f(items []int) {
	for _, it := range items {
		r.repoLog().Printf("%v", it)
	}
}`,
			wantSites: 1, wantLoops: 1,
		},
		{
			name: "log inside a closure inside a loop still runs per record",
			src: `package p
func (r *R) f(items []int) {
	for _, it := range items {
		func() { r.repoLog().Print(it) }()
	}
}`,
			wantSites: 1, wantLoops: 1,
		},
		{
			name: "exempted call",
			src: `package p
func (r *R) f(items []int) {
	for _, it := range items {
		// transient-failure-log: fires only on a failed write.
		r.repoLog().Printf("%v", it)
	}
}`,
			wantSites: 0, wantLoops: 1,
		},
		{
			name: "summary outside the loop is the wanted shape",
			src: `package p
func (r *R) f(items []int) {
	for _, it := range items {
		tally.note(it)
	}
	r.repoLog().Print(tally.summary())
}`,
			wantSites: 0, wantLoops: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			sites, loops, _ := scanPerRecordLogs(fset, f)
			if len(sites) != tc.wantSites {
				t.Fatalf("sites = %d %+v, want %d", len(sites), sites, tc.wantSites)
			}
			if loops != tc.wantLoops {
				t.Fatalf("loops = %d, want %d", loops, tc.wantLoops)
			}
		})
	}
}
