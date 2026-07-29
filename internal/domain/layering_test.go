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
