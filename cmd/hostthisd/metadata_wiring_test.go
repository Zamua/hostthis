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

// Every metadata bundle serves directories through the artifact adapter, and
// arms the legacy drain with that same value.
//
// Untagged so CI runs it: the shale bundle is behind the slatedb tag, which no
// CI job builds. The failure it guards has no symptom anywhere else. Handing
// the bundle the bare legacy repo compiles, boots, serves every request and
// passes every other test - the only difference is that no directory ever
// migrates, and the drain says nothing about it, because a nil sweeper is
// skipped silently at both call sites.
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
			if !w.sitesIsArtifact {
				t.Errorf("%s:%d %s wires Sites to %s, not %s.\n"+
					"The bare legacy repo serves correctly, so nothing fails - it just never migrates.",
					file, w.line, w.builder, describeWiring(w.sites), artifactSitesCtor)
			}
			if !w.sweeperIsArtifact {
				t.Errorf("%s:%d %s wires %s to %s, not the %s value.\n"+
					"A nil or mismatched sweeper is skipped without a log at both call sites, so the "+
					"migration reports nothing and looks like it found nothing to do.",
					file, w.line, w.builder, sweeperField, describeWiring(w.sweeper), artifactSitesCtor)
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
	// artifactSitesCtor builds the site surface that reads and writes the
	// artifact families, falling back to the legacy one it drains.
	artifactSitesCtor = "NewArtifactSites"
	bundleType        = "metadataBundle"
	sitesField        = "Sites"
	sweeperField      = "LegacySiteSweeper"
)

type bundleWiring struct {
	builder string
	line    int
	// sites / sweeper are the wiring expressions in source form, for the
	// failure message; empty means the field was never wired at all.
	sites             string
	sweeper           string
	sitesIsArtifact   bool
	sweeperIsArtifact bool
}

// scanBundleWiring reports how each bundle builder in f wires its site surface.
//
// AST rather than text: the two fields are wired in two different shapes (a
// composite-literal element and a later field assignment), either can name a
// local or call the constructor inline, and a grep cannot tell the value bound
// to the artifact adapter from a same-named local bound to the legacy repo -
// which is the substitution that matters.
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
			if id, ok := as.Lhs[0].(*ast.Ident); ok && callsArtifactSitesCtor(as.Rhs[0]) {
				adapters[id.Name] = true
			}
			return true
		})

		record := func(field string, val ast.Expr) {
			switch field {
			case sitesField:
				w.sites, w.sitesIsArtifact = exprSource(fset, val), isArtifactAdapter(val, adapters)
			case sweeperField:
				w.sweeper, w.sweeperIsArtifact = exprSource(fset, val), isArtifactAdapter(val, adapters)
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

// isArtifactAdapter reports whether e is the artifact site adapter: the
// constructor called inline, or a local bound to it.
func isArtifactAdapter(e ast.Expr, adapters map[string]bool) bool {
	if callsArtifactSitesCtor(e) {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && adapters[id.Name]
}

func callsArtifactSitesCtor(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == artifactSitesCtor
	case *ast.SelectorExpr:
		return fn.Sel.Name == artifactSitesCtor
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
		name                 string
		src                  string
		wantBuilders         int
		wantSites, wantSweep bool
	}{
		{
			name: "composite-literal wiring",
			src: `package main
func buildMetadataLocal() (*metadataBundle, error) {
	sites := storage.NewArtifactSites(repo, storage.NewShaleSiteRepo(repo))
	return &metadataBundle{Sites: sites, LegacySiteSweeper: sites}, nil
}`,
			wantBuilders: 1, wantSites: true, wantSweep: true,
		},
		{
			name: "field assigned after construction",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	sites := storage.NewArtifactSites(repo, storage.NewShaleSiteRepo(repo))
	bundle := &metadataBundle{Sites: sites}
	bundle.LegacySiteSweeper = sites
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true, wantSweep: true,
		},
		{
			name: "constructor called inline",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	bundle := &metadataBundle{Sites: storage.NewArtifactSites(repo, legacy)}
	bundle.LegacySiteSweeper = storage.NewArtifactSites(repo, legacy)
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true, wantSweep: true,
		},
		{
			name: "legacy repo wired as the site surface",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	sites := storage.NewShaleSiteRepo(repo)
	bundle := &metadataBundle{Sites: sites}
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: false, wantSweep: false,
		},
		{
			name: "adapter built but the drain left unarmed",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	sites := storage.NewArtifactSites(repo, storage.NewShaleSiteRepo(repo))
	bundle := &metadataBundle{Sites: sites}
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true, wantSweep: false,
		},
		{
			name: "drain armed with something other than the adapter",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	sites := storage.NewArtifactSites(repo, legacy)
	bundle := &metadataBundle{Sites: sites}
	bundle.LegacySiteSweeper = legacy
	return bundle, nil
}`,
			wantBuilders: 1, wantSites: true, wantSweep: false,
		},
		{
			name: "build-tag stub constructs no bundle",
			src: `package main
func buildMetadataShale() (*metadataBundle, error) {
	return nil, fmt.Errorf("requires -tags slatedb")
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
			if got[0].sitesIsArtifact != tc.wantSites {
				t.Errorf("sitesIsArtifact = %v (wired to %q), want %v",
					got[0].sitesIsArtifact, got[0].sites, tc.wantSites)
			}
			if got[0].sweeperIsArtifact != tc.wantSweep {
				t.Errorf("sweeperIsArtifact = %v (wired to %q), want %v",
					got[0].sweeperIsArtifact, got[0].sweeper, tc.wantSweep)
			}
		})
	}
}
