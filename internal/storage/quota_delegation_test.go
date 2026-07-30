package storage

import (
	"os"
	"path/filepath"
	"regexp"
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
	// Any comparison against the cap that is not delegating to the domain.
	openCoded := regexp.MustCompile(`[><]=?\s*userCap|userCap\s*[><]=?`)
	// ...except asking whether a cap is configured at all, which short-circuits
	// an expensive scan rather than applying the rule.
	configCheck := regexp.MustCompile(`userCap\s*(>\s*0|<=\s*0)\s*\{?\s*$`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found; this guard would pass vacuously")
	}

	var scanned int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if !openCoded.MatchString(code) {
				continue
			}
			if configCheck.MatchString(strings.TrimSpace(code)) {
				continue
			}
			t.Errorf("%s:%d compares against userCap directly:\n\t%s\n"+
				"The quota rule belongs to domain.Allowance - use Admit for a plain write or "+
				"AdmitReplacing when a record's bytes are being displaced (which credits the old "+
				"size, the case the open-coded copies disagreed about). Computing `used` stays here; "+
				"deciding does not.", f, i+1, strings.TrimSpace(line))
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test files; this guard checked nothing")
	}

	// Positive control: without it the check above passes on a package that
	// dropped quota enforcement altogether.
	var delegations int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		delegations += strings.Count(string(src), "domain.Allowance{")
	}
	if delegations < 6 {
		t.Fatalf("found only %d domain.Allowance call sites across the adapters; the quota checks "+
			"have gone missing rather than moved", delegations)
	}
}
