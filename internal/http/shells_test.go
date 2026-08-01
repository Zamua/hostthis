package http

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// Every shell page renders with its placeholders substituted. An unsubstituted
// __VER__ would make the asset URL literal and 404 every script on the page.
func TestShells_RenderWithoutLeftoverPlaceholders(t *testing.T) {
	if len(shells) == 0 {
		t.Fatal("no shells registered; re-point this guard rather than leaving it green")
	}
	for kind, sh := range shells {
		page := string(sh.html(kind))
		if page == "" {
			t.Errorf("%s: shell.html is empty, so the embed path is wrong", kind)
			continue
		}
		for _, ph := range []string{"__VER__", "__KIND__"} {
			if strings.Contains(page, ph) {
				t.Errorf("%s: %s left unsubstituted in the served page", kind, ph)
			}
		}
	}
}

// assetRef finds every /_hostthis/<name> the shell pages ask the browser to
// fetch.
var assetRef = regexp.MustCompile(`/_hostthis/([A-Za-z0-9._-]+)`)

// Every asset a shell page references must be whitelisted, or it 404s at
// runtime and the viewer silently loses a script.
//
// This is the guard that would have caught a shell referencing an asset it
// forgot to register: the page still serves 200, so nothing else fails.
func TestShells_ReferencedAssetsAreServable(t *testing.T) {
	// Sources are the shell pages AND the first-party scripts they load: a
	// lazily-fetched asset (the markdown shell pulling in mermaid) is named in
	// the JS, not in the HTML, so scanning only the pages misses the reference
	// most likely to be forgotten.
	sources := map[string]string{}
	for kind, sh := range shells {
		sources[string(kind)+":shell.html"] = string(sh.html(kind))
	}
	for name, sh := range assetSource {
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".mjs") {
			continue
		}
		// Vendored libraries reference their own internals; only first-party
		// bootstraps are scanned.
		if strings.Contains(name, ".min.") || strings.Contains(name, "duckdb-") {
			continue
		}
		b, err := sh.fs.ReadFile(sh.dir + "/" + name)
		if err != nil {
			continue
		}
		sources[name] = string(b)
	}

	checked := 0
	for src, body := range sources {
		for _, m := range assetRef.FindAllStringSubmatch(body, -1) {
			name := m[1]
			checked++
			if _, ok := assetSource[name]; !ok {
				t.Errorf("%s references /_hostthis/%s, which no shell whitelists: "+
					"the browser will 404 on it", src, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no asset references found in any source, so this guard checked NOTHING")
	}
	t.Logf("checked %d asset references across %d sources", checked, len(sources))
}

// Every whitelisted asset must actually exist in its shell's embedded FS.
// A typo'd whitelist entry otherwise serves a 404 that looks like a routing bug.
func TestShells_WhitelistedAssetsExist(t *testing.T) {
	for name, sh := range assetSource {
		if _, err := sh.fs.ReadFile(sh.dir + "/" + name); err != nil {
			t.Errorf("%s/%s is whitelisted but not embedded: %v", sh.dir, name, err)
		}
	}
}

// Two shells claiming the same asset name would make serving depend on map
// iteration order. Kinds SHARING one shell are the same pointer and are fine.
func TestShells_NoAssetNameCollision(t *testing.T) {
	owner := map[string]*clientShell{}
	all := make([]*clientShell, 0, len(shells)+1)
	seen := map[*clientShell]bool{}
	for _, sh := range shells {
		if !seen[sh] {
			seen[sh] = true
			all = append(all, sh)
		}
	}
	all = append(all, commonShell)
	for _, sh := range all {
		for name := range sh.assets {
			if prev, dup := owner[name]; dup && prev != sh {
				t.Errorf("asset %q is claimed by both %s and %s", name, prev.dir, sh.dir)
			}
			owner[name] = sh
		}
	}
}

// The relaxed policy is granted per shell, never globally: a renderer that
// needs WebAssembly must not hand every markdown paste the same capability.
func TestShells_WasmPolicyIsScopedToTheShellsThatNeedIt(t *testing.T) {
	needsWasm := map[domain.ContentKind]bool{
		domain.KindPDF:  true,
		domain.KindCSV:  true,
		domain.KindJSON: true,
	}
	for kind, sh := range shells {
		relaxed := strings.Contains(sh.policy(), "wasm-unsafe-eval")
		if needsWasm[kind] && !relaxed {
			t.Errorf("%s renders with WebAssembly but got the baseline policy", kind)
		}
		if !needsWasm[kind] && relaxed {
			t.Errorf("%s does not need WebAssembly but was granted 'wasm-unsafe-eval'", kind)
		}
	}
	// Whatever else it relaxes, no shell may re-admit inline or eval'd script:
	// that is the control protecting a paste's own origin.
	for kind, sh := range shells {
		for _, forbidden := range []string{"'unsafe-eval'", "'unsafe-inline' 'wasm", "script-src 'self' 'unsafe-inline'"} {
			if strings.Contains(sh.policy(), forbidden) {
				t.Errorf("%s policy contains %q, which re-admits script injection", kind, forbidden)
			}
		}
	}
}

// Every client-rendered kind needs a raw Content-Type: the shell fetches ?raw=1
// and a wrong type here is what makes a browser download a paste instead of
// rendering it.
func TestShells_EveryKindHasARawContentType(t *testing.T) {
	for kind := range shells {
		if rawContentType[kind] == "" {
			t.Errorf("kind %q has a shell but no raw Content-Type", kind)
		}
	}
}
