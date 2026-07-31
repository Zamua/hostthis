package storage

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The quota decision belongs to the domain; adapters supply the inputs.
//
// Computing how many bytes an identity occupies is a query and stays here.
// Deciding whether that total admits a write is a rule and does not.
//
// Untagged so CI runs it: most of these files are behind the slatedb tag, and
// the failure has no symptom in any single adapter. It is the adapters
// disagreeing with one another.
func TestQuotaDecisionIsNotOpenCodedInAdapters(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var scanned, capFuncs int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		scanned++
		rep := scanQuotaDelegation(fset, f)
		capFuncs += len(rep.capFuncs)

		for _, s := range rep.openCoded {
			t.Errorf("%s:%d compares against the cap directly:\n\t%s\n"+
				"The quota rule belongs to domain.Allowance - use Admit for a plain write or "+
				"AdmitReplacing when a record's bytes are being displaced (which credits the old "+
				"size, the case the open-coded copies disagreed about). Computing `used` stays here; "+
				"deciding does not.", file, s.line, s.detail)
		}
		// Positive control, per function rather than per package: a package-wide
		// count of delegations cannot tell "moved into the domain" from "dropped
		// altogether in one adapter while the others still delegate".
		for _, s := range rep.undelegated {
			t.Errorf("%s:%d %s takes a %s but neither builds a domain.Allowance nor forwards the cap "+
				"onward. Enforcement has gone missing from this path rather than moved into the domain.",
				file, s.line, s.detail, quotaCapParam)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test files; this guard checked nothing")
	}
	// Without this the guard passes vacuously the moment the cap parameter is
	// renamed: no function carries it, so nothing is required to delegate.
	if capFuncs == 0 {
		t.Fatalf("found no function taking a %s parameter across %d files, so this guard checked "+
			"NOTHING; re-point it rather than leaving it green", quotaCapParam, scanned)
	}
	t.Logf("scanned %d files, %d quota-bearing functions", scanned, capFuncs)
}

// quotaCapParam is the per-identity cap an adapter is handed. The rule it feeds
// belongs to domain.Allowance.
const quotaCapParam = "userCap"

type quotaSite struct {
	line   int
	detail string
}

type quotaReport struct {
	// openCoded: the cap compared against something other than the
	// is-a-cap-configured short-circuit.
	openCoded []quotaSite
	// capFuncs: every function declaration taking the cap.
	capFuncs []quotaSite
	// undelegated: the subset of capFuncs that neither builds a
	// domain.Allowance nor passes the cap to another call.
	undelegated []quotaSite
}

// scanQuotaDelegation reports how f handles the per-identity cap.
//
// AST rather than text: a line grep for the identifier cannot tell
// `used+body > userCap` (the rule, open-coded) from `if userCap > 0` (a
// short-circuit round a scan), splits on comment syntax rather than on
// structure, and misses a comparison wrapped across lines.
func scanQuotaDelegation(fset *token.FileSet, f *ast.File) quotaReport {
	var rep quotaReport

	ast.Inspect(f, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch bin.Op {
		case token.LSS, token.LEQ, token.GTR, token.GEQ:
		default:
			return true
		}
		x, y := unparen(bin.X), unparen(bin.Y)
		capOn := isIdent(x, quotaCapParam) || isIdent(y, quotaCapParam)
		if !capOn {
			return true
		}
		// ...except asking whether a cap is configured at all, which
		// short-circuits an expensive scan rather than applying the rule.
		if isZeroLit(x) || isZeroLit(y) {
			return true
		}
		rep.openCoded = append(rep.openCoded, quotaSite{
			line:   fset.Position(bin.Pos()).Line,
			detail: exprString(fset, bin),
		})
		return true
	})

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !takesParam(fn, quotaCapParam) {
			continue
		}
		site := quotaSite{line: fset.Position(fn.Pos()).Line, detail: fn.Name.Name}
		rep.capFuncs = append(rep.capFuncs, site)
		if !delegatesCap(fn.Body) {
			rep.undelegated = append(rep.undelegated, site)
		}
	}
	return rep
}

// delegatesCap reports whether body hands the quota decision on: either it
// builds the domain value that owns the rule, or it forwards the cap to
// something that does.
func delegatesCap(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.CompositeLit:
			if sel, ok := unparen(v.Type).(*ast.SelectorExpr); ok &&
				isIdent(sel.X, "domain") && sel.Sel.Name == "Allowance" {
				found = true
			}
		case *ast.CallExpr:
			for _, arg := range v.Args {
				if isIdent(unparen(arg), quotaCapParam) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func takesParam(fn *ast.FuncDecl, name string) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		for _, id := range field.Names {
			if id.Name == name {
				return true
			}
		}
	}
	return false
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := unparen(e).(*ast.Ident)
	return ok && id.Name == name
}

func isZeroLit(e ast.Expr) bool {
	lit, ok := unparen(e).(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable comparison>"
	}
	return b.String()
}

// The detector itself, over the shapes an adapter can take. Applied only to
// already-correct files it can report nothing about what it fails to recognise.
func TestScanQuotaDelegation(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		src                      string
		wantOpen, wantFuncs, und int
	}{
		{
			name: "open-coded rule",
			src: `package p
func (r *R) InsertWithQuotaCheck(p P, userCap int64) error {
	used := r.sum()
	if used+int64(p.Size) > userCap {
		return ErrOverUserQuota
	}
	return nil
}`,
			wantOpen: 1, wantFuncs: 1, und: 1,
		},
		{
			name: "open-coded rule spread across lines",
			src: `package p
func (r *R) InsertWithQuotaCheck(p P, userCap int64) error {
	if used+
		int64(p.Size) >= userCap {
		return ErrOverUserQuota
	}
	return (domain.Allowance{Cap: userCap}).Admit(1)
}`,
			wantOpen: 1, wantFuncs: 1, und: 0,
		},
		{
			name: "config short-circuit is not the rule",
			src: `package p
func (r *R) InsertWithQuotaCheck(p P, userCap int64) error {
	if userCap > 0 {
		return (domain.Allowance{Cap: userCap, Used: r.sum()}).Admit(p.Size)
	}
	return nil
}`,
			wantOpen: 0, wantFuncs: 1, und: 0,
		},
		{
			name: "thin wrapper forwarding the cap",
			src: `package p
func (s *S) InsertWithQuotaCheck(site Site, userCap int64) error {
	return s.repo.InsertSiteWithQuotaCheck(site, userCap)
}`,
			wantOpen: 0, wantFuncs: 1, und: 0,
		},
		{
			name: "enforcement dropped altogether",
			src: `package p
func (r *R) InsertWithQuotaCheck(p P, userCap int64) error {
	return r.insert(p)
}`,
			wantOpen: 0, wantFuncs: 1, und: 1,
		},
		{
			name: "no cap parameter at all",
			src: `package p
func (r *R) Insert(p P) error { return r.insert(p) }`,
			wantOpen: 0, wantFuncs: 0, und: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			rep := scanQuotaDelegation(fset, f)
			if len(rep.openCoded) != tc.wantOpen {
				t.Fatalf("openCoded = %d %+v, want %d", len(rep.openCoded), rep.openCoded, tc.wantOpen)
			}
			if len(rep.capFuncs) != tc.wantFuncs {
				t.Fatalf("capFuncs = %d %+v, want %d", len(rep.capFuncs), rep.capFuncs, tc.wantFuncs)
			}
			if len(rep.undelegated) != tc.und {
				t.Fatalf("undelegated = %d %+v, want %d", len(rep.undelegated), rep.undelegated, tc.und)
			}
		})
	}
}
