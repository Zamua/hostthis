package ssh_test

// Per-verb help tests (Phase C polish, pattern #3).
//
// Pin the additive `help <verb>` and `<verb> --help` / `<verb> -h`
// behavior so a future refactor of the verb dispatcher can't silently
// regress it. The global help banner is pinned byte-exact by the
// Phase A characterization suite; this file only covers the new
// verb-specific shapes.
//
// Conventions match the rest of the package's test suite:
//   - real ssh server + real ssh client via startStack
//   - sub-tests per behavior bullet
//   - assertions over (stdout, stderr, exit) shape

import (
	"strings"
	"testing"
)

// verbHelpVerbs is the canonical set of verb names the help system
// must recognize. Doc-only aliases (`put`, `get`) are included
// alongside the dispatch verbs. Whenever a new verb is added to the
// dispatcher, append it here AND add a descriptor in help_verbs.go.
var verbHelpVerbs = []string{
	"put", "get", "list", "show", "rename", "delete",
	"versions", "pin", "unpin", "whoami", "help",
}

// TestHelpVerb_HelpSpaceVerb covers `ssh hostthis.dev help <verb>`.
// One sub-test per known verb verifies the verb-specific block is
// emitted (signature line carries the verb name, stderr contains the
// `Usage:` and `Examples:` section headings) and the exit is 0.
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
			// `help <verb>` MUST NOT emit the global banner - that's the
			// whole point of verb-specific help. The global banner's
			// opening line is the canary.
			if strings.Contains(stderr, "Pipe a rendered file in") {
				t.Fatalf("verb help leaked the global banner: %q", stderr)
			}
		})
	}
}

// TestHelpVerb_VerbDashDashHelp covers `ssh hostthis.dev <verb> --help`.
// Each verb is dispatched with `--help` appended; the body must match
// the `help <verb>` form byte-for-byte (same source text, same PTY-
// awareness) so the two surfaces stay consistent.
func TestHelpVerb_VerbDashDashHelp(t *testing.T) {
	s := startStack(t)
	for _, v := range verbHelpVerbs {
		t.Run(v+"_dashdash_help", func(t *testing.T) {
			if v == "help" {
				// `help --help` is degenerate: the dispatcher's `help`
				// case sees `--help` as the verb arg and treats it as
				// an unknown verb (it isn't in the descriptor map).
				// Other verbs are exercised normally below.
				t.Skip("help --help has bespoke unknown-verb shape covered elsewhere")
			}
			_, stderr, exit := s.run(v+" --help", nil)
			if exit != 0 {
				t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
			}
			if !strings.Contains(stderr, "Usage:") {
				t.Fatalf("expected Usage: section for %s --help, got %q", v, stderr)
			}
			// Cross-check: same body as `help <verb>`.
			_, helpVerbStderr, _ := s.run("help "+v, nil)
			if stderr != helpVerbStderr {
				t.Fatalf("`%s --help` diverged from `help %s`:\n got %q\n want %q",
					v, v, stderr, helpVerbStderr)
			}
		})
	}
}

// TestHelpVerb_VerbDashH mirrors the --help test for the `-h` shorthand.
// We keep it as a separate test so a future regression in one form
// doesn't mask the other.
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

// TestHelpVerb_HelpUnknown pins the `help <unknown>` shape: prefix
// stderr with `unknown verb`, fall back to the global help banner,
// exit 0 (the user asked for help; we hand them help).
func TestHelpVerb_HelpUnknown(t *testing.T) {
	s := startStack(t)
	_, stderr, exit := s.run("help notarealverb", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if !strings.Contains(stderr, `unknown verb "notarealverb"`) {
		t.Fatalf("expected unknown-verb prefix, got %q", stderr)
	}
	// The global banner MUST follow so the user sees the verb list
	// they meant to pick from.
	if !strings.Contains(stderr, "Pipe a rendered file in") {
		t.Fatalf("expected global help to follow unknown-verb prefix, got %q", stderr)
	}
}

// TestHelpVerb_BareHelpUnchanged guards the Phase A invariant that
// `help` (no arg) emits the global banner unchanged. The byte-exact
// golden lives in characterization_test.go; this test only verifies
// the canary line + LF discipline to make a regression in this file
// fail fast next to the related code.
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

// TestHelpVerb_PtyCrLf pins the PTY-aware CRLF translation: with a
// PTY allocated, verb help lines are CRLF-terminated; without a PTY,
// they're LF-only. Mirrors the global help banner's PTY behavior so
// the user's terminal renders verb help cleanly either way.
func TestHelpVerb_PtyCrLf(t *testing.T) {
	s := startStack(t)

	t.Run("NoPty_LF_Only", func(t *testing.T) {
		_, stderr, exit := s.run("help put", nil)
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
		_, stderr, exit := runCmdWithPty(t, s.keyedClient, "help put")
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

// TestHelpVerb_VerbBodyMentionsVerb_Spot-checks a few verbs to confirm
// the descriptor's signature actually references the verb (catches a
// copy-paste regression where two descriptors' signatures swap).
func TestHelpVerb_VerbBodyMentionsVerb(t *testing.T) {
	s := startStack(t)
	cases := map[string]string{
		"list":     "list",
		"show":     "show",
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

// TestHelpVerb_NoSideEffects guards against the worst regression:
// `delete <slug> --help` running the delete. With no slug owned by
// the test identity, a real delete would surface a not-found error;
// the help intercept must emit the verb help instead and exit 0 with
// no Manage service call.
func TestHelpVerb_NoSideEffects(t *testing.T) {
	s := startStack(t)
	// Pick a slug-shaped string so the verb's own parser wouldn't
	// reject it on arg-shape grounds.
	_, stderr, exit := s.run("delete abcd1234 --help", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("expected verb help, got %q", stderr)
	}
	// A real delete that hit not-found would say `not found` on
	// stderr; the help intercept must short-circuit before then.
	if strings.Contains(stderr, "not found") {
		t.Fatalf("intercept failed - delete ran instead of help: %q", stderr)
	}
}
