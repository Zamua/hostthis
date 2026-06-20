package ssh_test

// Characterization tests for the SSH surface.
//
// These tests pin the byte-exact, exit-code-exact, and PTY-aware
// behavior of every verb in the current dispatcher. They are the
// Phase A safety net for the Phase B charmbracelet/wish migration
// (mechanical refactor, supposed to be 100% behavior-preserving) and
// the Phase C polish bundle (additive only). Both phases rely on
// these tests catching any accidental drift.
//
// Conventions:
//   - One sub-test per behavior bullet in the spec; each asserts a
//     concrete stdout/stderr/exit shape rather than a vague match.
//   - The fixture builds on the existing startStack pattern: real
//     sqlite + real blob store + real ssh client + real ssh server,
//     so the assertions exercise the full handler path the way
//     production does.
//   - Tests are named Test<Area>_Characterization_<Case> so they run
//     under `go test -run Characterization`.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// ---------------------------------------------------------------------------
// Fixture variants
// ---------------------------------------------------------------------------

// gatedStack wraps a stack-like bundle with a live KeyGate so Sybil
// behavior can be characterized. We don't reuse the existing stack
// helper because it intentionally leaves KeyGate nil to keep other
// tests deterministic; characterizing the gate requires it set.
type gatedStack struct {
	t           *testing.T
	httpURL     string
	sshAddr     string
	repo        *storage.PasteRepo
	keyGateRepo *storage.KeyGateRepo
	keyGate     *service.KeyGate
}

// startGatedStack stands up a full hostthisd-style stack with a live
// KeyGate using the provided per-subnet limit (window fixed at 24h).
func startGatedStack(t *testing.T, freshKeysPerSubnet int) *gatedStack {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storage.NewPasteRepo(db)
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	manage := service.NewManage(repo, blobUnit)
	kgRepo := storage.NewKeyGateRepo(db)
	keyGate := service.NewKeyGate(kgRepo)
	keyGate.MaxFreshKeysPerSubnet = freshKeysPerSubnet
	manage.KeyGate = keyGate

	httpSrv := httptest.NewServer((&httpapi.Server{Pastes: repo, Blobs: blobUnit}).Handler())
	t.Cleanup(httpSrv.Close)

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close()

	sshSrv := &hostssh.Server{
		Addr:       addr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Manage:     manage,
		KeyGate:    keyGate,
		BuildURL: func(s domain.Slug) string {
			return httpSrv.URL + "/p/" + s.String()
		},
		Logger: log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)
	return &gatedStack{
		t:           t,
		httpURL:     httpSrv.URL,
		sshAddr:     addr,
		repo:        repo,
		keyGateRepo: kgRepo,
		keyGate:     keyGate,
	}
}

// dialKeyed opens a fresh ssh client with a fresh ed25519 key.
func dialKeyed(t *testing.T, addr string) (*xssh.Client, string) {
	t.Helper()
	return newKeyClient(t, addr)
}

// runCmd is the same shape as stack.runOn - issues one ssh command,
// returns (stdout, stderr, exit). The body argument is optional.
func runCmd(t *testing.T, cli *xssh.Client, cmd string, stdin []byte) (string, string, int) {
	t.Helper()
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	exit := 0
	if err := sess.Run(cmd); err != nil {
		var ee *xssh.ExitError
		if asExitErr(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			t.Fatalf("run %q: %v\nstderr: %s", cmd, err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), exit
}

// runCmdWithPty is runCmd but with a PTY allocated. Used to pin the
// PTY-vs-pipe rendering split for the help text.
func runCmdWithPty(t *testing.T, cli *xssh.Client, cmd string) (string, string, int) {
	t.Helper()
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	// Request a tiny PTY. The size + terminal-mode flags are arbitrary;
	// only the presence of the PTY changes server behavior.
	modes := xssh.TerminalModes{
		xssh.ECHO:          0,
		xssh.TTY_OP_ISPEED: 14400,
		xssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm", 24, 80, modes); err != nil {
		t.Fatalf("requestpty: %v", err)
	}
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	exit := 0
	if err := sess.Run(cmd); err != nil {
		var ee *xssh.ExitError
		if asExitErr(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			t.Fatalf("run-pty %q: %v\nstderr: %s", cmd, err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), exit
}

// ---------------------------------------------------------------------------
// 1. Upload (put) - default verb, stdin
// ---------------------------------------------------------------------------

func TestUpload_Characterization(t *testing.T) {
	s := startStack(t)

	t.Run("FreshKey_HTML_URLAndExpiryNote", func(t *testing.T) {
		stdout, stderr, exit := s.run("", []byte("<!doctype html><h1>hello</h1>"))
		if exit != 0 {
			t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
		}
		url := strings.TrimSpace(stdout)
		// stdout is exactly the URL plus a trailing newline.
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
		// stderr is exactly the "expires in 7 days" line.
		if strings.TrimSpace(stderr) != "expires in 7 days" {
			t.Fatalf("stderr should be exactly 'expires in 7 days', got %q", stderr)
		}
	})

	t.Run("FreshKey_Markdown_URLAndExpiryNote", func(t *testing.T) {
		stdout, stderr, exit := s.run("", []byte("# hello\n\nworld"))
		if exit != 0 {
			t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout), s.httpURL+"/p/") {
			t.Fatalf("expected URL, got %q", stdout)
		}
		if !strings.Contains(stderr, "expires in 7 days") {
			t.Fatalf("expected expiry note, got %q", stderr)
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
		// stderr format pinned: `"demo". expires in 7 days`.
		if !strings.Contains(stderr, `"demo". expires in 7 days`) {
			t.Fatalf(`expected '"demo". expires in 7 days', got %q`, stderr)
		}
	})

	t.Run("EmptyStdin_RejectedExit1", func(t *testing.T) {
		// "empty upload" → service returns a plain errors.New; the SSH
		// surface maps it through the default branch of exitForServiceErr,
		// which returns 1.
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
		// Update stderr line: "v2 saved. expires in 7 days".
		if !strings.Contains(stderr2, "v2 saved. expires in 7 days") {
			t.Fatalf("expected 'v2 saved. expires in 7 days' on stderr, got %q", stderr2)
		}
	})

	t.Run("ForeignSlug_NotFound", func(t *testing.T) {
		// A different identity uploads first.
		other := startStack(t)
		stdout, _, _ := other.run("", []byte("<!doctype html><p>foreign</p>"))
		foreignSlug := extractSlug(stdout)
		// The current stack's identity tries to update it. Both stacks
		// use disjoint sqlite dbs so the slug literally doesn't exist
		// in `s`, but the assertion is the same as "wrong owner": a
		// not-found shape. ParseSlug succeeds so the dispatcher routes
		// through verbUpload's update path.
		_, stderr, exit := s.run(foreignSlug, []byte("<!doctype html><p>x</p>"))
		if exit == 0 {
			t.Fatalf("foreign-slug update should fail, got exit 0 (%q)", stderr)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("expected 'not found' on stderr, got %q", stderr)
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
		// Pinned to the exact stderr line.
		if strings.TrimSpace(stderr) != "no active pastes" {
			t.Fatalf("expected exactly 'no active pastes' on stderr, got %q", stderr)
		}
	})

	t.Run("WithPastes_HeaderOnStdoutFirst", func(t *testing.T) {
		s := startStack(t)
		// Two uploads so we can see a header + N rows shape.
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
		// Pinned header columns. Phase B / C must not reorder these.
		want := "SLUG\tNAME\tSIZE\tKIND\tEXPIRES_IN\tVERS"
		if lines[0] != want {
			t.Fatalf("list header drift:\n got: %q\nwant: %q", lines[0], want)
		}
		// Each row has the same number of tab-separated columns as
		// the header (6).
		for i, ln := range lines[1:] {
			cols := strings.Split(ln, "\t")
			if len(cols) != 6 {
				t.Fatalf("row %d has %d cols, want 6: %q", i+1, len(cols), ln)
			}
		}
	})

	t.Run("UnnamedPaste_NameColumnIsDash", func(t *testing.T) {
		s := startStack(t)
		_, _, _ = s.run("", []byte("<!doctype html><p>x</p>"))
		stdout, _, _ := s.run("list", nil)
		// Row 1 (index 1 after header) name column should be "-".
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected at least header + 1 row: %q", stdout)
		}
		cols := strings.Split(lines[1], "\t")
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
		out, stderr, exit := s.run("show "+slug, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (stderr=%q)", exit, stderr)
		}
		// Byte-exact: show writes the stored body to stdout, no trailing
		// newline added by the server.
		if out != string(body) {
			t.Fatalf("show stdout mismatch:\n got: %q\nwant: %q", out, body)
		}
	})

	t.Run("MissingSlugArg_Exit2_Usage", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("show", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for missing slug arg, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "hostthis:") {
			t.Fatalf("expected 'hostthis:' prefix on error, got %q", stderr)
		}
	})

	t.Run("InvalidSlug_Exit2", func(t *testing.T) {
		s := startStack(t)
		// "BAD" isn't 8 chars and contains uppercase - not a slug.
		_, stderr, exit := s.run("show BAD", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for invalid slug, got %d (%q)", exit, stderr)
		}
		_ = stderr
	})

	t.Run("WellFormedButNonExistentSlug_NotFound_Exit4", func(t *testing.T) {
		s := startStack(t)
		// Generate a syntactically valid slug that won't exist.
		ghost := domain.NewRandomSlug().String()
		_, stderr, exit := s.run("show "+ghost, nil)
		// Per the dispatcher's exitForServiceErr, ErrNotFound → 4.
		if exit != 4 {
			t.Fatalf("expected exit 4 for not-found, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("expected 'not found' message, got %q", stderr)
		}
	})

	t.Run("ForeignSlug_NotFound_Exit4", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>own</p>"))
		slug := extractSlug(stdout)
		// Fresh client = fresh identity. Same db.
		other, _ := newKeyClient(t, s.sshAddr)
		_, stderr, exit := s.runOn(other, "show "+slug, nil)
		// Per service.requireOwner: not-owner is collapsed to ErrNotFound
		// at the boundary so existence doesn't leak. Exit 4.
		if exit != 4 {
			t.Fatalf("expected exit 4 (collapsed not-found) for foreign show, got %d (%q)",
				exit, stderr)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("expected 'not found' for foreign show, got %q", stderr)
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
	})

	t.Run("MissingArgs_Exit2_UsageHint", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		_, stderr, exit := s.run("rename "+slug, nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for missing name arg, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "usage: rename") {
			t.Fatalf("expected usage hint, got %q", stderr)
		}
	})

	t.Run("InvalidName_Newline_Exit1", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		// Newline in name is forbidden by validName.
		_, stderr, exit := s.run(`rename `+slug+` "bad`+"\n"+`name"`, nil)
		if exit == 0 {
			t.Fatalf("expected nonzero exit for newline in name, got 0 (%q)", stderr)
		}
		if !strings.Contains(stderr, "1–60") && !strings.Contains(stderr, "printable") {
			t.Fatalf("expected invalid-name message, got %q", stderr)
		}
	})

	t.Run("InvalidSlugArg_Exit2", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run(`rename BAD "label"`, nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for invalid slug, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "invalid slug") {
			t.Fatalf("expected 'invalid slug' message, got %q", stderr)
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

	t.Run("NoArgs_Exit2_UsageHint", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("delete", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for missing slug, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "usage: delete <slug>") {
			t.Fatalf("expected delete usage hint, got %q", stderr)
		}
	})

	t.Run("TooManyArgs_Exit2_UsageHint", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("delete a b c", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for too many args, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "usage: delete <slug>") {
			t.Fatalf("expected delete usage hint, got %q", stderr)
		}
	})

	t.Run("VersionDelete_FreesBytes_Exit0", func(t *testing.T) {
		s := startStack(t)
		// v1 + v2 so we can free v1.
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
		// First delete succeeds; second is a no-op success per spec.
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
		// Only v1 exists, so v1 is currently served.
		_, stderr, exit := s.run("delete "+slug+" 1", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 deleting currently-served version, got %d (%q)",
				exit, stderr)
		}
		if !strings.Contains(stderr, "currently served") {
			t.Fatalf("expected 'currently served' hint, got %q", stderr)
		}
	})

	t.Run("VersionDelete_InvalidVerArg_Exit2", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
		slug := extractSlug(stdout)
		_, stderr, exit := s.run("delete "+slug+" notanumber", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for non-numeric ver, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "invalid version") {
			t.Fatalf("expected 'invalid version' message, got %q", stderr)
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
		// `versions` writes the table on stdout and a footer line on stderr.
		out, stderr, exit := s.run("versions "+slug, nil)
		if exit != 0 {
			t.Fatalf("exit: %d (%q)", exit, stderr)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 version rows on stdout, got %d: %q", len(lines), out)
		}
		// Rows are tab-separated: vN <TAB> marker <TAB> created_at <TAB> size.
		// First row is the latest (v2) so its marker is 'current' (unpinned →
		// MAX(non-deleted ver_num) is the served).
		cols := strings.Split(lines[0], "\t")
		if len(cols) != 4 {
			t.Fatalf("expected 4 cols on row, got %d: %q", len(cols), lines[0])
		}
		if cols[0] != "v2" {
			t.Fatalf("expected v2 as the latest row, got %q", cols[0])
		}
		if strings.TrimSpace(cols[1]) != "current" {
			t.Fatalf("expected 'current' marker on latest unpinned row, got %q", cols[1])
		}
		// Footer on stderr: contains 'unpinned' + 'expires in'.
		if !strings.Contains(stderr, "unpinned") {
			t.Fatalf("expected 'unpinned' on footer, got %q", stderr)
		}
		if !strings.Contains(stderr, "expires in") {
			t.Fatalf("expected 'expires in' on footer, got %q", stderr)
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
		// Pinned-confirmation stderr is pinned.
		if strings.TrimSpace(stderr) != "pinned v1." {
			t.Fatalf("expected exactly 'pinned v1.' on stderr, got %q", stderr)
		}
		// versions footer should now say 'pinned to v1'.
		_, vstderr, _ := s.run("versions "+slug, nil)
		if !strings.Contains(vstderr, "pinned to v1") {
			t.Fatalf("expected 'pinned to v1' in versions footer, got %q", vstderr)
		}
	})

	t.Run("PinMissingArgs_Exit2", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("pin", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "usage: pin <slug> <ver-num>") {
			t.Fatalf("expected pin usage hint, got %q", stderr)
		}
	})

	t.Run("PinInvalidSlug_Exit2", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("pin BAD 1", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "invalid slug") {
			t.Fatalf("expected 'invalid slug', got %q", stderr)
		}
	})

	t.Run("PinInvalidVer_Exit2", func(t *testing.T) {
		s := startStack(t)
		stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
		slug := extractSlug(stdout)
		_, stderr, exit := s.run("pin "+slug+" 0", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		if !strings.Contains(stderr, "invalid version") {
			t.Fatalf("expected 'invalid version', got %q", stderr)
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

	t.Run("UnpinMissingSlug_Exit2", func(t *testing.T) {
		s := startStack(t)
		_, stderr, exit := s.run("unpin", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2 for missing slug, got %d (%q)", exit, stderr)
		}
		_ = stderr
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
		// stdout contains 'key: <fingerprint>' line. Pin the prefix
		// shape so Phase B doesn't change it accidentally.
		if !strings.Contains(stdout, "key:") {
			t.Fatalf("expected 'key:' line on stdout, got %q", stdout)
		}
		// The keyed-fingerprint string (s.keyedOwner is SHA256:hex) must
		// appear verbatim on stdout.
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
		// Sessions opened with sess.Run() get no PTY by default - the
		// pipe-like path. Help text emits LF-terminated lines.
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
		// PTY allocated → emitHelp translates LF to CRLF so the
		// client-side raw terminal renders without staircase effect.
		_, stderr, exit := runCmdWithPty(t, s.keyedClient, "help")
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
		// Empty argv WITH a PTY → help. (Empty argv WITHOUT a PTY → upload.)
		_, stderr, exit := runCmdWithPty(t, s.keyedClient, "")
		if exit != 0 {
			t.Fatalf("exit: %d", exit)
		}
		if !strings.Contains(stderr, "Pipe a rendered file in") {
			t.Fatalf("PTY + empty cmd should show help, got %q", stderr)
		}
	})

	t.Run("HelpBody_MentionsApex", func(t *testing.T) {
		_, stderr, _ := s.run("help", nil)
		// The fixture's apex is "paste.test"; any drift in helpTextTemplate
		// must keep the apex substitution working.
		if !strings.Contains(stderr, "paste.test") {
			t.Fatalf("help should mention apex 'paste.test', got %q", stderr)
		}
	})

	t.Run("HelpVerb_NoPty_ByteExactGolden", func(t *testing.T) {
		// Pin the FULL stderr byte content of the no-PTY help banner.
		// Substring assertions elsewhere catch big drifts but miss
		// single-character edits ("paste" -> "paste " in any line is
		// a silent regression for users running ssh + reading output
		// on a narrow terminal). The golden below is the canonical
		// rendered help for apex "paste.test"; a single byte added,
		// removed, or reordered in helpTextTemplate must fail this.
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

// expectedHelpNoPty_PasteTest is the byte-exact rendered help banner
// emitted to stderr when no PTY is allocated and the configured apex
// is "paste.test". emitHelp's no-PTY path uses fmt.Fprintln, which
// appends a single trailing "\n", so the golden ends with one LF after
// the closing period. Any drift in helpTextTemplate (line addition,
// character insertion, whitespace tweak) MUST fail the golden assertion
// - that's the whole point of pinning the full string.
const expectedHelpNoPty_PasteTest = "Pipe a rendered file in, get a URL out. Pastes expire 7 days after last update.\n" +
	"\n" +
	"UPLOAD\n" +
	"\n" +
	"    cat foo.html | ssh paste.test\n" +
	"    cat doc.md   | ssh paste.test --name \"design notes\"\n" +
	"\n" +
	"UPDATE & MANAGE (owner only; ssh key authenticates)\n" +
	"\n" +
	"    cat foo.html | ssh paste.test <slug>      replace bytes; URL stays the same\n" +
	"    ssh paste.test list                       all your active pastes\n" +
	"    ssh paste.test show <slug>                read content back\n" +
	"    ssh paste.test rename <slug> \"label\"      set / change owner label\n" +
	"    ssh paste.test delete <slug>              wipe the paste entirely\n" +
	"    ssh paste.test delete <slug> <ver>        free one version's bytes (tombstone)\n" +
	"    ssh paste.test whoami                     show your identity + active count\n" +
	"\n" +
	"VERSION HISTORY\n" +
	"\n" +
	"    Each `update` adds a new version (v2, v3, ...). Default URL serves the latest.\n" +
	"\n" +
	"    ssh paste.test versions <slug>            timeline of every version\n" +
	"    ssh paste.test pin <slug> <ver>           stick URL to <ver> (survives updates)\n" +
	"    ssh paste.test unpin <slug>               URL follows latest again\n" +
	"\n" +
	"LIMITS\n" +
	"\n" +
	"    10 MiB total per identity, counting post-compression bytes across\n" +
	"    all your active pastes (every version of every paste). Highly\n" +
	"    redundant text compresses 5-10x, so typical HTML/Markdown fits a\n" +
	"    lot of content under the cap.\n" +
	"\n" +
	"    Content types: HTML, Markdown. Anything else rejected at upload.\n"

// ---------------------------------------------------------------------------
// 10. Auth refusal + Sybil refusal
// ---------------------------------------------------------------------------

func TestAuth_Characterization(t *testing.T) {
	t.Run("PasswordOnlyClient_Exit3_KeyRequired", func(t *testing.T) {
		s := startStack(t)
		// Password auth is accepted by the gliderlabs handler but yields
		// no public key, so handleSession refuses with the key-required
		// message and exit 3.
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
	// Cap at 2: the first two fresh keys from a subnet are admitted,
	// the third is refused. Real ssh-client traffic over loopback all
	// shares one /24 subnet (127.0.0.0/24), so this exercises the
	// production code path end to end.
	t.Run("ThirdFreshKey_Exit6_AndRichMessage", func(t *testing.T) {
		g := startGatedStack(t, 2)
		c1, _ := dialKeyed(g.t, g.sshAddr)
		_, _, e1 := runCmd(g.t, c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first key should be admitted, got exit %d", e1)
		}
		c2, _ := dialKeyed(g.t, g.sshAddr)
		_, _, e2 := runCmd(g.t, c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second key should be admitted, got exit %d", e2)
		}
		c3, _ := dialKeyed(g.t, g.sshAddr)
		_, stderr, e3 := runCmd(g.t, c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third key should be refused with exit 6, got %d (%q)", e3, stderr)
		}
		// The rich SybilRefusal path prints subnet + cap usage.
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
		// Cap at 1. Two distinct sessions using the SAME key from the
		// same subnet must both be admitted: the second session reuses
		// an existing (identity, subnet) row so the cap isn't consulted.
		g := startGatedStack(t, 1)
		_, priv, err := genEd25519()
		if err != nil {
			t.Fatalf("genkey: %v", err)
		}
		signer, err := xssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		cfg := &xssh.ClientConfig{
			User:            "x",
			Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
			HostKeyCallback: xssh.InsecureIgnoreHostKey(),
			Timeout:         3 * time.Second,
		}
		dial := func() *xssh.Client {
			cli, err := xssh.Dial("tcp", g.sshAddr, cfg)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = cli.Close() })
			return cli
		}
		c1 := dial()
		_, _, e1 := runCmd(t, c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first session: exit %d", e1)
		}
		c2 := dial()
		_, _, e2 := runCmd(t, c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("returning-key session should be admitted past the cap, got exit %d", e2)
		}
	})
}

// ---------------------------------------------------------------------------
// 11. PROXY protocol (HOSTTHIS_SSH_PROXY_PROTOCOL=true)
// ---------------------------------------------------------------------------

// proxyProtoStack stands up a hostthisd-style SSH server with PROXY-protocol
// v1 parsing enabled via the env var. Tests inject a v1 PROXY header on
// the wire before the SSH handshake.
type proxyProtoStack struct {
	t       *testing.T
	sshAddr string
	keyGate *service.KeyGate
}

func startProxyProtoStack(t *testing.T, freshKeysPerSubnet int) *proxyProtoStack {
	t.Helper()
	t.Setenv("HOSTTHIS_SSH_PROXY_PROTOCOL", "true")
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storage.NewPasteRepo(db)
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	manage := service.NewManage(repo, blobUnit)
	kgRepo := storage.NewKeyGateRepo(db)
	kg := service.NewKeyGate(kgRepo)
	kg.MaxFreshKeysPerSubnet = freshKeysPerSubnet
	manage.KeyGate = kg

	httpSrv := httptest.NewServer((&httpapi.Server{Pastes: repo, Blobs: blobUnit}).Handler())
	t.Cleanup(httpSrv.Close)

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close()

	sshSrv := &hostssh.Server{
		Addr:       addr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Manage:     manage,
		KeyGate:    kg,
		BuildURL: func(s domain.Slug) string {
			return httpSrv.URL + "/p/" + s.String()
		},
		Logger: log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)
	return &proxyProtoStack{t: t, sshAddr: addr, keyGate: kg}
}

// dialWithProxyV1 opens a TCP connection, writes a PROXY v1 header
// claiming the given src/dst tuple, then runs an SSH handshake on top.
func dialWithProxyV1(t *testing.T, addr, srcIP string, srcPort int) *xssh.Client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// PROXY protocol v1: "PROXY TCP4 <src-ip> <dst-ip> <src-port> <dst-port>\r\n"
	hdr := fmt.Sprintf("PROXY TCP4 %s 127.0.0.1 %d 2222\r\n", srcIP, srcPort)
	if _, err := c.Write([]byte(hdr)); err != nil {
		t.Fatalf("write proxy header: %v", err)
	}

	_, priv, err := genEd25519()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &xssh.ClientConfig{
		User:            "anyone",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	clientConn, chans, reqs, err := xssh.NewClientConn(c, addr, cfg)
	if err != nil {
		_ = c.Close()
		t.Fatalf("ssh handshake (with proxy header): %v", err)
	}
	cli := xssh.NewClient(clientConn, chans, reqs)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestProxyProtocol_Characterization(t *testing.T) {
	t.Run("RealClientIPDistinguishesSubnetsForSybilGate", func(t *testing.T) {
		// Cap at 1 fresh key per subnet. From an unwrapped (loopback)
		// stack two fresh keys would both come from 127.0.0.0/24 and
		// the second would be rejected. With PROXY-protocol parsing
		// enabled, claiming different src IPs in DIFFERENT /24s lets
		// both be admitted - proving the gate sees the proxied IP.
		p := startProxyProtoStack(t, 1)
		c1 := dialWithProxyV1(t, p.sshAddr, "203.0.113.10", 50000)
		_, _, e1 := runCmd(t, c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first proxied client (203.0.113.0/24) should be admitted, got exit %d", e1)
		}
		c2 := dialWithProxyV1(t, p.sshAddr, "198.51.100.10", 50001)
		_, _, e2 := runCmd(t, c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second proxied client (198.51.100.0/24) should be admitted on a different subnet, got exit %d", e2)
		}
		// A second fresh key from the FIRST proxied subnet must now be
		// refused - its subnet's slot is full.
		c3 := dialWithProxyV1(t, p.sshAddr, "203.0.113.20", 50002)
		_, stderr, e3 := runCmd(t, c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third proxied client from the full subnet should hit Sybil refusal, got exit %d (%q)",
				e3, stderr)
		}
		if !strings.Contains(stderr, "203.0.113.0/24") {
			t.Fatalf("expected the proxied subnet '203.0.113.0/24' in the refusal, got %q", stderr)
		}
	})
}

// ---------------------------------------------------------------------------
// 12. PTY/no-PTY behavior on a second verb (list) - pinned by inspection
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
		// When a PTY is allocated, the SSH server's PTY layer cooks
		// outbound newlines to CR+LF on the way to the client. Pin this
		// so wish migration doesn't accidentally swap raw vs cooked.
		stdout, _, _ := runCmdWithPty(t, s.keyedClient, "list")
		if !strings.Contains(stdout, "\r") {
			t.Fatalf("list stdout over PTY session should contain CR, got %q", stdout)
		}
	})
}

// ---------------------------------------------------------------------------
// 13. Exit-code matrix (canonical table; pin all distinct codes the
//     current SSH layer emits)
// ---------------------------------------------------------------------------

func TestExitCodes_Characterization(t *testing.T) {
	s := startStack(t)
	// Set up some state so all cases are reachable.
	stdoutA, _, _ := s.run("", []byte("<!doctype html><p>a</p>"))
	slugA := extractSlug(stdoutA)
	// Foreign client = different identity, used for the not-owner path.
	foreignClient, _ := newKeyClient(t, s.sshAddr)

	cases := []struct {
		name string
		// "" client = keyed default. anon = uses anon client. foreign = different keyed identity.
		client string
		cmd    string
		stdin  []byte
		want   int
		desc   string
	}{
		{
			name: "ExitCode0_HelpSuccess",
			cmd:  "help",
			want: 0,
			desc: "help is the canonical exit-0 path with no side effects",
		},
		{
			name: "ExitCode0_WhoamiSuccess",
			cmd:  "whoami",
			want: 0,
			desc: "whoami always exits 0 for a keyed session",
		},
		{
			name: "ExitCode2_UnknownVerb",
			cmd:  "wibble",
			want: 2,
			desc: "unknown command → exit 2 with help dump",
		},
		{
			name: "ExitCode2_UsageError_DeleteNoArgs",
			cmd:  "delete",
			want: 2,
			desc: "verb-level usage error → exit 2",
		},
		{
			name: "ExitCode2_InvalidVer",
			cmd:  "delete " + slugA + " notanumber",
			want: 2,
			desc: "non-numeric ver arg → exit 2",
		},
		{
			name:   "ExitCode3_KeylessSession",
			client: "anon",
			cmd:    "whoami",
			want:   3,
			desc:   "session without a key → exit 3",
		},
		{
			name: "ExitCode4_NotFound",
			cmd:  "show " + domain.NewRandomSlug().String(),
			want: 4,
			desc: "well-formed but non-existent slug → exit 4",
		},
		{
			name:   "ExitCode4_NotOwner_CollapsedToNotFound",
			client: "foreign",
			cmd:    "show " + slugA,
			want:   4,
			desc:   "foreign owner's slug is hidden as 'not found' → exit 4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				stdout, stderr string
				exit           int
			)
			switch tc.client {
			case "anon":
				stdout, stderr, exit = s.runAnon(tc.cmd, tc.stdin)
			case "foreign":
				stdout, stderr, exit = s.runOn(foreignClient, tc.cmd, tc.stdin)
			default:
				stdout, stderr, exit = s.run(tc.cmd, tc.stdin)
			}
			if exit != tc.want {
				t.Fatalf("%s: %s\n  cmd: %q\n  got exit %d, want %d\n  stdout: %q\n  stderr: %q",
					tc.name, tc.desc, tc.cmd, exit, tc.want, stdout, stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 14. Edge cases - concurrent ops from the same identity
// ---------------------------------------------------------------------------

// concurrentUpload runs one upload on its own session under cli and
// returns the URL or "" + the error. It does NOT call t.Fatalf - we
// want to drive this from many goroutines and aggregate failures in
// the parent.
func concurrentUpload(cli *xssh.Client, body []byte) (string, error) {
	sess, err := cli.NewSession()
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	sess.Stdin = bytes.NewReader(body)
	if err := sess.Run(""); err != nil {
		return "", fmt.Errorf("run: %w (stderr=%q)", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func TestConcurrent_Characterization(t *testing.T) {
	// Sequential-but-from-many-clients pin: N back-to-back uploads each
	// on a freshly-dialed ssh connection (one per upload) all succeed
	// and return distinct slugs. This pins:
	//   - the slug-collision retry loop (5 attempts) is enough across N inserts
	//   - the per-session ssh handshake teardown doesn't leak state
	//   - distinct slugs come back
	//
	// Why not parallel? The current implementation uses a single sqlite
	// connection with the default modernc.org/sqlite busy timeout, and
	// truly concurrent inserts can surface SQLITE_BUSY as a generic
	// exit-1 to the user. That IS the current behavior - Phase B's wish
	// migration shouldn't change it without an explicit decision. We
	// pin "many quick sequential uploads" as the guaranteed-no-BUSY
	// envelope, which is the realistic workload.
	s := startStack(t)
	const N = 6
	urls := map[string]struct{}{}
	for i := range N {
		body := fmt.Appendf(nil, "<!doctype html><p>seq %d</p>", i)
		cli, _ := newKeyClient(t, s.sshAddr)
		url, err := concurrentUpload(cli, body)
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
		if !strings.HasPrefix(url, s.httpURL+"/p/") {
			t.Fatalf("upload %d: expected URL, got %q", i, url)
		}
		if _, dup := urls[url]; dup {
			t.Fatalf("duplicate URL from sequential uploads: %q", url)
		}
		urls[url] = struct{}{}
	}
	if len(urls) != N {
		t.Fatalf("expected %d distinct slugs, got %d", N, len(urls))
	}
	// As a sanity check, `list` on the default keyed client (which
	// uploaded nothing) shows zero pastes - every upload above was on a
	// fresh keyed identity. Pinning this confirms per-identity isolation.
	listOut, _, _ := s.run("list", nil)
	rows := bufio.NewScanner(strings.NewReader(listOut))
	count := 0
	for rows.Scan() {
		if strings.HasPrefix(rows.Text(), "SLUG\t") {
			continue
		}
		if strings.TrimSpace(rows.Text()) == "" {
			continue
		}
		count++
	}
	if count != 0 {
		t.Fatalf("default-client list should be empty (uploads were on fresh identities), got %d rows: %q",
			count, listOut)
	}
}

// ---------------------------------------------------------------------------
// 15. Owner-collapse: NotOwner is intentionally indistinguishable from
//     NotFound at the SSH boundary. Every owner-gated verb pinned here
//     to lock in the "no existence leak" contract.
// ---------------------------------------------------------------------------

func TestOwnerCollapse_Characterization(t *testing.T) {
	// service.Manage.requireOwner returns ErrNotFound (NOT ErrNotOwner)
	// whenever the slug belongs to a different identity. The SSH surface
	// therefore always exits 4 on a foreign-slug verb, never 5. This pins
	// that collapse across every owner-gated verb. If a future refactor
	// surfaces ErrNotOwner distinctly, exitForServiceErr must regain its
	// NotOwner branch AND a new exit-code (5) test must land alongside -
	// changes to this test are an explicit policy decision, not silent.
	s := startStack(t)

	// Identity A creates a paste.
	stdoutA, _, _ := s.run("", []byte("<!doctype html><p>owned by A</p>"))
	slugA := extractSlug(stdoutA)
	// v2 so we have something to delete by version + something to pin.
	_, _, _ = s.run(slugA, []byte("<!doctype html><p>v2</p>"))

	// Identity B (a different keyed client on the same server).
	otherClient, _ := newKeyClient(t, s.sshAddr)

	cases := []struct {
		name string
		cmd  string
		body []byte
	}{
		{name: "Show_ForeignSlug", cmd: "show " + slugA},
		{name: "Rename_ForeignSlug", cmd: `rename ` + slugA + ` "hijack"`},
		{name: "Delete_ForeignSlug", cmd: "delete " + slugA},
		{name: "DeleteVersion_ForeignSlug", cmd: "delete " + slugA + " 1"},
		{name: "Pin_ForeignSlug", cmd: "pin " + slugA + " 1"},
		{name: "Unpin_ForeignSlug", cmd: "unpin " + slugA},
		{name: "Versions_ForeignSlug", cmd: "versions " + slugA},
		{name: "Update_ForeignSlug", cmd: slugA, body: []byte("<!doctype html><p>x</p>")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, exit := s.runOn(otherClient, tc.cmd, tc.body)
			if exit != 4 {
				t.Fatalf("foreign %s expected exit 4 (NotFound, NOT 5/NotOwner), got %d (%q)",
					tc.name, exit, stderr)
			}
			// Stderr also pinned: the user-facing message is "not found",
			// not "not your paste". Anything else would leak that the
			// slug exists under a different identity.
			if !strings.Contains(stderr, "not found") {
				t.Fatalf("foreign %s expected 'not found' on stderr, got %q",
					tc.name, stderr)
			}
			if strings.Contains(stderr, "not your paste") {
				t.Fatalf("foreign %s LEAKS existence via 'not your paste' message: %q",
					tc.name, stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 16. parseUploadFlags negative paths - byte-exact stderr lines
// ---------------------------------------------------------------------------

func TestUploadFlags_NegativeCharacterization(t *testing.T) {
	s := startStack(t)

	// The parseUploadFlags path is reached when argv[0] starts with "--"
	// OR when argv[0] is a valid slug. "put" isn't a verb in the
	// dispatcher (no such case exists), so "put --name" actually routes
	// through the unknown-command path with exit 2 + the help dump -
	// NOT the parser. The parser is exercised by `--name` / `--type`
	// in the FIRST position (no slug), which is the canonical "upload
	// with a label, no slug" shape per docs/SPEC.md.

	t.Run("DashDashNameNoValue_Exit2_ByteExactStderr", func(t *testing.T) {
		// First arg is `--name` (flag in position 0) so the dispatcher
		// routes straight into verbUpload → parseUploadFlags. The parser
		// sees `--name` with nothing after it and returns the canonical
		// "needs a value" error. The SSH layer prefixes "hostthis: " and
		// appends "\n". Pinned byte-exact.
		_, stderr, exit := s.run("--name", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		want := "hostthis: --name needs a value\n"
		if stderr != want {
			t.Fatalf("stderr drift:\n got: %q\nwant: %q", stderr, want)
		}
	})

	t.Run("DashDashTypeNoValue_Exit2_ByteExactStderr", func(t *testing.T) {
		_, stderr, exit := s.run("--type", nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		want := "hostthis: --type needs a value\n"
		if stderr != want {
			t.Fatalf("stderr drift:\n got: %q\nwant: %q", stderr, want)
		}
	})

	t.Run("UnexpectedArgument_Exit2_ByteExactStderr", func(t *testing.T) {
		// `--name foo bar` → name=foo, then `bar` is an unexpected
		// positional (not a slug, not a flag). Parser returns the
		// canonical "unexpected argument" error. SSH layer prefixes
		// "hostthis: " and appends "\n".
		_, stderr, exit := s.run(`--name foo bar`, nil)
		if exit != 2 {
			t.Fatalf("expected exit 2, got %d (%q)", exit, stderr)
		}
		want := "hostthis: unexpected argument \"bar\"\n"
		if stderr != want {
			t.Fatalf("stderr drift:\n got: %q\nwant: %q", stderr, want)
		}
	})
}

// ---------------------------------------------------------------------------
// 17. Sybil gate - IPv6 (/48) subnet path via PROXY protocol v1 TCP6
// ---------------------------------------------------------------------------

// dialWithProxyV6 opens a TCP connection, writes a PROXY v1 TCP6 header
// claiming the given IPv6 src, then runs an SSH handshake on top. Mirrors
// dialWithProxyV1 but for IPv6 - drives the /48 mask path in ipSubnet.
func dialWithProxyV6(t *testing.T, addr, srcIP string, srcPort int) *xssh.Client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// PROXY protocol v1, IPv6 form:
	//   PROXY TCP6 <src-ipv6> <dst-ipv6> <src-port> <dst-port>\r\n
	// We use ::1 as the dst (the test listener is on 127.0.0.1 but PROXY
	// v1 requires the family to agree across src + dst; ::1 satisfies
	// the parser and is never consulted by the gate, only the src is).
	hdr := fmt.Sprintf("PROXY TCP6 %s ::1 %d 2222\r\n", srcIP, srcPort)
	if _, err := c.Write([]byte(hdr)); err != nil {
		t.Fatalf("write proxy header: %v", err)
	}
	_, priv, err := genEd25519()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &xssh.ClientConfig{
		User:            "anyone",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	clientConn, chans, reqs, err := xssh.NewClientConn(c, addr, cfg)
	if err != nil {
		_ = c.Close()
		t.Fatalf("ssh handshake (with proxy header): %v", err)
	}
	cli := xssh.NewClient(clientConn, chans, reqs)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestProxyProtocol_IPv6_SybilCharacterization(t *testing.T) {
	// Pin the /48 masking path for IPv6 src addresses. KeyGate uses /48
	// (per ipSubnet in server.go). Three fresh keys from the SAME /48
	// (different lower 80 bits) must trigger the refusal on the third;
	// a fresh key from a DIFFERENT /48 must succeed.
	t.Run("SameSlash48_ThirdRefused_DifferentSlash48_Admitted", func(t *testing.T) {
		p := startProxyProtoStack(t, 2)

		// Three IPv6 addresses all in 2001:db8:1::/48 - only the lower
		// 80 bits differ. ipSubnet should bucket them together.
		c1 := dialWithProxyV6(t, p.sshAddr, "2001:db8:1::aa", 50000)
		_, _, e1 := runCmd(t, c1, "whoami", nil)
		if e1 != 0 {
			t.Fatalf("first IPv6 client in 2001:db8:1::/48 should be admitted, got exit %d", e1)
		}
		c2 := dialWithProxyV6(t, p.sshAddr, "2001:db8:1:1234::bb", 50001)
		_, _, e2 := runCmd(t, c2, "whoami", nil)
		if e2 != 0 {
			t.Fatalf("second IPv6 client in same /48 should be admitted (cap=2), got exit %d", e2)
		}
		// Third fresh key - different lower bits, SAME /48 - must be refused.
		c3 := dialWithProxyV6(t, p.sshAddr, "2001:db8:1:ffff::cc", 50002)
		_, stderr3, e3 := runCmd(t, c3, "whoami", nil)
		if e3 != 6 {
			t.Fatalf("third IPv6 client in full /48 should hit Sybil refusal (exit 6), got %d (%q)",
				e3, stderr3)
		}
		if !strings.Contains(stderr3, "too many new keys from this network today") {
			t.Fatalf("expected canonical sybil refusal, got %q", stderr3)
		}
		// The subnet detail line must name the IPv6 /48. Pin the prefix
		// (the exact mask-canonicalization of 2001:db8:1:: depends on
		// net.IP.Mask) - assert the "/48" suffix and the 2001:db8:1
		// prefix appear together.
		if !strings.Contains(stderr3, "/48") {
			t.Fatalf("expected '/48' in IPv6 subnet detail, got %q", stderr3)
		}
		if !strings.Contains(stderr3, "2001:db8:1") {
			t.Fatalf("expected '2001:db8:1' prefix in IPv6 subnet detail, got %q", stderr3)
		}

		// A fresh key from a DIFFERENT /48 (2001:db8:2::/48) gets in.
		c4 := dialWithProxyV6(t, p.sshAddr, "2001:db8:2::dd", 50003)
		_, _, e4 := runCmd(t, c4, "whoami", nil)
		if e4 != 0 {
			t.Fatalf("fresh key from a different /48 should be admitted, got exit %d (Sybil gate is per-/48)", e4)
		}
	})
}

// ---------------------------------------------------------------------------
// 18. Sybil refusal - "(b) wait until <timestamp>" line shape
// ---------------------------------------------------------------------------

func TestSybil_WaitUntilLine_Characterization(t *testing.T) {
	// Phase A's TestSybilGate_Characterization pins the canonical refusal
	// line via substring. This pin is sharper: the rich enrichment
	// path emits a "(b) wait until <YYYY-MM-DD HH:MM UTC> - the oldest
	// entry ages out then" line, and we lock in the prefix byte-exact
	// + a regex on the formatted-time tail. The literal timestamp is
	// time-sensitive (it's now+window), so we can't pin it exactly;
	// the regex on the format string is what we want.
	g := startGatedStack(t, 2)
	c1, _ := dialKeyed(g.t, g.sshAddr)
	_, _, _ = runCmd(g.t, c1, "whoami", nil)
	c2, _ := dialKeyed(g.t, g.sshAddr)
	_, _, _ = runCmd(g.t, c2, "whoami", nil)
	c3, _ := dialKeyed(g.t, g.sshAddr)
	_, stderr, e3 := runCmd(g.t, c3, "whoami", nil)
	if e3 != 6 {
		t.Fatalf("third key should be refused, got exit %d", e3)
	}
	// Locate the "(b) wait until ..." line. Server emits it with the
	// fmt format "  (b) wait until %s - the oldest entry ages out then\n"
	// where %s is now+window formatted as "2006-01-02 15:04 UTC". The
	// indent (2 spaces) + literal prefix are byte-exact pinned.
	const wantPrefix = "  (b) wait until "
	_, after, ok := strings.Cut(stderr, wantPrefix)
	if !ok {
		t.Fatalf("missing '(b) wait until ' line in refusal:\n%q", stderr)
	}
	// Take the rest of the line after the prefix.
	rest := after
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		t.Fatalf("'(b) wait until ' line is unterminated: %q", rest)
	}
	line := rest[:nl]
	// Regex on the timestamp tail: YYYY-MM-DD HH:MM UTC then the
	// trailing " - the oldest entry ages out then" sentence. The em
	// dash is the literal character the server uses (this is the one
	// place we don't have control over).
	wantTail := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC - the oldest entry ages out then$`)
	if !wantTail.MatchString(line) {
		t.Fatalf("'(b) wait until ' tail drift:\n got: %q\nwant pattern: %q",
			line, wantTail.String())
	}
}
