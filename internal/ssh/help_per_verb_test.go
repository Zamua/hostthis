package ssh_test

// Pins the `help <verb>` and `<verb> --help` / `<verb> -h` shapes against a
// real ssh server + client (startStack). The global help banner is pinned
// byte-exact by the characterization suite; this file covers only the
// verb-specific shapes.

import (
	"strings"
	"testing"
)

// verbHelpVerbs is the canonical set of verb names the help system must
// recognize (one descriptor each in help_verbs.go). A verb added to the
// dispatcher needs an entry here AND a descriptor.
var verbHelpVerbs = []string{
	"get", "list", "url", "qr", "rename", "delete",
	"versions", "pin", "unpin", "whoami", "help",
}

// TestHelpVerb_HelpSpaceVerb pins that `help <verb>` emits the verb-specific
// block (Usage: + Examples:) and exits 0, for every known verb.
func TestHelpVerb_HelpSpaceVerb(t *testing.T) {
	s := startStack(t)
	for _, v := range verbHelpVerbs {
		t.Run("help_"+v, func(t *testing.T) {
			_, stderr, exit := s.run("help "+v, nil)
			if exit != 0 {
				t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
			}
			if !strings.Contains(stderr, "Usage:") {
				t.Fatalf("expected Usage: section in verb help, got %q", stderr)
			}
			if !strings.Contains(stderr, "Examples:") {
				t.Fatalf("expected Examples: section in verb help, got %q", stderr)
			}
			// The global banner's opening line is the canary: verb help
			// must not fall through to it.
			if strings.Contains(stderr, "Pipe a rendered file in") {
				t.Fatalf("verb help leaked the global banner: %q", stderr)
			}
		})
	}
}

// TestHelpVerb_VerbDashDashHelp pins that `<verb> --help` produces the
// `help <verb>` body byte-for-byte, so the two surfaces cannot drift.
func TestHelpVerb_VerbDashDashHelp(t *testing.T) {
	s := startStack(t)
	for _, v := range verbHelpVerbs {
		t.Run(v+"_dashdash_help", func(t *testing.T) {
			if v == "help" {
				// Degenerate: the dispatcher's `help` case sees `--help`
				// as the verb arg, which is not in the descriptor map.
				t.Skip("help --help has bespoke unknown-verb shape covered elsewhere")
			}
			_, stderr, exit := s.run(v+" --help", nil)
			if exit != 0 {
				t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
			}
			if !strings.Contains(stderr, "Usage:") {
				t.Fatalf("expected Usage: section for %s --help, got %q", v, stderr)
			}
			_, helpVerbStderr, _ := s.run("help "+v, nil)
			if stderr != helpVerbStderr {
				t.Fatalf("`%s --help` diverged from `help %s`:\n got %q\n want %q",
					v, v, stderr, helpVerbStderr)
			}
		})
	}
}

// TestHelpVerb_VerbDashH mirrors the --help pin for the `-h` shorthand, kept
// separate so a regression in one form cannot mask the other.
func TestHelpVerb_VerbDashH(t *testing.T) {
	s := startStack(t)
	for _, v := range verbHelpVerbs {
		t.Run(v+"_dash_h", func(t *testing.T) {
			if v == "help" {
				t.Skip("help -h has bespoke unknown-verb shape covered elsewhere")
			}
			_, stderr, exit := s.run(v+" -h", nil)
			if exit != 0 {
				t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
			}
			if !strings.Contains(stderr, "Usage:") {
				t.Fatalf("expected Usage: section for %s -h, got %q", v, stderr)
			}
			_, helpVerbStderr, _ := s.run("help "+v, nil)
			if stderr != helpVerbStderr {
				t.Fatalf("`%s -h` diverged from `help %s`:\n got %q\n want %q",
					v, v, stderr, helpVerbStderr)
			}
		})
	}
}

// TestHelpVerb_HelpUnknown pins the `help <unknown>` shape: an `unknown verb`
// prefix on stderr, then the global banner, exit 0.
func TestHelpVerb_HelpUnknown(t *testing.T) {
	s := startStack(t)
	_, stderr, exit := s.run("help notarealverb", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if !strings.Contains(stderr, `unknown verb "notarealverb"`) {
		t.Fatalf("expected unknown-verb prefix, got %q", stderr)
	}
	// The banner must follow so the user sees the verb list to pick from.
	if !strings.Contains(stderr, "Pipe a rendered file in") {
		t.Fatalf("expected global help to follow unknown-verb prefix, got %q", stderr)
	}
}

// TestHelpVerb_BareHelpUnchanged pins that bare `help` still emits the global
// banner and no verb block. The byte-exact golden lives in
// characterization_test.go; this checks the canary line next to the verb code.
func TestHelpVerb_BareHelpUnchanged(t *testing.T) {
	s := startStack(t)
	_, stderr, exit := s.run("help", nil)
	if exit != 0 {
		t.Fatalf("exit: %d", exit)
	}
	if !strings.Contains(stderr, "Pipe a rendered file in") {
		t.Fatalf("bare `help` must emit global banner, got %q", stderr)
	}
	if strings.Contains(stderr, "Usage:") {
		t.Fatalf("bare `help` must NOT emit verb-specific Usage: block, got %q", stderr)
	}
}

// TestHelpVerb_PtyCrLf pins the PTY-aware CRLF translation: CRLF-terminated
// with a PTY allocated, LF-only without one.
func TestHelpVerb_PtyCrLf(t *testing.T) {
	s := startStack(t)

	t.Run("NoPty_LF_Only", func(t *testing.T) {
		_, stderr, exit := s.run("help get", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if strings.Contains(stderr, "\r\n") {
			t.Fatalf("no-PTY verb help should be LF-only, found CRLF in %q", stderr)
		}
		if !strings.Contains(stderr, "Usage:") {
			t.Fatalf("expected Usage: line, got %q", stderr)
		}
	})

	t.Run("WithPty_CRLF", func(t *testing.T) {
		_, stderr, exit := runCmdWithPty(t, s.keyedClient, "help get")
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "\r\n") {
			t.Fatalf("PTY verb help should be CRLF, got LF-only %q", stderr)
		}
	})

	t.Run("VerbDashDashHelp_NoPty_LF", func(t *testing.T) {
		_, stderr, exit := s.run("list --help", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if strings.Contains(stderr, "\r\n") {
			t.Fatalf("no-PTY `list --help` should be LF-only, found CRLF in %q", stderr)
		}
	})

	t.Run("VerbDashDashHelp_WithPty_CRLF", func(t *testing.T) {
		_, stderr, exit := runCmdWithPty(t, s.keyedClient, "list --help")
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "\r\n") {
			t.Fatalf("PTY `list --help` should be CRLF, got LF-only %q", stderr)
		}
	})
}

// TestHelpVerb_VerbBodyMentionsVerb catches a copy-paste regression where two
// descriptors' signatures swap: each verb's help must name that verb.
func TestHelpVerb_VerbBodyMentionsVerb(t *testing.T) {
	s := startStack(t)
	cases := map[string]string{
		"list":     "list",
		"get":      "get",
		"rename":   "rename",
		"delete":   "delete",
		"versions": "versions",
		"pin":      "pin",
		"unpin":    "unpin",
		"whoami":   "whoami",
	}
	for verb, needle := range cases {
		t.Run(verb, func(t *testing.T) {
			_, stderr, exit := s.run("help "+verb, nil)
			if exit != 0 {
				t.Fatalf("exit: %d", exit)
			}
			if !strings.Contains(stderr, needle) {
				t.Fatalf("expected verb help for %q to mention %q, got %q", verb, needle, stderr)
			}
		})
	}
}

// TestHelpVerb_NoSideEffects pins that `delete <slug> --help` does NOT run the
// delete: the help intercept must short-circuit before the verb body.
func TestHelpVerb_NoSideEffects(t *testing.T) {
	s := startStack(t)
	// A slug-shaped string, so the verb's own parser would not reject it on
	// arg-shape grounds.
	_, stderr, exit := s.run("delete abcd1234 --help", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("expected verb help, got %q", stderr)
	}
	// A real delete would have surfaced `not found` for this unowned slug.
	if strings.Contains(stderr, "not found") {
		t.Fatalf("intercept failed - delete ran instead of help: %q", stderr)
	}
}
