package ssh

import (
	"fmt"
	"strings"

	gossh "github.com/charmbracelet/ssh"
)

// verbDescriptor is the per-verb metadata behind `help <verb>` and
// `<verb> --help`. {{apex}} in Signature and Examples is substituted at render
// time. Keep bodies short: one paragraph, a signature line, one or two
// examples. The conceptual model lives in the global help banner.
type verbDescriptor struct {
	Name        string // lookup key: the verb the user types
	Signature   string
	Description string
	Examples    []string
}

// verbDescriptors is the source of truth for per-verb help. A verb added to the
// dispatcher needs an entry here so `help <verb>` and `<verb> --help` stay
// aligned.
//
// There is no `put` or `show` verb: upload is verbless (the default action
// when no verb is supplied), and reads are `get`.
var verbDescriptors = map[string]verbDescriptor{
	"get": {
		Name:      "get",
		Signature: "ssh {{apex}} get <slug>",
		Description: "Streams the stored content for one of your pastes to " +
			"stdout. Owner-only; foreign slugs return `not found` (no " +
			"existence-probing).",
		Examples: []string{
			"ssh {{apex}} get abc12345",
			"ssh {{apex}} get abc12345 | less",
		},
	},
	"list": {
		Name:      "list",
		Signature: "ssh {{apex}} list [-o json]",
		Description: "List your active pastes, soonest-to-expire first. Empty list " +
			"prints `no active pastes` on stderr.",
		Examples: []string{
			"ssh {{apex}} list",
			"ssh {{apex}} list -o json | jq -r '.[].slug'",
		},
	},
	"url": {
		Name:      "url",
		Signature: "ssh {{apex}} url <slug>",
		Description: "Print just the shareable URL for an existing paste (or " +
			"deployed site) on stdout - handy for re-copying a link without " +
			"re-uploading. No ownership check (the URL is a public " +
			"capability), but a missing or expired slug returns `not found` " +
			"(exit 4).",
		Examples: []string{
			"ssh {{apex}} url abc12345",
			"ssh {{apex}} url abc12345 | pbcopy",
		},
	},
	"qr": {
		Name:      "qr",
		Signature: "ssh {{apex}} qr <slug>",
		Description: "Re-show an existing paste's link as a scannable QR code. " +
			"Mirrors create: the URL goes to stdout and the QR block to " +
			"stderr, so `2>/dev/null` keeps just the URL. No ownership check; " +
			"a missing or expired slug returns `not found` (exit 4).",
		Examples: []string{
			"ssh {{apex}} qr abc12345",
		},
	},
	"rename": {
		Name:      "rename",
		Signature: "ssh {{apex}} rename <slug> \"<name>\"",
		Description: "Set or change the owner-only label for one of your pastes. " +
			"Pass an empty string to clear. Renaming does not reset the " +
			"expiry clock.",
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
		Signature: "ssh {{apex}} versions <slug> [-o json]",
		Description: "Show the version timeline for one paste, newest first. The " +
			"middle column marks the currently-served version and any " +
			"tombstoned (deleted) versions. Footer on stderr shows pin " +
			"state and expiry.",
		Examples: []string{
			"ssh {{apex}} versions abc12345",
			"ssh {{apex}} versions abc12345 -o json | jq '.versions'",
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
		Signature: "ssh {{apex}} whoami [-o json]",
		Description: "Show the identity the server sees for this session (the " +
			"SHA256 fingerprint of the connecting ssh key), the active " +
			"paste count, the quota in use, and the current Sybil-gate " +
			"subnet budget.",
		Examples: []string{
			"ssh {{apex}} whoami",
			"ssh {{apex}} whoami -o json | jq .quota_bytes",
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
			"ssh {{apex}} help get",
			"ssh {{apex}} pin --help",
		},
	},
}

func lookupVerbDescriptor(name string) (verbDescriptor, bool) {
	d, ok := verbDescriptors[name]
	return d, ok
}

// renderVerbHelp produces the verb-help body with {{apex}} substituted, always
// LF-terminated. PTY-aware CRLF translation is the caller's job (emitVerbHelp).
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

// emitVerbHelp writes the verb-help body to stderr with the same PTY-aware
// CRLF discipline as emitHelp. The rendered text already ends in a newline, so
// no extra CRLF is emitted and the output matches renderVerbHelp byte for byte.
func emitVerbHelp(sess gossh.Session, apex string, d verbDescriptor) {
	text := renderVerbHelp(d, apex)
	if _, _, hasPty := sess.Pty(); hasPty {
		text = strings.ReplaceAll(text, "\n", "\r\n")
		fmt.Fprint(sess.Stderr(), text) //nolint:errcheck
		return
	}
	_, _ = fmt.Fprint(sess.Stderr(), text)
}

// argvWantsHelp reports whether argv carries `--help` or `-h` after argv[0],
// letting the dispatcher intercept `<verb> --help` before the verb body runs.
// argv[0] is skipped deliberately: it is the verb itself, and the no-verb form
// is handled by the dispatcher's own `--help` / `-h` cases.
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
