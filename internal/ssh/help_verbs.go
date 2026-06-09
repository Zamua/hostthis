package ssh

import (
	"fmt"
	"strings"

	gossh "github.com/charmbracelet/ssh"
)

// verbDescriptor is the per-verb metadata behind `help <verb>` and
// `<verb> --help`. Keep these short: one paragraph + a signature line
// + one or two example invocations. The full conceptual model lives
// in the global help banner.
type verbDescriptor struct {
	// Name is the lookup key (the verb the user types, or a doc-only
	// alias like "put" / "get" that points back at the canonical
	// upload / show paths).
	Name string
	// Signature is the one-line usage shape, with {{apex}} substituted
	// at render time.
	Signature string
	// Description is the one-paragraph "what it does" body.
	Description string
	// Examples are 1-2 concrete invocations, each line starting with
	// the bare command. {{apex}} is substituted at render time.
	Examples []string
}

// verbDescriptors is the source of truth for per-verb help. New verbs
// added to the dispatcher should add an entry here so `help <verb>`
// and `<verb> --help` stay aligned.
//
// `put` and `get` are doc-only aliases: there is no `put` verb in the
// dispatcher (uploads happen with no explicit verb), and `get` is the
// nickname for `show`. Recognizing them here lets a user reach the
// right help text by either name; the runtime dispatch is unchanged.
var verbDescriptors = map[string]verbDescriptor{
	"put": {
		Name:      "put",
		Signature: "cat <file> | ssh {{apex}} [--name \"label\"] [--type html|md]",
		Description: "Upload new content. Pipe a rendered HTML or Markdown file " +
			"in on stdin; the server prints the URL on stdout and an " +
			"expiry note on stderr. There is no explicit `put` verb: " +
			"upload is the default action when no verb is supplied.",
		Examples: []string{
			"cat index.html | ssh {{apex}}",
			"cat notes.md   | ssh {{apex}} --name \"design notes\"",
		},
	},
	"get": {
		Name:      "get",
		Signature: "ssh {{apex}} show <slug>",
		Description: "Owner-only read-back: streams the stored content for one " +
			"of your pastes to stdout. `get` is a doc alias for `show`; " +
			"the verb the server dispatches on is `show`.",
		Examples: []string{
			"ssh {{apex}} show abc12345 > local.html",
		},
	},
	"show": {
		Name:      "show",
		Signature: "ssh {{apex}} show <slug>",
		Description: "Streams the stored content for one of your pastes to " +
			"stdout. Owner-only; foreign slugs return `not found` (no " +
			"existence-probing).",
		Examples: []string{
			"ssh {{apex}} show abc12345",
			"ssh {{apex}} show abc12345 | less",
		},
	},
	"list": {
		Name:      "list",
		Signature: "ssh {{apex}} list",
		Description: "List your active pastes, soonest-to-expire first. Output " +
			"is tab-separated with a header row on stdout for easy " +
			"`awk`-ing. Empty list prints `no active pastes` on stderr.",
		Examples: []string{
			"ssh {{apex}} list",
			"ssh {{apex}} list | tail -n +2 | awk '{print $1}'",
		},
	},
	"rename": {
		Name:      "rename",
		Signature: "ssh {{apex}} rename <slug> \"<name>\"",
		Description: "Set or change the owner-only label for one of your pastes. " +
			"Pass an empty string to clear. Renaming does not reset the " +
			"7-day expiry clock.",
		Examples: []string{
			"ssh {{apex}} rename abc12345 \"design v4\"",
			"ssh {{apex}} rename abc12345 \"\"",
		},
	},
	"delete": {
		Name:      "delete",
		Signature: "ssh {{apex}} delete <slug> [<ver>]",
		Description: "One-arg form wipes the whole paste (all versions). Two-arg " +
			"form deletes a single historical version's bytes and leaves " +
			"a tombstone row so the version number isn't reused. Cannot " +
			"delete the currently-served version; pin a different version " +
			"first.",
		Examples: []string{
			"ssh {{apex}} delete abc12345",
			"ssh {{apex}} delete abc12345 2",
		},
	},
	"versions": {
		Name:      "versions",
		Signature: "ssh {{apex}} versions <slug>",
		Description: "Show the version timeline for one paste, newest first. The " +
			"middle column marks the currently-served version and any " +
			"tombstoned (deleted) versions. Footer on stderr shows pin " +
			"state and expiry.",
		Examples: []string{
			"ssh {{apex}} versions abc12345",
		},
	},
	"pin": {
		Name:      "pin",
		Signature: "ssh {{apex}} pin <slug> <ver>",
		Description: "Stick the URL to a specific version. Subsequent updates are " +
			"recorded as new versions but the URL keeps serving the " +
			"pinned version until you `unpin` or `pin` a different one. " +
			"Pinning does not reset the expiry clock.",
		Examples: []string{
			"ssh {{apex}} pin abc12345 1",
			"ssh {{apex}} pin abc12345 v3",
		},
	},
	"unpin": {
		Name:      "unpin",
		Signature: "ssh {{apex}} unpin <slug>",
		Description: "Clear the pin: the URL goes back to serving the latest " +
			"version, and future updates publish immediately.",
		Examples: []string{
			"ssh {{apex}} unpin abc12345",
		},
	},
	"whoami": {
		Name:      "whoami",
		Signature: "ssh {{apex}} whoami",
		Description: "Show the identity the server sees for this session (the " +
			"SHA256 fingerprint of the connecting ssh key), the active " +
			"paste count, the quota in use, and the current Sybil-gate " +
			"subnet budget.",
		Examples: []string{
			"ssh {{apex}} whoami",
		},
	},
	"help": {
		Name:      "help",
		Signature: "ssh {{apex}} help [<verb>]",
		Description: "With no argument, print the global help banner listing every " +
			"verb. With a verb, print that verb's signature, description, " +
			"and a couple of examples. `<verb> --help` and `<verb> -h` " +
			"are equivalent to `help <verb>`.",
		Examples: []string{
			"ssh {{apex}} help",
			"ssh {{apex}} help put",
			"ssh {{apex}} pin --help",
		},
	},
}

// lookupVerbDescriptor returns the verb descriptor for name, or false
// if name isn't a known verb (or doc-only alias).
func lookupVerbDescriptor(name string) (verbDescriptor, bool) {
	d, ok := verbDescriptors[name]
	return d, ok
}

// renderVerbHelp produces the verb-help body (LF-terminated lines)
// with {{apex}} substituted. The caller is responsible for the
// PTY-aware CRLF translation; see emitVerbHelp.
func renderVerbHelp(d verbDescriptor, apex string) string {
	var b strings.Builder
	fmt.Fprintln(&b, strings.ReplaceAll(d.Description, "{{apex}}", apex))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Usage:")
	fmt.Fprintln(&b, "    "+strings.ReplaceAll(d.Signature, "{{apex}}", apex))
	if len(d.Examples) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "Examples:")
		for _, ex := range d.Examples {
			fmt.Fprintln(&b, "    "+strings.ReplaceAll(ex, "{{apex}}", apex))
		}
	}
	return b.String()
}

// emitVerbHelp writes the rendered verb-help body to stderr with the
// same PTY-aware CRLF discipline as emitHelp. The trailing newline is
// already in the rendered text, so the PTY path just translates the
// whole block; no extra CRLF is emitted, keeping the output a clean
// block matching renderVerbHelp's byte count.
func emitVerbHelp(sess gossh.Session, apex string, d verbDescriptor) {
	text := renderVerbHelp(d, apex)
	if _, _, hasPty := sess.Pty(); hasPty {
		text = strings.ReplaceAll(text, "\n", "\r\n")
		fmt.Fprint(sess.Stderr(), text)
		return
	}
	_, _ = fmt.Fprint(sess.Stderr(), text)
}

// argvWantsHelp reports whether argv contains a `--help` or `-h` flag
// AFTER the first positional argument. Used by the dispatcher to
// intercept `<verb> --help` and `<verb> -h` before the verb body runs.
//
// Callers should NOT pass argv[0] through this check: the first token
// is the verb itself, and the existing `--help` / `-h` cases in the
// switch handle the no-verb form. This guards against e.g. the upload
// path tripping on `--help` as a flag value.
func argvWantsHelp(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	for _, a := range argv[1:] {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}
