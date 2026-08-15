package ssh_test

// Pins the client-invocation rule across every surface that teaches it: the
// README, the SPEC, and the in-product help. All three drifted apart once, and
// the authoritative-looking one (help) was the wrong one, so a reader who
// trusted generated output over prose got a command that cannot run.
//
// The rule, measured against OpenSSH 10.2p1 and 9.6p1 rather than assumed:
// after ssh consumes the destination it resumes its own option parsing, but
// getopt halts at the first argument not beginning with "-". So a flag is only
// eaten when it is the FIRST token after the host, which is exactly the upload
// case (no verb). Verb commands protect their own flags and must NOT carry a
// terminator.
//
// A deliberate counter-example is exempted by putting WRONG on the line.

import (
	"os"
	"strings"
	"testing"
)

// sshOptTakesValue lists the ssh options that consume the following argument,
// so the destination is identified correctly when one precedes it.
var sshOptTakesValue = map[byte]bool{
	'B': true, 'b': true, 'c': true, 'D': true, 'E': true, 'e': true,
	'F': true, 'I': true, 'i': true, 'J': true, 'L': true, 'l': true,
	'm': true, 'O': true, 'o': true, 'p': true, 'Q': true, 'R': true,
	'S': true, 'W': true, 'w': true,
}

// eatenFlag reports the flag ssh would consume from an invocation, or "" when
// the line is safe. Safe means: no destination, a terminator, or a verb between
// the destination and the flag.
func eatenFlag(cmd string) string {
	toks := strings.Fields(cmd)
	i := 0
	for i < len(toks) && strings.HasPrefix(toks[i], "-") {
		if toks[i] == "--" {
			return "" // terminator before any destination; nothing to judge
		}
		if len(toks[i]) == 2 && sshOptTakesValue[toks[i][1]] {
			i += 2 // option plus its value
			continue
		}
		i++
	}
	if i >= len(toks) {
		return "" // no destination on this line
	}
	i++ // the destination itself
	if i >= len(toks) {
		return ""
	}
	// Only the FIRST token after the destination can be eaten; anything else
	// means getopt already halted on a verb.
	if next := toks[i]; next != "--" && strings.HasPrefix(next, "-") {
		return next
	}
	return ""
}

// sshInvocations pulls each `ssh ...` invocation out of a documentation or help
// line, ignoring ssh-keygen and friends (which are not followed by a space).
func sshInvocations(line string) []string {
	var out []string
	rest := line
	for {
		idx := strings.Index(rest, "ssh ")
		if idx < 0 {
			return out
		}
		// Reject a match inside a longer word (e.g. "ssh-keygen", "mussh").
		if idx > 0 {
			prev := rest[idx-1]
			if prev != ' ' && prev != '|' && prev != '`' && prev != '(' && prev != '\t' {
				rest = rest[idx+4:]
				continue
			}
		}
		out = append(out, rest[idx+4:])
		rest = rest[idx+4:]
	}
}

func TestDocumentedSSHInvocationsAreRunnable(t *testing.T) {
	// server.go carries helpTextTemplate, the surface that was wrong.
	for _, path := range []string{"../../README.md", "../../docs/SPEC.md", "server.go"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "WRONG") {
				continue // documented counter-example
			}
			for _, inv := range sshInvocations(line) {
				if flag := eatenFlag(inv); flag != "" {
					t.Errorf("%s:%d: ssh would consume %q before the server sees it; "+
						"put -- after the host, or a verb before the flag\n  %s",
						path, n+1, flag, strings.TrimSpace(line))
				}
			}
		}
	}
}

// The parser itself, pinned against the forms measured live. Without these the
// test above could pass by failing to recognise anything.
func TestEatenFlagParser(t *testing.T) {
	for _, tc := range []struct {
		cmd, want string
	}{
		// Eaten: the flag is the first token after the destination.
		{`-T hostthis.dev --name "design notes"`, "--name"},
		{`hostthis.dev --output json`, "--output"},
		{`-T hostthis.dev -n x`, "-n"},
		// Safe: terminator present.
		{`-T hostthis.dev -- --name "design notes"`, ""},
		{`-T hostthis.dev -- --type csv`, ""},
		// Safe: a verb halts ssh's parser before the flag.
		{`hostthis.dev list -o json`, ""},
		{`hostthis.dev whoami --output json`, ""},
		{`hostthis.dev versions abc12345 -o json`, ""},
		// Safe: ssh's own options precede the destination.
		{`-p 12222 -o StrictHostKeyChecking=no localhost`, ""},
		{`-T {{apex}}`, ""},
	} {
		if got := eatenFlag(tc.cmd); got != tc.want {
			t.Errorf("eatenFlag(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
