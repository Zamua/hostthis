package domain_test

import (
	"os/exec"
	"strings"
	"testing"
)

// Dependencies point inward: domain depends on nothing, service and storage on
// domain only, transports on service. storage must never be reachable from
// service or a transport, since a port is defined by its consumer and an
// adapter type crossing that boundary points the arrow the wrong way.
//
// Enforced rather than documented: a rule nothing checks decays.
func TestLayeringDependenciesPointInward(t *testing.T) {
	forbidden := map[string][]string{
		// package under test -> packages it must NOT import
		"domain":  {"service", "storage", "http", "ssh", "relay", "render"},
		"service": {"storage", "http", "ssh"},
		"storage": {"service", "http", "ssh"},
		"http":    {"storage"},
		"ssh":     {"storage"},
		"render":  {"service", "storage", "http", "ssh"},
		// Adapters holding a mechanism; must not grow upward into application or
		// transport concerns.
		"archive": {"service", "storage", "http", "ssh"},
		"mime":    {"service", "storage", "http", "ssh", "domain"},
	}

	for pkg, banned := range forbidden {
		out, err := exec.Command("go", "list", "-deps", "../"+pkg+"/...").Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		deps := string(out)
		for _, b := range banned {
			needle := "hostthis/internal/" + b + "\n"
			if strings.Contains(deps, needle) {
				t.Errorf("LAYERING VIOLATION: internal/%s imports internal/%s (directly or transitively).\n"+
					"Dependencies must point inward. If %s needs something from %s, either move the type to "+
					"domain (if it is a pure value) or declare a port in the consumer and have %s implement it.",
					pkg, b, pkg, b, b)
			}
		}
	}
}

// The domain must not depend on infrastructure, and stdlib counts.
//
// A package is banned when it represents a MECHANISM (transport, wire format,
// storage, process). Pure computation over values the domain already holds is
// fine, which is why crypto, encoding/json and regexp are not listed.
//
// When the domain needs one of these the answer is a port: declare the
// capability in the domain, let an adapter supply it.
func TestDomainDoesNotDependOnInfrastructure(t *testing.T) {
	banned := []string{
		"net/http",      // a transport (was imported for DetectContentType)
		"net",           // sockets
		"archive/tar",   // a wire format (was imported by the site extractor)
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

	// Guard the guard: if the dependency list ever comes back empty or tiny the
	// loop above passes while checking nothing.
	if len(deps) < 5 {
		t.Fatalf("go list returned %d deps for internal/domain, which cannot be right; "+
			"this guard would pass vacuously", len(deps))
	}
}
