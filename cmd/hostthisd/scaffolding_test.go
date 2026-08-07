package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaffoldingMarkers are what a change writes on code that exists only to
// produce a fixture or reproduce a fault: it compiles, it works, and it must
// not ship. Writing the note is the right instinct. Nothing but this test
// stands between writing it and merging it anyway.
var scaffoldingMarkers = []string{
	"never merge",
	"do not merge",
	"dont merge",
	"don't merge",
	"throwaway",
}

// Scaffolding does not reach a release.
//
// Go sources only, and deliberately so: this guards code that CHANGES WHAT THE
// BINARY DOES. Scaffolding is at its most dangerous where it is least visible -
// a composition root under a build tag no CI job compiles, swapped to wire a
// fixture, still passing every test because what it broke has no local symptom.
func TestNoScaffoldingMarkersInGoSources(t *testing.T) {
	root := moduleRoot(t)
	self, err := filepath.Abs("scaffolding_test.go")
	if err != nil {
		t.Fatalf("resolve own path: %v", err)
	}
	var scanned int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		// The marker list itself is not a marker.
		if !strings.HasSuffix(path, ".go") || path == self {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		lower := strings.ToLower(string(body))
		rel, _ := filepath.Rel(root, path)
		for _, marker := range scaffoldingMarkers {
			if line, ok := findMarkerLine(lower, string(body), marker); ok {
				t.Errorf("%s:%d carries the scaffolding marker %q.\n"+
					"Code annotated this way exists to build a fixture or reproduce a fault, and "+
					"a release must not carry it. Restore what it displaced before merging.",
					rel, line, marker)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// Without this a walk that silently matches nothing - a changed suffix, an
	// over-eager skip - reports a clean tree it never read.
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s, so this guard checked NOTHING", root)
	}
	t.Logf("scanned %d Go files under %s", scanned, root)
}

// moduleRoot walks up from the test's directory to the one holding go.mod.
//
// Anchored rather than a relative hop: a wrong number of ".." still names a
// real directory with Go files in it, so the walk succeeds, the count is
// non-zero, and the guard reports a clean tree having read a fraction of it.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory; cannot locate the module root")
		}
		dir = parent
	}
}

// findMarkerLine reports the 1-indexed line of marker's first occurrence.
// lower is body lowercased, so the search is case-insensitive while the line
// count is taken over the original bytes.
func findMarkerLine(lower, body, marker string) (int, bool) {
	i := strings.Index(lower, marker)
	if i < 0 {
		return 0, false
	}
	return strings.Count(body[:i], "\n") + 1, true
}

func TestFindMarkerLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		marker   string
		wantLine int
		wantOK   bool
	}{
		{name: "absent", body: "package p\n", marker: "never merge", wantOK: false},
		{
			name:   "case-insensitive on a later line",
			body:   "package p\n\n// THROWAWAY (never merge): wire the legacy repo\nvar x = 1\n",
			marker: "never merge", wantLine: 3, wantOK: true,
		},
		{
			name:   "first line",
			body:   "// do not merge\npackage p\n",
			marker: "do not merge", wantLine: 1, wantOK: true,
		},
		{
			name:   "substring of a longer word still counts",
			body:   "package p\n// a throwaway fixture\n",
			marker: "throwaway", wantLine: 2, wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := findMarkerLine(strings.ToLower(tc.body), tc.body, tc.marker)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && line != tc.wantLine {
				t.Fatalf("line = %d, want %d", line, tc.wantLine)
			}
		})
	}
}
