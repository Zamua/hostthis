package ssh

import (
	"strings"

	gossh "github.com/charmbracelet/ssh"
)

// colorContext is the minimal slice of an SSH session that color
// decisions depend on: whether a PTY is allocated and the client's
// declared environment. A separate interface (rather than taking
// gossh.Session directly) lets tests exercise shouldColor without
// constructing a real session, and documents the inputs the rule
// actually consumes.
type colorContext interface {
	Pty() (gossh.Pty, <-chan gossh.Window, bool)
	Environ() []string
}

// shouldColor reports whether the SSH surface should emit ANSI color
// escapes for the given session. The rule follows the universal CLI
// convention (https://no-color.org) plus the long-standing TERM=dumb
// opt-out:
//
//   - No PTY → false. Pipes, redirections, and scripted clients
//     never see escapes; matches the existing PTY-vs-pipe split that
//     emitHelp already uses for CRLF handling.
//   - NO_COLOR set to ANY value (including empty) → false. The
//     no-color.org spec is explicit: presence alone is the signal,
//     the value is irrelevant. We must not require "NO_COLOR=1" or
//     similar.
//   - TERM=dumb → false. Conventional opt-out for users on
//     non-ANSI terminals (emacs M-x shell, screen readers, etc.).
//   - Otherwise → true.
//
// hostthis emits no color today; this helper is the central decision
// point future callers must route through before writing any ANSI
// escape so the convention is honored uniformly from day one.
func shouldColor(sess colorContext) bool {
	if _, _, hasPty := sess.Pty(); !hasPty {
		return false
	}
	for _, kv := range sess.Environ() {
		key, val := splitEnv(kv)
		if key == "NO_COLOR" {
			// Per https://no-color.org: any presence disables.
			// The empty-value case ("NO_COLOR=") still counts.
			_ = val
			return false
		}
		if key == "TERM" && val == "dumb" {
			return false
		}
	}
	return true
}

// splitEnv splits a "KEY=VALUE" pair as exposed by Session.Environ().
// Returns (key, value); when the entry has no "=", returns (entry, "").
func splitEnv(kv string) (string, string) {
	if before, after, ok := strings.Cut(kv, "="); ok {
		return before, after
	}
	return kv, ""
}
