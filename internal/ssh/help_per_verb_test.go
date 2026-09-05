package ssh_test

// Pins the `help <verb>` and `<verb> --help` / `<verb> -h` shapes against a
// real ssh server + client. The global help banner is pinned byte-exact by the
// characterization suite; this file covers only the verb-specific shapes.

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
// block (Usage: + Examples:), exits 0, and that the Usage line names THAT
// verb, for every known verb. The Usage line is matched as a whole token: a
// substring search over the body cannot see two descriptors swapped, since
// "pin" occurs inside "unpin" and "delete" inside "deleted".
func TestHelpVerb_HelpSpaceVerb(t *testing.T) {
	s := startStack(t)
	for _, v := range verbHelpVerbs {
		t.Run(v, func(t *testing.T) {
			_, stderr, exit := s.run("help "+v, nil)
			if exit != 0 {
				t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
			}
			if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "Examples:") {
				t.Fatalf("expected Usage: and Examples: sections in verb help, got %q", stderr)
			}
			// The global banner's opening line is the canary: verb help
			// must not fall through to it.
			if strings.Contains(stderr, "Pipe a rendered file in") {
				t.Fatalf("verb help leaked the global banner: %q", stderr)
			}
			usage, ok := usageLine(stderr)
			if !ok {
				t.Fatalf("no Usage: line in help for %q, got %q", v, stderr)
			}
			// "ssh <apex> <verb> ..."; the verb is the third whitespace token.
			if fields := strings.Fields(usage); len(fields) < 3 || fields[2] != v {
				t.Fatalf("help %q printed the usage %q: the descriptors are swapped or mis-keyed", v, usage)
			}
		})
	}
}

// TestHelpVerb_VerbHelpFlags pins that `<verb> --help` and `<verb> -h` each
// produce the `help <verb>` body byte-for-byte, so the surfaces cannot drift.
func TestHelpVerb_VerbHelpFlags(t *testing.T) {
	s := startStack(t)
	for _, flag := range []string{"--help", "-h"} {
		for _, v := range verbHelpVerbs {
			if v == "help" {
				// Degenerate: the dispatcher's `help` case sees the flag as
				// the verb arg, which is not in the descriptor map.
				continue
			}
			t.Run(v+" "+flag, func(t *testing.T) {
				_, stderr, exit := s.run(v+" "+flag, nil)
				if exit != 0 {
					t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
				}
				_, helpVerbStderr, _ := s.run("help "+v, nil)
				if stderr != helpVerbStderr || !strings.Contains(stderr, "Usage:") {
					t.Fatalf("`%s %s` diverged from `help %s`:\n got %q\n want %q",
						v, flag, v, stderr, helpVerbStderr)
				}
			})
		}
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

// TestHelpVerb_PtyCrLf pins the PTY-aware CRLF translation for both verb-help
// surfaces: CRLF-terminated with a PTY allocated, LF-only without one.
func TestHelpVerb_PtyCrLf(t *testing.T) {
	s := startStack(t)
	cmds := []string{"help get", "list --help"}

	t.Run("NoPty_LF_Only", func(t *testing.T) {
		for _, cmd := range cmds {
			_, stderr, exit := s.run(cmd, nil)
			if exit != 0 || !strings.Contains(stderr, "Usage:") {
				t.Fatalf("%q: exit %d stderr %q", cmd, exit, stderr)
			}
			if strings.Contains(stderr, "\r\n") {
				t.Fatalf("no-PTY %q should be LF-only, found CRLF in %q", cmd, stderr)
			}
		}
	})

	t.Run("WithPty_CRLF", func(t *testing.T) {
		for _, cmd := range cmds {
			_, stderr, exit := s.runPty(cmd)
			if exit != 0 {
				t.Fatalf("%q: exit %d", cmd, exit)
			}
			if !strings.Contains(stderr, "\r\n") {
				t.Fatalf("PTY %q should be CRLF, got LF-only %q", cmd, stderr)
			}
		}
	})
}

// usageLine returns the line following the "Usage:" header, trimmed.
func usageLine(body string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "Usage:" && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1]), true
		}
	}
	return "", false
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
