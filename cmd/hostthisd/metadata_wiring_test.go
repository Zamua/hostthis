package main

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

// Every metadata bundle serves directories through the paste adapter.
// Wiring the site surface directly to a repository compiles and serves, so the
// guard makes the substitution visible.
func TestEveryMetadataBundleWiresTheArtifactSiteAdapter(t *testing.T) {
	files, err := filepath.Glob("metadata*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var builders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, w := range scanBundleWiring(fset, f) {
			builders = append(builders, file+":"+w.builder)
			if !w.sitesIsAdapter {
				t.Errorf("%s:%d %s wires Sites to %s, not %s.",
					file, w.line, w.builder, describeWiring(w.sites), sitesCtor)
			}
		}
	}
	// Without this the guard passes vacuously the moment the bundle type is
	// renamed or the builders move: nothing matches, so nothing is required.
	if len(builders) == 0 {
		t.Fatalf("found no function constructing a %s across %d files, so this guard checked "+
			"NOTHING; re-point it rather than leaving it green", bundleType, len(files))
	}
	t.Logf("checked %d bundle builder(s): %s", len(builders), strings.Join(builders, ", "))
}

const (
	// sitesCtor builds the site surface over the unified paste family.
	sitesCtor  = "NewSites"
	bundleType = "metadataBundle"
	sitesField = "Sites"
)

type bundleWiring struct {
	builder string
	line    int
	// sites is the wiring expression in source form, for the failure message;
	// empty means the field was never wired at all.
	sites          string
	sitesIsAdapter bool
}

// scanBundleWiring reports how each bundle builder in f wires its site surface.
//
// AST rather than text: the two fields are wired in two different shapes (a
// composite-literal element and a later field assignment), either can name a
// local or call the constructor inline, and a grep cannot resolve the value
// bound to a same-named local.
func scanBundleWiring(fset *token.FileSet, f *ast.File) []bundleWiring {
	var out []bundleWiring
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		// A builder that constructs no bundle is a stub for the other build
		// tag: it wires nothing, so there is nothing to require of it.
		if !ok || fn.Body == nil || !buildsBundle(fn.Body) {
			continue
		}
		w := bundleWiring{builder: fn.Name.Name, line: fset.Position(fn.Pos()).Line}

		// Locals bound to the adapter, collected first so a field wired from
		// one is recognised wherever the assignment sits.
		adapters := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			if id, ok := as.Lhs[0].(*ast.Ident); ok && callsSitesCtor(as.Rhs[0]) {
				adapters[id.Name] = true
			}
			return true
		})

		record := func(field string, val ast.Expr) {
			switch field {
			case sitesField:
				w.sites, w.sitesIsAdapter = exprSource(fset, val), isSitesAdapter(val, adapters)
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if !isBundleLit(v) {
					return true
				}
				for _, elt := range v.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok {
						record(key.Name, kv.Value)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range v.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || i >= len(v.Rhs) {
						continue
					}
					record(sel.Sel.Name, v.Rhs[i])
				}
			}
			return true
		})
		out = append(out, w)
	}
	return out
}

func buildsBundle(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && isBundleLit(lit) {
			found = true
		}
		return !found
	})
	return found
}

func isBundleLit(lit *ast.CompositeLit) bool {
	id, ok := lit.Type.(*ast.Ident)
	return ok && id.Name == bundleType
}

// isSitesAdapter reports whether e is the paste site adapter: the
// constructor called inline, or a local bound to it.
func isSitesAdapter(e ast.Expr, adapters map[string]bool) bool {
	if callsSitesCtor(e) {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && adapters[id.Name]
}

func callsSitesCtor(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == sitesCtor
	case *ast.SelectorExpr:
		return fn.Sel.Name == sitesCtor
	}
	return false
}

func describeWiring(src string) string {
	if src == "" {
		return "nothing"
	}
	return src
}

func exprSource(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable expression>"
	}
	return b.String()
}

// The detector itself, over the wiring shapes a builder can take. Applied only
// to already-correct files it can report nothing about what it fails to
// recognise.
func TestScanBundleWiring(t *testing.T) {
	for _, tc := range []struct {
		name         string
		src          string
		wantBuilders int
		wantSites    bool
	}{
		{
			name: "composite-literal wiring",
			src: `package main
func buildMetadataLocal() (*metadataBundle, error) {
	sites := storage.NewSites(repo)
	return &metadataBundle{Sites: sites}, nil
}`,
			wantBuilders: 1, wantSites: true,
		},
		{
			name: "field assigned after construction",
			src: `package main
func buildMetadataCelld() (*metadataBundle, error) {
	bundle := &metadataBundle{}
	bundle.Sites = storage.NewSites(repo)
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true,
		},
		{
			name: "constructor called inline",
			src: `package main
func buildMetadataCelld() (*metadataBundle, error) {
	bundle := &metadataBundle{Sites: storage.NewSites(repo)}
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true,
		},
		{
			name: "repository wired directly as the site surface",
			src: `package main
func buildMetadataCelld() (*metadataBundle, error) {
	sites := storage.NewMemRepo()
	bundle := &metadataBundle{Sites: sites}
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: false,
		},
		{
			name: "stub constructs no bundle",
			src: `package main
func buildMetadataCelld() (*metadataBundle, error) {
	return nil, errDisabled
}`,
			wantBuilders: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := scanBundleWiring(fset, f)
			if len(got) != tc.wantBuilders {
				t.Fatalf("builders = %d %+v, want %d", len(got), got, tc.wantBuilders)
			}
			if tc.wantBuilders == 0 {
				return
			}
			if got[0].sitesIsAdapter != tc.wantSites {
				t.Errorf("sitesIsAdapter = %v (wired to %q), want %v",
					got[0].sitesIsAdapter, got[0].sites, tc.wantSites)
			}
		})
	}
}
