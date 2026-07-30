package storage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The quota DECISION belongs to the domain; adapters only supply the inputs.
//
// UNTAGGED so CI runs it: most of these files are behind the slatedb tag, and
// the failure this prevents has no behavioural symptom in any single adapter -
// it is the adapters disagreeing with each other.
//
// This rule existed nowhere and the arithmetic was open-coded in twelve places
// across four repositories, which had already diverged: some were a plain
// `used + body > cap`, some credited a replaced record's bytes
// (`used - creditOld + body`), two expressed that credit in different algebra,
// and some summed paste and site totals while others did not. Twelve copies of
// a rule are twelve chances for it to drift, and the subtle case
// (replace-credits-the-old-size) is exactly the one that gets fixed in one
// place and missed elsewhere.
//
// Computing how many bytes an identity occupies is a QUERY and stays here.
// Deciding whether that total admits a write is a RULE and does not.
func TestQuotaDecisionIsNotOpenCodedInAdapters(t *testing.T) {
	// Any comparison against the cap that is not delegating to the domain.
	openCoded := regexp.MustCompile(`[><]=?\s*userCap|userCap\s*[><]=?`)
	// ...except asking whether a cap is CONFIGURED at all. That is not the
	// rule, it is a short-circuit that skips an expensive byte-summing scan
	// when no ceiling applies, and it belongs in the adapter because the scan
	// does. domain.Allowance.Unlimited() encodes the same meaning for callers
	// that hold an Allowance.
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

	// Positive control: the delegation this guard exists to protect must
	// actually be present. Without it the check above passes just as happily
	// on a package that dropped quota enforcement altogether.
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
