package ssh_test

// Characterization tests pinning the byte-exact, exit-code-exact and PTY-aware
// behavior of every verb in the dispatcher.
//
// Conventions:
//   - One sub-test per spec behavior bullet, each asserting a concrete
//     stdout/stderr/exit shape rather than a vague match.
//   - The fixture is startStack (real metadata repo, blob store, ssh client and
//     ssh server), so assertions exercise the full handler path.
//   - Names are Test<Area>_Characterization_<Case>, so `go test -run
//     Characterization` selects them.

import (
	"net"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
)

// ---------------------------------------------------------------------------
// 1. Upload (put) - default verb, stdin
// ---------------------------------------------------------------------------

func TestUpload_Characterization(t *testing.T) {
	s := startStack(t)

	t.Run("FreshKey_HTML_URLAndQR", func(t *testing.T) {
		stdout, stderr, exit := s.run("", []byte("<!doctype html><h1>hello</h1>"))
		if exit != 0 {
			t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
		}
		url := strings.TrimSpace(stdout)
		// stdout is exactly the URL plus one trailing newline.
		if !strings.HasSuffix(stdout, "\n") {
			t.Fatalf("stdout must end with newline so `... | pbcopy` is clean: %q", stdout)
		}
		if strings.Count(stdout, "\n") != 1 {
			t.Fatalf("stdout must be one line (URL), got %d newlines: %q",
				strings.Count(stdout, "\n"), stdout)
		}
		if !strings.HasPrefix(url, s.httpURL+"/p/") {
			t.Fatalf("expected /p/<slug> URL, got %q", url)
		}
		slug := extractSlug(stdout)
		if _, err := domain.ParseSlug(slug); err != nil {
			t.Fatalf("server returned malformed slug %q: %v", slug, err)
		}
		// stderr carries the URL's QR code, rendered on every create.
		if !strings.ContainsAny(stderr, "█▀▄") {
			t.Fatalf("stderr should contain a rendered QR code, got %q", stderr)
		}
	})

	t.Run("FreshKey_Markdown_URLAndQR", func(t *testing.T) {
		stdout, stderr, exit := s.run("", []byte("# hello\n\nworld"))
		if exit != 0 {
			t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout), s.httpURL+"/p/") {
			t.Fatalf("expected URL, got %q", stdout)
		}
		if !strings.ContainsAny(stderr, "█▀▄") {
			t.Fatalf("expected a QR code on stderr, got %q", stderr)
		}
	})

	t.Run("WithName_StderrIncludesQuotedName", func(t *testing.T) {
		stdout, stderr, exit := s.run(`--name "demo"`, []byte("<!doctype html><h1>x</h1>"))
		if exit != 0 {
			t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout), s.httpURL+"/p/") {
			t.Fatalf("expected URL, got %q", stdout)
		}
		// Pinned stderr format: `"demo".`
		if !strings.Contains(stderr, `"demo".`) {
			t.Fatalf(`expected '"demo".' on stderr, got %q`, stderr)
		}
	})

	t.Run("EmptyStdin_RejectedExit1", func(t *testing.T) {
		// The service returns a plain errors.New for "empty upload", which
		// exitForServiceErr maps through its default branch to 1.
		stdout, stderr, exit := s.run("", []byte(""))
		if exit != 1 {
			t.Fatalf("expected exit 1 for empty body, got %d (stdout=%q stderr=%q)",
				exit, stdout, stderr)
		}
		if !strings.Contains(stderr, "empty upload") {
			t.Fatalf("expected 'empty upload' on stderr, got %q", stderr)
		}
	})

	t.Run("UnsupportedKind_Rejected", func(t *testing.T) {
		// Binary-ish bytes that won't sniff as text/html or markdown.
		stdout, stderr, exit := s.run("", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05})
		if exit == 0 {
			t.Fatalf("expected nonzero exit for unsupported kind (stdout=%q stderr=%q)",
				stdout, stderr)
		}
		if !strings.Contains(stderr, "html, markdown") {
			t.Fatalf("expected unsupported-kind message naming html/markdown, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Update (slug as verb) - appends a version
// ---------------------------------------------------------------------------

func TestUpdate_Characterization(t *testing.T) {
	s := startStack(t)

	t.Run("OwnedSlug_AppendsVersion_StderrShowsVersionNum", func(t *testing.T) {
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		stdout2, stderr2, exit := s.run(slug, []byte("<!doctype html><p>v2</p>"))
		if exit != 0 {
			t.Fatalf("update exit: %d (%q)", exit, stderr2)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout2), s.httpURL+"/p/") {
			t.Fatalf("expected URL on stdout, got %q", stdout2)
		}
		// Pinned update stderr line: "v2 saved."
		if !strings.Contains(stderr2, "v2 saved.") {
			t.Fatalf("expected 'v2 saved.' on stderr, got %q", stderr2)
		}
	})
}

// ---------------------------------------------------------------------------
// 3. List
// ---------------------------------------------------------------------------

func TestList_Characterization(t *testing.T) {
	t.Run("Empty_StderrNotice_Exit0", func(t *testing.T) {
		s := startStack(t)
		stdout, stderr, exit := s.run("list", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if stdout != "" {
			t.Fatalf("empty stdout expected, got %q", stdout)
		}
		if strings.TrimSpace(stderr) != "no active pastes" {
			t.Fatalf("expected exactly 'no active pastes' on stderr, got %q", stderr)
		}
	})

	t.Run("WithPastes_HeaderOnStdoutFirst", func(t *testing.T) {
		s := startStack(t)
		// Two uploads, for a header + N rows shape.
		_, _, _ = s.run(`--name "demo"`, []byte("<!doctype html><p>a</p>"))
		_, _, _ = s.run("", []byte("# md\nbody"))

		stdout, _, exit := s.run("list", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("expected header + 2 rows = 3 lines, got %d: %q", len(lines), stdout)
		}
		// Pinned header columns. Output is space-padded, so match on the field
		// tokens rather than a literal tab-joined string.
		want := []string{"SLUG", "NAME", "SIZE", "KIND", "VERS"}
		if got := strings.Fields(lines[0]); !reflect.DeepEqual(got, want) {
			t.Fatalf("list header drift:\n got: %v\nwant: %v", got, want)
		}
		// Each row has the header's 5 whitespace-separated columns. These
		// fixtures use single-word names and a bare v1 VERS, so field
		// splitting is unambiguous.
		for i, ln := range lines[1:] {
			cols := strings.Fields(ln)
			if len(cols) != len(want) {
				t.Fatalf("row %d has %d cols, want %d: %q", i+1, len(cols), len(want), ln)
			}
		}
	})

	t.Run("UnnamedPaste_NameColumnIsDash", func(t *testing.T) {
		s := startStack(t)
		_, _, _ = s.run("", []byte("<!doctype html><p>x</p>"))
		stdout, _, _ := s.run("list", nil)
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected at least header + 1 row: %q", stdout)
		}
		cols := strings.Fields(lines[1])
		if cols[1] != "-" {
			t.Fatalf("unnamed paste should render name='-', got %q", cols[1])
		}
	})
}

// ---------------------------------------------------------------------------
// 4. Show (read content back)
// ---------------------------------------------------------------------------

func TestShow_Characterization(t *testing.T) {
	t.Run("OwnedSlug_BodyBack_Exit0", func(t *testing.T) {
		s := startStack(t)
		body := []byte("<!doctype html><p>hello</p>")
		stdout, _, _ := s.run("", body)
		slug := extractSlug(stdout)
		out, stderr, exit := s.run("get "+slug, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (stderr=%q)", exit, stderr)
		}
		// Byte-exact: the server adds no trailing newline to the stored body.
		if out != string(body) {
			t.Fatalf("get stdout mismatch:\n got: %q\nwant: %q", out, body)
		}
	})

	t.Run("WellFormedButNonExistentSlug_NotFound_Exit4", func(t *testing.T) {
		s := startStack(t)
		// A syntactically valid slug that does not exist.
		ghost := domain.NewRandomSlug().String()
		_, stderr, exit := s.run("get "+ghost, nil)
		// exitForServiceErr maps ErrNotFound to 4.
		if exit != 4 {
			t.Fatalf("expected exit 4 for not-found, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("expected 'not found' message, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 5. Rename
// ---------------------------------------------------------------------------

func TestRename_Characterization(t *testing.T) {
	t.Run("ValidName_StderrConfirm_Exit0", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		_, stderr, exit := s.run(`rename `+slug+` "label v2"`, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (%q)", exit, stderr)
		}
		if strings.TrimSpace(stderr) != "renamed." {
			t.Fatalf("expected exactly 'renamed.' on stderr, got %q", stderr)
		}
		if listOut, _, _ := s.run("list", nil); !strings.Contains(listOut, "label v2") {
			t.Fatalf("expected new label in list: %q", listOut)
		}
	})

	t.Run("NoLabel_Clears", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		// Omitting the label clears it: the empty-string form cannot survive
		// ssh's argv-join, so no-label is the only invocable clear path.
		if _, _, ex := s.run("rename "+slug+" mylabel", nil); ex != 0 {
			t.Fatalf("set label: exit %d", ex)
		}
		_, stderr, exit := s.run("rename "+slug, nil)
		if exit != 0 {
			t.Fatalf("expected exit 0 clearing the label, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "cleared") {
			t.Fatalf("expected 'label cleared.', got %q", stderr)
		}
	})

	t.Run("InvalidName_Newline_Exit1", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		// validName forbids newlines.
		_, stderr, exit := s.run(`rename `+slug+` "bad`+"\n"+`name"`, nil)
		if exit == 0 {
			t.Fatalf("expected nonzero exit for newline in name, got 0 (%q)", stderr)
		}
		if !strings.Contains(stderr, "1–60") && !strings.Contains(stderr, "printable") {
			t.Fatalf("expected invalid-name message, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. Delete (whole-paste and per-version)
// ---------------------------------------------------------------------------

func TestDelete_Characterization(t *testing.T) {
	t.Run("WholePaste_Exit0_StderrConfirm", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		_, stderr, exit := s.run("delete "+slug, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (%q)", exit, stderr)
		}
		if strings.TrimSpace(stderr) != "deleted." {
			t.Fatalf("expected exactly 'deleted.' on stderr, got %q", stderr)
		}
	})

	t.Run("VersionDelete_FreesBytes_Exit0", func(t *testing.T) {
		s := startStack(t)
		// v1 + v2, so v1 is not the served version and can be freed.
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, _, _ = s.run(slug, []byte("<!doctype html><p>v2 longer body</p>"))
		_, stderr, exit := s.run("delete "+slug+" 1", nil)
		if exit != 0 {
			t.Fatalf("version-delete exit: %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "deleted v1. freed ") {
			t.Fatalf("expected 'deleted v1. freed ...', got %q", stderr)
		}
	})

	t.Run("VersionDelete_AlreadyDeleted_Exit0_Idempotent", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, _, _ = s.run(slug, []byte("<!doctype html><p>v2</p>"))
		// The first delete succeeds; the second is a no-op success per spec.
		_, _, _ = s.run("delete "+slug+" 1", nil)
		_, stderr, exit := s.run("delete "+slug+" 1", nil)
		if exit != 0 {
			t.Fatalf("expected exit 0 for already-deleted re-delete, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "already deleted") {
			t.Fatalf("expected 'already deleted', got %q", stderr)
		}
	})

	t.Run("VersionDelete_CurrentlyServed_Exit2", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		// Only v1 exists, so it is the served version.
		_, stderr, exit := s.run("delete "+slug+" 1", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 deleting currently-served version, got %d (%q)",
				exit, stderr)
		}
		if !strings.Contains(stderr, "currently served") {
			t.Fatalf("expected 'currently served' hint, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 7. Versions + Pin + Unpin
// ---------------------------------------------------------------------------

func TestVersions_Characterization(t *testing.T) {
	t.Run("ListVersions_TableShape_AndFooter", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, _, _ = s.run(slug, []byte("<!doctype html><p>v2</p>"))
		// The table goes to stdout, the footer line to stderr.
		out, stderr, exit := s.run("versions "+slug, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (%q)", exit, stderr)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 version rows on stdout, got %d: %q", len(lines), out)
		}
		// Rows are space-padded: vN  marker  created_at  size. The first row
		// is the latest (v2), so unpinned its marker is 'current'
		// (MAX(non-deleted ver_num) is served). The date field contains
		// spaces, so match leading tokens rather than a fixed count.
		cols := strings.Fields(lines[0])
		if cols[0] != "v2" {
			t.Fatalf("expected v2 as the latest row, got %q", cols[0])
		}
		if cols[1] != "current" {
			t.Fatalf("expected 'current' marker on latest unpinned row, got %q", cols[1])
		}
		// The stderr footer carries the pin state.
		if !strings.Contains(stderr, "unpinned") {
			t.Fatalf("expected 'unpinned' on footer, got %q", stderr)
		}
	})

	t.Run("PinV1_Confirms_AndChangesFooter", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, _, _ = s.run(slug, []byte("<!doctype html><p>v2</p>"))
		_, stderr, exit := s.run("pin "+slug+" 1", nil)
		if exit != 0 {
			t.Fatalf("pin exit: %d (%q)", exit, stderr)
		}
		if strings.TrimSpace(stderr) != "pinned v1." {
			t.Fatalf("expected exactly 'pinned v1.' on stderr, got %q", stderr)
		}
		_, vstderr, _ := s.run("versions "+slug, nil)
		if !strings.Contains(vstderr, "pinned to v1") {
			t.Fatalf("expected 'pinned to v1' in versions footer, got %q", vstderr)
		}
	})

	t.Run("Unpin_StderrConfirm", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, _, _ = s.run(slug, []byte("<!doctype html><p>v2</p>"))
		_, _, _ = s.run("pin "+slug+" 1", nil)
		_, stderr, exit := s.run("unpin "+slug, nil)
		if exit != 0 {
			t.Fatalf("unpin exit: %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "unpinned. URL now serves the latest version.") {
			t.Fatalf("expected unpin confirmation, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 8. Whoami
// ---------------------------------------------------------------------------

func TestWhoami_Characterization(t *testing.T) {
	t.Run("Basic_KeyAndActiveOnStdout", func(t *testing.T) {
		s := startStack(t)
		stdout, stderr, exit := s.run("whoami", nil)
		if exit != 0 {
			t.Fatalf("exit: %d (%q)", exit, stderr)
		}
		if !strings.Contains(stdout, "key:") {
			t.Fatalf("expected 'key:' line on stdout, got %q", stdout)
		}
		// s.keyedOwner (SHA256:hex) must appear verbatim on stdout.
		if !strings.Contains(stdout, s.keyedOwner) {
			t.Fatalf("expected owner fingerprint %q in stdout %q",
				s.keyedOwner, stdout)
		}
		if !strings.Contains(stdout, "active:") {
			t.Fatalf("expected 'active:' line, got %q", stdout)
		}
		if !strings.Contains(stdout, "quota:") {
			t.Fatalf("expected 'quota:' line, got %q", stdout)
		}
	})

	t.Run("AfterOneUpload_ActiveOne", func(t *testing.T) {
		s := startStack(t)
		_, _, _ = s.run("", []byte("<!doctype html><p>x</p>"))
		stdout, _, _ := s.run("whoami", nil)
		if !strings.Contains(stdout, "active:  1 paste(s)") {
			t.Fatalf("expected 'active:  1 paste(s)' on stdout, got %q", stdout)
		}
	})
}

// ---------------------------------------------------------------------------
// 9. Help (PTY-aware)
// ---------------------------------------------------------------------------

func TestHelp_Characterization(t *testing.T) {
	s := startStack(t)

	t.Run("HelpVerb_NoPty_LF", func(t *testing.T) {
		// sess.Run() allocates no PTY, so help emits LF-terminated lines.
		_, stderr, exit := s.run("help", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("expected help body, got %q", stderr)
		}
		if strings.Contains(stderr, "\r\n") {
			t.Fatalf("help over no-PTY session should be LF-only, found CRLF in %q", stderr)
		}
	})

	t.Run("HelpVerb_WithPty_CRLF", func(t *testing.T) {
		// With a PTY, emitHelp translates LF to CRLF so the client's raw
		// terminal renders without the staircase effect.
		_, stderr, exit := s.runPty("help")
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "\r\n") {
			t.Fatalf("help over PTY session should be CRLF, got LF-only %q", stderr)
		}
	})

	t.Run("DashHelpFlag_SameAsHelp", func(t *testing.T) {
		_, stderr, exit := s.run("--help", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("expected help body for --help, got %q", stderr)
		}
	})

	t.Run("DashHFlag_SameAsHelp", func(t *testing.T) {
		_, stderr, exit := s.run("-h", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("expected help body for -h, got %q", stderr)
		}
	})

	t.Run("UnknownVerb_PrefixesErrorThenHelp_Exit2", func(t *testing.T) {
		_, stderr, exit := s.run("totallybogus", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for unknown verb, got %d", exit)
		}
		if !strings.Contains(stderr, `unknown command "totallybogus"`) {
			t.Fatalf("expected canonical unknown-command line, got %q", stderr)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("expected help after error, got %q", stderr)
		}
	})

	t.Run("PtyOnly_NoArgs_ShowsHelp", func(t *testing.T) {
		// Empty argv WITH a PTY shows help; without a PTY it is an upload.
		_, stderr, exit := s.runPty("")
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("PTY + empty cmd should show help, got %q", stderr)
		}
	})

	t.Run("HelpBody_MentionsApex", func(t *testing.T) {
		_, stderr, _ := s.run("help", nil)
		// The fixture's apex is "paste.test": helpTextTemplate must keep
		// substituting it.
		if !strings.Contains(stderr, "paste.test") {
			t.Fatalf("help should mention apex 'paste.test', got %q", stderr)
		}
	})

	t.Run("HelpVerb_NoPty_ByteExactGolden", func(t *testing.T) {
		// The full stderr byte content of the no-PTY help banner. The
		// substring assertions above catch big drifts but miss
		// single-character edits, which read as a silent regression on a
		// narrow terminal.
		_, stderr, exit := s.run("help", nil)
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if stderr != expectedHelpNoPty_PasteTest {
			t.Fatalf("help banner drift on no-PTY render:\n got %d bytes:\n%q\nwant %d bytes:\n%q",
				len(stderr), stderr, len(expectedHelpNoPty_PasteTest), expectedHelpNoPty_PasteTest)
		}
	})
}

// expectedHelpNoPty_PasteTest is the byte-exact help banner emitted to stderr
// with no PTY and apex "paste.test". emitHelp's no-PTY path uses fmt.Fprintln,
// which appends a single trailing "\n", so the golden ends with one LF after
// the closing period.
const expectedHelpNoPty_PasteTest = "Pipe a rendered file in, get a URL out. Pastes persist indefinitely.\n" +
	"\n" +
	"UPLOAD  (-T silences the ssh pseudo-terminal warning on piped uploads;\n" +
	"         -- is required before an UPLOAD flag, or ssh parses it as its\n" +
	"         own; verb flags like -o json are already safe;\n" +
	"         a QR code of the URL also prints to stderr on success)\n" +
	"\n" +
	"    cat foo.html   | ssh -T paste.test\n" +
	"    cat doc.md     | ssh -T paste.test -- --name \"design notes\"\n" +
	"    git diff       | ssh -T paste.test                 rendered as a diff\n" +
	"    cat chart.mmd  | ssh -T paste.test                 mermaid diagram\n" +
	"    cat report.pdf | ssh -T paste.test                 paged pdf viewer\n" +
	"    cat sales.csv  | ssh -T paste.test                 sortable table + SQL\n" +
	"    cat events.json| ssh -T paste.test                 collapsible tree\n" +
	"    cat cpu.folded | ssh -T paste.test                 interactive flame graph\n" +
	"    cat app.ndjson | ssh -T paste.test                 log viewer (query + histogram)\n" +
	"    cat nginx.conf | ssh -T paste.test                 text with linkable lines\n" +
	"    cat x.txt      | ssh -T paste.test -- --type csv   force a renderer\n" +
	"\n" +
	"UPDATE & MANAGE (owner only; ssh key authenticates)\n" +
	"\n" +
	"    cat foo.html | ssh -T paste.test <slug>   replace bytes; URL stays the same\n" +
	"    ssh paste.test list                       all your active pastes\n" +
	"    ssh paste.test get <slug>                 read content back\n" +
	"    ssh paste.test url <slug>                 re-show the URL (no QR)\n" +
	"    ssh paste.test qr <slug>                  re-show the URL + QR code\n" +
	"    ssh paste.test rename <slug> \"label\"      set / change owner label\n" +
	"    ssh paste.test delete <slug> [<ver>]      wipe the paste, or tombstone one version\n" +
	"    ssh paste.test whoami                     identity + active count + quota\n" +
	"\n" +
	"VERSION HISTORY\n" +
	"\n" +
	"    ssh paste.test versions <slug>            timeline of every version\n" +
	"    ssh paste.test pin <slug> <ver>           stick the URL to <ver> (survives updates)\n" +
	"    ssh paste.test unpin <slug>               URL follows latest again\n" +
	"\n" +
	"LINK TO A PLACE INSIDE A PASTE\n" +
	"\n" +
	"    <url>#some-heading      markdown: jump to that heading\n" +
	"    <url>#page=3            pdf: jump to that page\n" +
	"    <url>#row=42            csv: jump to that row\n" +
	"    <url>#F1L42-L48         diff: highlight lines 42-48 of file 1\n" +
	"    <url>#focus=main;serve  flamegraph: zoom to that stack\n" +
	"    <url>#q=malloc          flamegraph: highlight matching frames\n" +
	"    <url>#L42-L48           text / log: highlight those lines\n" +
	"    <url>#q=level=error     log: a saved query, time window and selection\n" +
	"                            all live in the fragment, so a filtered view\n" +
	"                            is just a link\n" +
	"\n" +
	"    Clicking a heading, row, page, diff line number, or flame frame updates\n" +
	"    the URL, so the address bar always holds a link you can copy. To select\n" +
	"    a range of diff lines: shift-click a second line number, or on a touch\n" +
	"    screen just tap it.\n" +
	"\n" +
	"OUTPUT\n" +
	"\n" +
	"    list, versions, whoami accept -o json\n" +
	"\n" +
	"STATIC SITES\n" +
	"\n" +
	"    tar czf - site/ | ssh -T paste.test        deploy a multi-file site\n" +
	"    tar czf - site/ | ssh -T paste.test <slug> re-deploy in place\n" +
	"\n" +
	"LIMITS\n" +
	"\n" +
	"    100 MiB per identity, counting post-compression bytes across all\n" +
	"    your active pastes. HTML, Markdown, diff, Mermaid, PDF, CSV, JSON,\n" +
	"    folded stacks, NDJSON logs, plain text, or a gzip-tar site archive.\n" +
	"\n" +
	"    Apps can persist + sync state: https://paste.test/  (rooms, realtime + push API)\n"

// ---------------------------------------------------------------------------
// 10. Auth refusal + Sybil refusal
// ---------------------------------------------------------------------------

func TestAuth_Characterization(t *testing.T) {
	t.Run("PasswordOnlyClient_Exit3_KeyRequired", func(t *testing.T) {
		s := startStack(t)
		// The gliderlabs handler accepts password auth but it yields no
		// public key, so handleSession refuses with exit 3.
		_, stderr, exit := s.runAnon("whoami", nil)
		if exit != 3 {
			t.Fatalf("expected exit 3 for keyless session, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "ssh key required") {
			t.Fatalf("expected 'ssh key required' nudge, got %q", stderr)
		}
		if !strings.Contains(stderr, "ssh-keygen") {
			t.Fatalf("expected ssh-keygen hint, got %q", stderr)
		}
	})
}

func TestSybilGate_Characterization(t *testing.T) {
	// Cap at 2: the first two fresh keys from a subnet are admitted, the third
	// refused. Loopback ssh traffic all shares 127.0.0.0/24, so this drives
	// the production path end to end.
	t.Run("ThirdFreshKey_Exit6_AndRichMessage", func(t *testing.T) {
		g := startStack(t, withKeyGate(2))
		c1, _ := newKeyClient(t, g.sshAddr)
		_, _, e1 := g.runOn(c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first key should be admitted, got exit %d", e1)
		}
		c2, _ := newKeyClient(t, g.sshAddr)
		_, _, e2 := g.runOn(c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second key should be admitted, got exit %d", e2)
		}
		c3, _ := newKeyClient(t, g.sshAddr)
		_, stderr, e3 := g.runOn(c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third key should be refused with exit 6, got %d (%q)", e3, stderr)
		}
		// The SybilRefusal path prints subnet + cap usage.
		if !strings.Contains(stderr, "too many new keys from this network today") {
			t.Fatalf("expected canonical sybil refusal line, got %q", stderr)
		}
		if !strings.Contains(stderr, "subnet ") {
			t.Fatalf("expected 'subnet ...' detail, got %q", stderr)
		}
		if !strings.Contains(stderr, "to get in:") {
			t.Fatalf("expected guidance block, got %q", stderr)
		}
	})

	t.Run("ReturningKey_AdmittedEvenWhenSubnetFull", func(t *testing.T) {
		// Cap at 1: two sessions using the SAME key from the same subnet are
		// both admitted, because the second reuses an existing
		// (identity, subnet) row and never consults the cap.
		g := startStack(t, withKeyGate(1))
		_, cfg := freshKeyConfig(t)
		dial := func() *xssh.Client {
			cli, err := xssh.Dial("tcp", g.sshAddr, cfg)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = cli.Close() })
			return cli
		}
		c1 := dial()
		_, _, e1 := g.runOn(c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first session: exit %d", e1)
		}
		c2 := dial()
		_, _, e2 := g.runOn(c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("returning-key session should be admitted past the cap, got exit %d", e2)
		}
	})
}

// ---------------------------------------------------------------------------
// 11. PROXY protocol (HOSTTHIS_SSH_PROXY_PROTOCOL=true)
// ---------------------------------------------------------------------------

func TestProxyProtocol_Characterization(t *testing.T) {
	t.Run("RealClientIPDistinguishesSubnetsForSybilGate", func(t *testing.T) {
		// Cap at 1 fresh key per subnet. Without PROXY parsing both fresh
		// keys would come from 127.0.0.0/24 and the second would be rejected;
		// admitting both from different /24s proves the gate sees the proxied
		// IP.
		p := startStack(t, withProxyProto(), withKeyGate(1))
		c1 := dialWithProxy(t, p.sshAddr, "TCP4", "203.0.113.10", 50000)
		_, _, e1 := p.runOn(c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first proxied client (203.0.113.0/24) should be admitted, got exit %d", e1)
		}
		c2 := dialWithProxy(t, p.sshAddr, "TCP4", "198.51.100.10", 50001)
		_, _, e2 := p.runOn(c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second proxied client (198.51.100.0/24) should be admitted on a different subnet, got exit %d", e2)
		}
		// A second fresh key from the FIRST proxied subnet is refused: that
		// subnet's slot is full.
		c3 := dialWithProxy(t, p.sshAddr, "TCP4", "203.0.113.20", 50002)
		_, stderr, e3 := p.runOn(c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third proxied client from the full subnet should hit Sybil refusal, got exit %d (%q)",
				e3, stderr)
		}
		if !strings.Contains(stderr, "203.0.113.0/24") {
			t.Fatalf("expected the proxied subnet '203.0.113.0/24' in the refusal, got %q", stderr)
		}
	})
}

// A headerless connection is refused when PROXY parsing is enabled: only the
// reverse proxy should reach this listener and it always sends a header, so
// serving a bare connection would attribute the client to the proxy itself.
func TestProxyProtocol_HeaderlessConnectionRefused(t *testing.T) {
	p := startStack(t, withProxyProto(), withKeyGate(10))
	c, err := net.DialTimeout("tcp", p.sshAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck
	// An SSH client hello, not a PROXY header. The listener must refuse the
	// connection rather than run a handshake attributed to 127.0.0.1.
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("SSH-2.0-probe\r\n"))
	buf := make([]byte, 64)
	n, rerr := c.Read(buf)
	if rerr == nil && strings.HasPrefix(string(buf[:n]), "SSH-") {
		t.Fatalf("headerless connection was served an SSH banner %q; REQUIRE policy is not in effect", string(buf[:n]))
	}
}

// ---------------------------------------------------------------------------
// 12. PTY/no-PTY behavior on a second verb (list)
// ---------------------------------------------------------------------------

func TestPty_Characterization_List(t *testing.T) {
	s := startStack(t)
	// Two pastes so list has rows on stdout.
	_, _, _ = s.run("", []byte("<!doctype html><p>a</p>"))
	_, _, _ = s.run("", []byte("<!doctype html><p>b</p>"))

	t.Run("NoPty_StdoutLF", func(t *testing.T) {
		stdout, _, _ := s.run("list", nil)
		if strings.Contains(stdout, "\r\n") {
			t.Fatalf("list stdout over no-PTY session should be LF-only, found CRLF: %q", stdout)
		}
	})

	t.Run("WithPty_StdoutSeesCR", func(t *testing.T) {
		// With a PTY allocated, the server's PTY layer cooks outbound newlines
		// to CR+LF on the way to the client.
		stdout, _, _ := s.runPty("list")
		if !strings.Contains(stdout, "\r") {
			t.Fatalf("list stdout over PTY session should contain CR, got %q", stdout)
		}
	})
}

// ---------------------------------------------------------------------------
// 13. Exit-code matrix: every distinct code the SSH layer emits
// ---------------------------------------------------------------------------

func TestExitCodes_Characterization(t *testing.T) {
	s := startStack(t)
	// State so every case below is reachable.
	stdoutA, _, _ := s.run("", []byte("<!doctype html><p>a</p>"))
	slugA := extractSlug(stdoutA)
	ghost := domain.NewRandomSlug().String()

	cases := []struct {
		name   string
		anon   bool
		cmd    string
		stdin  []byte
		want   int
		stderr string // substring, or the whole stream when exact
		exact  bool
	}{
		{name: "ExitCode0_HelpSuccess", cmd: "help", want: 0},
		{name: "ExitCode0_WhoamiSuccess", cmd: "whoami", want: 0},
		{name: "ExitCode2_UnknownVerb", cmd: "wibble", want: 2, stderr: `unknown command "wibble"`},
		{name: "ExitCode2_GetMissingSlug", cmd: "get", want: 2, stderr: "hostthis:"},
		{name: "ExitCode2_GetInvalidSlug", cmd: "get BAD", want: 2},
		{name: "ExitCode2_URLInvalidSlug", cmd: "url not-a-valid-slug", want: 2},
		{name: "ExitCode2_RenameInvalidSlug", cmd: `rename BAD "label"`, want: 2, stderr: "invalid slug"},
		{name: "ExitCode2_DeleteNoArgs", cmd: "delete", want: 2, stderr: "usage: delete <slug>"},
		{name: "ExitCode2_DeleteTooManyArgs", cmd: "delete a b c", want: 2, stderr: "usage: delete <slug>"},
		{name: "ExitCode2_DeleteInvalidVer", cmd: "delete " + slugA + " notanumber", want: 2, stderr: "invalid version"},
		{name: "ExitCode2_PinMissingArgs", cmd: "pin", want: 2, stderr: "usage: pin <slug> <ver-num>"},
		{name: "ExitCode2_PinInvalidSlug", cmd: "pin BAD 1", want: 2, stderr: "invalid slug"},
		{name: "ExitCode2_PinInvalidVer", cmd: "pin " + slugA + " 0", want: 2, stderr: "invalid version"},
		{name: "ExitCode2_UnpinMissingSlug", cmd: "unpin", want: 2},
		{name: "ExitCode2_UnknownOutputFormat", cmd: "list -o yaml", want: 2, stderr: "unknown output format"},
		// A leading flag routes straight into parseUploadFlags, whose message
		// the SSH layer prefixes with "hostthis: ". `--name foo bar` is NOT an
		// error: --name greedily joins the remaining tokens into one label.
		{name: "ExitCode2_NameNoValue", cmd: "--name", want: 2, stderr: "hostthis: --name needs a value\n", exact: true},
		{name: "ExitCode2_TypeNoValue", cmd: "--type", want: 2, stderr: "hostthis: --type needs a value\n", exact: true},
		{name: "ExitCode2_UnexpectedArgument", cmd: "--type html bad", want: 2, stderr: "hostthis: unexpected argument \"bad\"\n", exact: true},
		{name: "ExitCode3_KeylessSession", anon: true, cmd: "whoami", want: 3, stderr: "ssh key required"},
		{name: "ExitCode4_GetGhostSlug", cmd: "get " + ghost, want: 4, stderr: "not found"},
		{name: "ExitCode4_UpdateGhostSlug", cmd: ghost, stdin: []byte("<!doctype html><p>x</p>"), want: 4, stderr: "not found"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := s.run
			if tc.anon {
				run = s.runAnon
			}
			stdout, stderr, exit := run(tc.cmd, tc.stdin)
			if exit != tc.want {
				t.Fatalf("cmd %q: got exit %d, want %d\n  stdout: %q\n  stderr: %q",
					tc.cmd, exit, tc.want, stdout, stderr)
			}
			if tc.want != 0 && stdout != "" {
				t.Fatalf("cmd %q: a failure must leave stdout empty, got %q", tc.cmd, stdout)
			}
			if tc.exact && stderr != tc.stderr {
				t.Fatalf("cmd %q: stderr drift:\n got: %q\nwant: %q", tc.cmd, stderr, tc.stderr)
			}
			if !tc.exact && !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("cmd %q: stderr %q lacks %q", tc.cmd, stderr, tc.stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 15. Owner-collapse: NotOwner is indistinguishable from NotFound at the SSH
//     boundary, across every owner-gated verb
// ---------------------------------------------------------------------------

func TestOwnerCollapse_Characterization(t *testing.T) {
	// requireOwner returns ErrNotFound whenever the slug belongs to a
	// different identity, so a foreign-slug verb always exits 4, never 5.
	// Surfacing not-owner distinctly would require a new sentinel, an
	// exitForServiceErr branch and a new exit-code 5 test, which makes
	// changing this an explicit policy decision rather than a silent one.
	s := startStack(t)

	// Identity A creates a paste.
	stdoutA, _, _ := s.run("", []byte("<!doctype html><p>owned by A</p>"))
	slugA := extractSlug(stdoutA)
	// v2, so there is something to delete by version and something to pin.
	_, _, _ = s.run(slugA, []byte("<!doctype html><p>v2</p>"))

	// Identity B: a different keyed client on the same server.
	otherClient, _ := newKeyClient(t, s.sshAddr)

	cases := []struct {
		name string
		cmd  string
		body []byte
	}{
		{name: "Show_ForeignSlug", cmd: "get " + slugA},
		{name: "Rename_ForeignSlug", cmd: `rename ` + slugA + ` "hijack"`},
		{name: "Delete_ForeignSlug", cmd: "delete " + slugA},
		{name: "DeleteVersion_ForeignSlug", cmd: "delete " + slugA + " 1"},
		{name: "Pin_ForeignSlug", cmd: "pin " + slugA + " 1"},
		{name: "Unpin_ForeignSlug", cmd: "unpin " + slugA},
		{name: "Versions_ForeignSlug", cmd: "versions " + slugA},
		{name: "Update_ForeignSlug", cmd: slugA, body: []byte("<!doctype html><p>x</p>")},
	}

	// Every verb reports the SAME bytes a plain not-found does, with no
	// internal layer name in them, so no verb is a side channel.
	_, wantStderr, _ := s.run("get "+domain.NewRandomSlug().String(), nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := s.runOn(otherClient, tc.cmd, tc.body)
			if exit != 4 {
				t.Fatalf("foreign %s expected exit 4 (NotFound, NOT 5/NotOwner), got %d (%q)",
					tc.name, exit, stderr)
			}
			if stdout != "" {
				t.Fatalf("foreign %s leaked onto stdout: %q", tc.name, stdout)
			}
			if stderr != wantStderr {
				t.Fatalf("foreign %s stderr %q must match the ghost-slug not-found %q",
					tc.name, stderr, wantStderr)
			}
			if strings.Contains(stderr, "service:") {
				t.Fatalf("foreign %s leaks the service layer name: %q", tc.name, stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 17. Sybil gate - IPv6 (/48) subnet path via PROXY protocol v1 TCP6
// ---------------------------------------------------------------------------

func TestProxyProtocol_IPv6_SybilCharacterization(t *testing.T) {
	// KeyGate buckets IPv6 by /48 (ipSubnet in server.go): three fresh keys
	// from the SAME /48 refuse the third, while a fresh key from a DIFFERENT
	// /48 succeeds.
	t.Run("SameSlash48_ThirdRefused_DifferentSlash48_Admitted", func(t *testing.T) {
		p := startStack(t, withProxyProto(), withKeyGate(2))

		// Three addresses in 2001:db8:1::/48, differing only in the lower 80
		// bits, which ipSubnet buckets together.
		c1 := dialWithProxy(t, p.sshAddr, "TCP6", "2001:db8:1::aa", 50000)
		_, _, e1 := p.runOn(c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first IPv6 client in 2001:db8:1::/48 should be admitted, got exit %d", e1)
		}
		c2 := dialWithProxy(t, p.sshAddr, "TCP6", "2001:db8:1:1234::bb", 50001)
		_, _, e2 := p.runOn(c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second IPv6 client in same /48 should be admitted (cap=2), got exit %d", e2)
		}
		// A third fresh key with different lower bits but the SAME /48.
		c3 := dialWithProxy(t, p.sshAddr, "TCP6", "2001:db8:1:ffff::cc", 50002)
		_, stderr3, e3 := p.runOn(c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third IPv6 client in full /48 should hit Sybil refusal (exit 6), got %d (%q)",
				e3, stderr3)
		}
		if !strings.Contains(stderr3, "too many new keys from this network today") {
			t.Fatalf("expected canonical sybil refusal, got %q", stderr3)
		}
		// The subnet detail line names the IPv6 /48. Only the prefix is
		// asserted: the exact mask canonicalization of 2001:db8:1:: is
		// net.IP.Mask's business.
		if !strings.Contains(stderr3, "/48") {
			t.Fatalf("expected '/48' in IPv6 subnet detail, got %q", stderr3)
		}
		if !strings.Contains(stderr3, "2001:db8:1") {
			t.Fatalf("expected '2001:db8:1' prefix in IPv6 subnet detail, got %q", stderr3)
		}

		// A fresh key from 2001:db8:2::/48 gets in.
		c4 := dialWithProxy(t, p.sshAddr, "TCP6", "2001:db8:2::dd", 50003)
		_, _, e4 := p.runOn(c4, "whoami", nil)
		if e4 != 0 {
			t.Fatalf("fresh key from a different /48 should be admitted, got exit %d (Sybil gate is per-/48)", e4)
		}
	})
}

// ---------------------------------------------------------------------------
// 18. Sybil refusal - "(b) wait until <timestamp>" line shape
// ---------------------------------------------------------------------------

func TestSybil_WaitUntilLine_Characterization(t *testing.T) {
	// The refusal's enrichment path emits a "(b) wait until
	// <YYYY-MM-DD HH:MM UTC> - the oldest entry ages out then" line. The
	// prefix is pinned byte-exact and the tail by regex, since the timestamp
	// is now+window and cannot be pinned literally.
	g := startStack(t, withKeyGate(2))
	c1, _ := newKeyClient(t, g.sshAddr)
	_, _, _ = g.runOn(c1, "whoami", nil)
	c2, _ := newKeyClient(t, g.sshAddr)
	_, _, _ = g.runOn(c2, "whoami", nil)
	c3, _ := newKeyClient(t, g.sshAddr)
	_, stderr, e3 := g.runOn(c3, "whoami", nil)
	if e3 != 6 {
		t.Fatalf("third key should be refused, got exit %d", e3)
	}
	// The server emits "  (b) wait until %s - the oldest entry ages out
	// then\n", where %s is now+window as "2006-01-02 15:04 UTC". The 2-space
	// indent and literal prefix are part of the pin.
	const wantPrefix = "  (b) wait until "
	_, after, ok := strings.Cut(stderr, wantPrefix)
	if !ok {
		t.Fatalf("missing '(b) wait until ' line in refusal:\n%q", stderr)
	}
	rest := after
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		t.Fatalf("'(b) wait until ' line is unterminated: %q", rest)
	}
	line := rest[:nl]
	// The timestamp tail: YYYY-MM-DD HH:MM UTC followed by the trailing
	// " - the oldest entry ages out then" sentence.
	wantTail := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC - the oldest entry ages out then$`)
	if !wantTail.MatchString(line) {
		t.Fatalf("'(b) wait until ' tail drift:\n got: %q\nwant pattern: %q",
			line, wantTail.String())
	}
}
