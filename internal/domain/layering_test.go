package domain_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The layering rule: dependencies point INWARD. domain depends on nothing,
// service and storage depend only on domain, transports depend on service.
// storage must never be reachable from service or a transport - a port is
// defined by the consumer, and an adapter type crossing that boundary points
// the arrow the wrong way.
//
// This is enforced rather than documented because it held by convention for
// months and still drifted: a value type and an encoder function had leaked
// from storage into service and http, found only by auditing the import graph
// by hand. A rule nothing checks is a rule that decays.
func TestLayeringDependenciesPointInward(t *testing.T) {
	forbidden := map[string][]string{
		// package under test -> packages it must NOT import
		"domain":  {"service", "storage", "http", "ssh", "relay", "render"},
		"service": {"storage", "http", "ssh"},
		"storage": {"service", "http", "ssh"},
		"http":    {"storage"},
		"ssh":     {"storage"},
		"render":  {"service", "storage", "http", "ssh"},
		// The two adapters extracted to keep the domain pure. They exist to
		// hold a mechanism (a wire format, a classifier) and must not grow
		// upward into application or transport concerns.
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

// The domain must not depend on INFRASTRUCTURE, and stdlib counts.
//
// The rule above only checks our own packages, which is a real gap: it passed
// for months while the domain imported net/http and archive/tar. Neither is a
// layering violation by package name, but both are infrastructure concerns
// wearing a stdlib label - a transport, and an archive codec.
//
// The distinction this encodes: a package is banned when it represents a
// MECHANISM the domain should not know about (transport, wire format, storage,
// process, clock source). Pure computation on values is fine, which is why
// crypto/sha256, encoding/json and regexp are not listed - they compute over
// bytes the domain already holds rather than reaching outside the process.
//
// If the domain needs one of these, the answer is a PORT: declare the
// capability as an interface or function type in the domain and let an adapter
// supply the mechanism. DetectKind takes a MIMESniffer for exactly this reason.
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
