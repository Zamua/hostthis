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
// transitively. It fails closed: anything a package reaches that is not on its
// list is a violation, and a package directory with no entry is itself one.
//
// Inward to outward:
//
//	mime              a mechanism holding no business knowledge, so it sits
//	                  BELOW domain and must not reach it; domain consumes it
//	                  through a port instead (DetectKind takes a MIMESniffer).
//	domain            pure types and rules.
//	archive, cache    mechanisms that speak in domain values.
//	storage           repository adapters.
//	service           use cases; declares the ports, never names an adapter.
//	relay, relaygrpc  the room relay and its gRPC peer transport.
//	http, ssh         transports; they reach service, never an adapter.
var layerPolicy = map[string][]string{
	"mime": {},
	// Collectors only: the consumer declares a recorder port and this package
	// satisfies it, so it must never reach the layers it observes.
	"metrics": {},
	"domain":  {},
	// The shared room-realtime vocabulary. A leaf like domain: both relay
	// implementations speak it, and it may never grow machinery.
	"roomwire": {"domain"},
	"archive":  {"domain"},
	"cache":    {"domain"},
	"storage":  {"domain"},
	"service":  {"archive", "domain", "mime"},
	"http":     {"archive", "domain", "mime", "roomwire", "service"},
	"ssh":      {"archive", "domain", "mime", "service"},

	// The celld backend implements domain-shaped ports directly and does not
	// depend on the in-process storage adapter.
	"celld": {"domain", "roomwire"},

	// Test-only harness: the importable package is empty and must stay so. Its
	// _test.go files wire whole stacks, which is why the graph is production-only.
	"sitevalidation": {},

	// Test-only fixture that opens the metadata repo other packages' tests
	// build on. Nothing in production may import it, which holds because it is
	// in no production package's allowed set.
	"storagetest": {"domain", "storage"},
}

// domainBannedStd are the stdlib packages the domain may not reach, even
// transitively: each is a MECHANISM (transport, wire format, storage,
// process). Pure computation over values the domain already holds is fine,
// which is why crypto, encoding/json and regexp are absent.
var domainBannedStd = []string{
	"net/http", "net", "archive/tar", "archive/zip", "compress/gzip", "database/sql", "os/exec",
}

// Dependencies point inward: domain depends on nothing, service and storage on
// domain only, transports on service. A port is defined by its consumer, so an
// adapter type reachable from service or a transport points the arrow the
// wrong way.
func TestLayeringDependenciesPointInward(t *testing.T) {
	dirs := internalPackageDirs(t, "..")
	graph, std := internalDependencyGraph(t)

	for _, v := range layerViolations(layerPolicy, dirs, graph) {
		t.Error(v)
	}
	for _, b := range domainBannedStd {
		if slices.Contains(std["domain"], b) {
			t.Errorf("DOMAIN PURITY VIOLATION: internal/domain depends on %q; declare a port and have an adapter supply it", b)
		}
	}
}

// layerViolations reports every way an observed package set and dependency
// graph departs from policy. No I/O, so the guard itself can be exercised
// against a synthetic graph.
func layerViolations(policy map[string][]string, dirs []string, graph map[string][]string) []string {
	var out []string

	for _, pkg := range dirs {
		allowed, governed := policy[pkg]
		if !governed {
			out = append(out, "UNGUARDED PACKAGE: internal/"+pkg+" has no layerPolicy entry; a new package is denied by default")
			continue
		}
		deps, listed := graph[pkg]
		if !listed {
			out = append(out, "UNCHECKED PACKAGE: internal/"+pkg+" is on disk but go list did not report it; an unloadable package is an unguarded one")
			continue
		}
		for _, dep := range deps {
			if dep == pkg || slices.Contains(allowed, dep) {
				continue
			}
			out = append(out, "LAYERING VIOLATION: internal/"+pkg+" reaches internal/"+dep+
				"; move the type to domain or declare a port in the consumer, widening layerPolicy is the last resort")
		}
	}

	for pkg := range policy {
		if !slices.Contains(dirs, pkg) {
			out = append(out, "STALE POLICY ENTRY: layerPolicy names internal/"+pkg+", which does not exist")
		}
	}

	// A package inherits whatever its permitted imports may reach, so an
	// allow-list not closed under the policy describes an impossible graph.
	for pkg, allowed := range policy {
		for _, dep := range allowed {
			inherited, governed := policy[dep]
			if !governed {
				out = append(out, "POLICY NAMES UNKNOWN PACKAGE: internal/"+pkg+" may reach internal/"+dep+", which has no layerPolicy entry")
				continue
			}
			for _, indirect := range inherited {
				if !slices.Contains(allowed, indirect) {
					out = append(out, "POLICY NOT CLOSED: internal/"+pkg+" may reach internal/"+dep+
						", which may reach internal/"+indirect+", missing from "+pkg+"'s list")
				}
			}
		}
	}

	slices.Sort(out)
	return out
}

// internalPackageDirs enumerates the packages that exist on disk. Reading the
// tree rather than a hand-written list is what makes the guard fail closed.
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
	// Guard the guard: a tiny package list makes every check pass vacuously.
	if len(dirs) < 8 {
		t.Fatalf("found %d packages under %s, which cannot be right", len(dirs), root)
	}
	return dirs
}

// internalDependencyGraph maps each internal package to the internal packages
// it reaches transitively, and separately to its non-internal (stdlib and
// module) deps. The default build must load every package, or an unloadable
// package could escape this guard.
func internalDependencyGraph(t *testing.T) (internal, external map[string][]string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Deps " "}}`, "../...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ../...: %v\n%s", err, stderr.String())
	}

	internal, external = map[string][]string{}, map[string][]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg, ok := strings.CutPrefix(fields[0], internalPrefix)
		if !ok {
			continue
		}
		internal[pkg] = []string{}
		for _, d := range fields[1:] {
			if dep, ok := strings.CutPrefix(d, internalPrefix); ok {
				internal[pkg] = append(internal[pkg], dep)
			} else {
				external[pkg] = append(external[pkg], d)
			}
		}
	}
	// Guard the guard: a dependency list this short means go list saw nothing.
	if len(external["domain"]) < 5 {
		t.Fatalf("go list returned %d deps for internal/domain, which cannot be right", len(external["domain"]))
	}
	return internal, external
}

// The guard catches the shapes a hand-listed ban map structurally cannot: a
// package with no entry, an outward import whose target the author did not
// name, and an allow-list not closed under the policy.
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
		name   string
		policy map[string][]string
		dirs   []string
		graph  map[string][]string
		want   string
	}{
		{
			name: "package with no policy entry",
			dirs: []string{"adapterx", "domain", "mime", "relay", "storage"},
			graph: map[string][]string{
				"domain": {}, "mime": {}, "relay": {"domain"}, "storage": {"domain"},
				"adapterx": {"storage"},
			},
			want: "UNGUARDED PACKAGE: internal/adapterx",
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
		{
			name: "policy not closed under itself",
			policy: map[string][]string{
				"domain":  {},
				"archive": {"domain"},
				"service": {"archive"}, // reaches domain through archive, but omits it
			},
			dirs: []string{"archive", "domain", "service"},
			graph: map[string][]string{
				"domain": {}, "archive": {"domain"}, "service": {"archive", "domain"},
			},
			want: "POLICY NOT CLOSED:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			if p == nil {
				p = policy
			}
			got := layerViolations(p, tc.dirs, tc.graph)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, tc.want) }) {
				t.Errorf("no violation starting with %q; got %v", tc.want, got)
			}
		})
	}
}
