package domain_test

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const internalPrefix = "github.com/Zamua/hostthis/internal/"

// layerPolicy states the architecture positively: for every package under
// internal/, the COMPLETE set of internal packages it may reach, directly or
// transitively. Two rules make it fail closed - anything a package reaches
// that is not on its list is a violation, and a package directory with no
// entry here is itself a violation. A package added tomorrow is therefore
// guarded the moment it exists, rather than whenever someone remembers to
// extend a ban list.
//
// Inward to outward:
//
//	mime, render      mechanisms holding no business knowledge, so they sit
//	                  BELOW domain and must not reach it; domain consumes them
//	                  through a port instead (DetectKind takes a MIMESniffer).
//	durable           the same shape for crash recovery: an intent-log port and
//	                  its value types, knowing nothing about pastes. storage
//	                  consumes it, so which mechanism provides durability never
//	                  reaches service or a transport.
//	domain            pure types and rules.
//	archive, cache    mechanisms that speak in domain values.
//	storage           repository adapters.
//	service           use cases; declares the ports, never names an adapter.
//	relay, relaygrpc  the room relay and its gRPC peer transport.
//	http, ssh         transports; they reach service, never an adapter.
//	shaleblob         the single adapter binding a storage-side blob unit to a
//	                  service port, so it alone may see both. Anything else
//	                  needing both belongs in cmd/hostthisd.
var layerPolicy = map[string][]string{
	"mime":            {},
	"render":          {},
	"durable":         {},
	"domain":          {},
	"archive":         {"domain"},
	"cache":           {"domain"},
	"storage":         {"domain", "durable"},
	"service":         {"archive", "domain", "mime"},
	"relay":           {"domain"},
	"relay/relaygrpc": {"domain", "relay"},
	"http":            {"archive", "domain", "mime", "relay", "service"},
	"ssh":             {"archive", "domain", "mime", "service"},
	"shaleblob":       {"archive", "domain", "durable", "mime", "service", "storage"},

	// Test-only harness: the importable package is empty, so what is pinned
	// here is that it STAYS empty. Its _test.go files wire whole stacks, which
	// is legitimate and is why the graph below is production-only.
	"sitevalidation": {},

	// Test-only fixture: opens the metadata repo other packages' tests build
	// on, so it reaches storage by design. Nothing in production may import it,
	// which the "no adapter reachable from service" rules below enforce because
	// it is not in any production package's allowed set.
	"storagetest": {"domain", "durable", "storage"},
}

// Dependencies point inward: domain depends on nothing, service and storage on
// domain only, transports on service. A port is defined by its consumer, so an
// adapter type reachable from service or a transport points the arrow the
// wrong way.
func TestLayeringDependenciesPointInward(t *testing.T) {
	dirs := internalPackageDirs(t, "..")
	graph := internalDependencyGraph(t)

	for _, v := range layerViolations(layerPolicy, dirs, graph) {
		t.Error(v)
	}
}

// layerViolations reports every way an observed package set and dependency
// graph departs from policy. It performs no I/O so the guard itself can be
// exercised against a synthetic graph.
func layerViolations(policy map[string][]string, dirs []string, graph map[string][]string) []string {
	var out []string

	for _, pkg := range dirs {
		allowed, governed := policy[pkg]
		if !governed {
			out = append(out, "UNGUARDED PACKAGE: internal/"+pkg+" has no layerPolicy entry.\n"+
				"Every package under internal/ must declare the complete set of internal packages it may "+
				"reach, so that a new package is denied by default rather than silently exempt.")
			continue
		}
		deps, listed := graph[pkg]
		if !listed {
			out = append(out, "UNCHECKED PACKAGE: internal/"+pkg+" exists on disk but go list did not report it, "+
				"so its dependencies were never inspected. Fix the build constraints or the list invocation; "+
				"an unloadable package is an unguarded one.")
			continue
		}
		for _, dep := range deps {
			if dep == pkg || slices.Contains(allowed, dep) {
				continue
			}
			out = append(out, "LAYERING VIOLATION: internal/"+pkg+" reaches internal/"+dep+
				" (directly or transitively).\nDependencies must point inward. If "+pkg+" needs something from "+
				dep+", either move the type to domain (if it is a pure value) or declare a port in the consumer "+
				"and have "+dep+" implement it. Widening layerPolicy is the last resort, not the first.")
		}
	}

	for pkg := range policy {
		if !slices.Contains(dirs, pkg) {
			out = append(out, "STALE POLICY ENTRY: layerPolicy names internal/"+pkg+", which does not exist. "+
				"Remove the entry so the map keeps describing the tree.")
		}
	}

	// A package inherits whatever its permitted imports may reach, so an
	// allow-list that is not closed under the policy describes a graph that
	// cannot occur and would report spurious violations the first time the
	// intermediate package grew a dependency.
	for pkg, allowed := range policy {
		for _, dep := range allowed {
			inherited, governed := policy[dep]
			if !governed {
				out = append(out, "POLICY NAMES UNKNOWN PACKAGE: internal/"+pkg+" is allowed to reach internal/"+
					dep+", which has no layerPolicy entry.")
				continue
			}
			for _, indirect := range inherited {
				if !slices.Contains(allowed, indirect) {
					out = append(out, "POLICY NOT CLOSED: internal/"+pkg+" may reach internal/"+dep+
						", which may reach internal/"+indirect+", but internal/"+indirect+
						" is missing from "+pkg+"'s list.")
				}
			}
		}
	}

	slices.Sort(out)
	return out
}

// internalPackageDirs enumerates the packages that actually exist on disk.
// Reading the tree rather than a hand-written list is what makes the guard
// fail closed: a package nobody added to layerPolicy still shows up here.
func internalPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != root &&
			(name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		hasGo := slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
			return !e.IsDir() && strings.HasSuffix(e.Name(), ".go")
		})
		if !hasGo {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// Guard the guard: an empty or tiny package list makes every loop above
	// pass while checking nothing.
	if len(dirs) < 8 {
		t.Fatalf("found %d packages under %s, which cannot be right; this guard would pass vacuously", len(dirs), root)
	}
	return dirs
}

// internalDependencyGraph maps each internal package to the internal packages
// it reaches, transitively. The slatedb tag is required rather than optional:
// internal/shaleblob exists only under it, and a package go list cannot see is
// a package this guard cannot check.
func internalDependencyGraph(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "slatedb", "-f", `{{.ImportPath}} {{join .Deps " "}}`, "../...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -tags slatedb ../...: %v\n%s", err, stderr.String())
	}

	graph := make(map[string][]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg, ok := strings.CutPrefix(fields[0], internalPrefix)
		if !ok {
			continue
		}
		deps := []string{}
		for _, d := range fields[1:] {
			if dep, ok := strings.CutPrefix(d, internalPrefix); ok {
				deps = append(deps, dep)
			}
		}
		graph[pkg] = deps
	}
	return graph
}

// The guard must catch the shapes a hand-listed ban map structurally cannot:
// a package with no entry at all, and an outward import whose target the
// author happened not to name.
func TestLayerViolationsFailsClosed(t *testing.T) {
	policy := map[string][]string{
		"mime":    {},
		"domain":  {},
		"storage": {"domain"},
		"relay":   {"domain"},
	}
	dirs := []string{"domain", "mime", "relay", "storage"}
	clean := map[string][]string{
		"domain":  {},
		"mime":    {},
		"relay":   {"domain"},
		"storage": {"domain"},
	}

	if got := layerViolations(policy, dirs, clean); len(got) != 0 {
		t.Fatalf("conforming graph reported violations: %v", got)
	}

	cases := []struct {
		name  string
		dirs  []string
		graph map[string][]string
		want  string
	}{
		{
			name: "package with no policy entry",
			dirs: []string{"domain", "mime", "relay", "shaleblob", "storage"},
			graph: map[string][]string{
				"domain": {}, "mime": {}, "relay": {"domain"}, "storage": {"domain"},
				"shaleblob": {"storage"},
			},
			want: "UNGUARDED PACKAGE: internal/shaleblob",
		},
		{
			name: "outward import into an adapter",
			dirs: dirs,
			graph: map[string][]string{
				"domain": {}, "mime": {}, "storage": {"domain"},
				"relay": {"domain", "storage"},
			},
			want: "LAYERING VIOLATION: internal/relay reaches internal/storage",
		},
		{
			name: "domain reaching a mechanism below it",
			dirs: dirs,
			graph: map[string][]string{
				"domain": {"mime"}, "mime": {}, "relay": {"domain"}, "storage": {"domain"},
			},
			want: "LAYERING VIOLATION: internal/domain reaches internal/mime",
		},
		{
			name: "package on disk that go list never reported",
			dirs: dirs,
			graph: map[string][]string{
				"domain": {}, "mime": {}, "storage": {"domain"},
			},
			want: "UNCHECKED PACKAGE: internal/relay",
		},
		{
			name:  "policy entry with no package on disk",
			dirs:  []string{"domain", "mime", "relay"},
			graph: clean,
			want:  "STALE POLICY ENTRY: layerPolicy names internal/storage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := layerViolations(policy, tc.dirs, tc.graph)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, tc.want) }) {
				t.Errorf("no violation starting with %q; got %v", tc.want, got)
			}
		})
	}
}

// An allow-list that is not closed under the policy describes an impossible
// graph, so the closure rule has to be able to fire.
func TestLayerViolationsRejectsAnUnclosedPolicy(t *testing.T) {
	policy := map[string][]string{
		"domain":  {},
		"archive": {"domain"},
		"service": {"archive"}, // reaches domain through archive, but omits it
	}
	dirs := []string{"archive", "domain", "service"}
	graph := map[string][]string{
		"domain": {}, "archive": {"domain"}, "service": {"archive", "domain"},
	}

	got := layerViolations(policy, dirs, graph)
	if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, "POLICY NOT CLOSED:") }) {
		t.Errorf("unclosed policy not reported; got %v", got)
	}
}

// The domain must not depend on infrastructure, and stdlib counts. A package
// is banned when it represents a MECHANISM (transport, wire format, storage,
// process); pure computation over values the domain already holds is fine,
// which is why crypto, encoding/json and regexp are not listed.
func TestDomainDoesNotDependOnInfrastructure(t *testing.T) {
	banned := []string{
		"net/http",      // a transport
		"net",           // sockets
		"archive/tar",   // a wire format
		"archive/zip",   //
		"compress/gzip", //
		"database/sql",  // storage
		"os/exec",       // process
	}

	out, err := exec.Command("go", "list", "-deps", "../domain").Output()
	if err != nil {
		t.Fatalf("go list -deps domain: %v", err)
	}
	deps := make(map[string]bool)
	for line := range strings.SplitSeq(string(out), "\n") {
		deps[strings.TrimSpace(line)] = true
	}

	for _, b := range banned {
		if deps[b] {
			t.Errorf("DOMAIN PURITY VIOLATION: internal/domain depends on %q.\n"+
				"That is a mechanism (transport / wire format / storage / process), not a business rule. "+
				"Declare a PORT in the domain - an interface or function type naming the capability - and "+
				"have an adapter supply it, the way DetectKind takes a MIMESniffer.", b)
		}
	}

	// Guard the guard: an empty or tiny dependency list makes the loop above
	// pass while checking nothing.
	if len(deps) < 5 {
		t.Fatalf("go list returned %d deps for internal/domain, which cannot be right; "+
			"this guard would pass vacuously", len(deps))
	}
}
