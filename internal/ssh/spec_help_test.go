package ssh

import (
	"flag"
	"os"
	"strings"
	"testing"
)

var updateSpecHelp = flag.Bool("update-spec-help", false, "rewrite the help banner in docs/SPEC.md from helpText")

const specPath = "../../docs/SPEC.md"

// The help banner quoted in docs/SPEC.md under "### Help" is the banner the
// server prints, so the spec cannot drift from it. After changing the banner:
//
//	go test ./internal/ssh -run TestSpecQuotesTheHelpBanner -update-spec-help
func TestSpecQuotesTheHelpBanner(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	const open, fence = "### Help\n", "\n```\n"
	section := strings.Index(spec, open)
	if section < 0 {
		t.Fatal(`docs/SPEC.md has no "### Help" section`)
	}
	start := strings.Index(spec[section:], fence)
	if start < 0 {
		t.Fatal(`no fenced block under "### Help"`)
	}
	start += section + len(fence)
	end := strings.Index(spec[start:], fence)
	if end < 0 {
		t.Fatal(`the fenced block under "### Help" never closes`)
	}
	end += start
	want := helpText("hostthis.dev")
	if got := spec[start:end]; got != want {
		if *updateSpecHelp {
			if err := os.WriteFile(specPath, []byte(spec[:start]+want+spec[end:]), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatalf("the help banner in docs/SPEC.md is not the one the server prints; run with -update-spec-help")
	}
}
