package ssh_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// stack is the per-test bundle of services + listener addresses for
// driving the SSH server end-to-end.
type stack struct {
	t           *testing.T
	httpURL     string
	sshAddr     string
	repo        *storage.PasteRepo
	tokenRepo   *storage.TokenRepo
	blobs       *storage.BlobStore
	keyedClient *xssh.Client
	keyedOwner  string
	anonClient  *xssh.Client
}

// signWith returns a stable SHA256 fingerprint for an ssh key, the
// same way the server computes it. We pre-seed it so tests can
// assert ownership without parsing the live ssh greeting.
func newKeyClient(t *testing.T, addr string) (*xssh.Client, string) {
	t.Helper()
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
	cli, err := xssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	// fingerprint matches what fingerprintKey emits on the server
	// (SHA256:<hex>). We mirror that here so tests can reason about
	// expected owners without reaching into the server.
	hash := fingerprintSigner(signer.PublicKey())
	return cli, hash
}

func newAnonClient(t *testing.T, addr string) *xssh.Client {
	t.Helper()
	cfg := &xssh.ClientConfig{
		User:            "anon",
		Auth:            []xssh.AuthMethod{xssh.Password("ignored")},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	cli, err := xssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("ssh dial (anon): %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func startStack(t *testing.T) *stack {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)
	tokenRepo := storage.NewTokenRepo(db)
	upload := service.NewUpload(repo, blobs)
	manage := service.NewManage(repo, blobs)
	tokenSvc := service.NewTokenService(tokenRepo)

	httpSrv := httptest.NewServer((&httpapi.Server{Pastes: repo, Blobs: blobs}).Handler())
	t.Cleanup(httpSrv.Close)

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close()

	sshSrv := &hostssh.Server{
		Addr:   addr,
		Upload: upload,
		Manage: manage,
		Token:  tokenSvc,
		BuildURL: func(s domain.Slug) string {
			return httpSrv.URL + "/p/" + s.String()
		},
		Logger: log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)

	keyedClient, keyedOwner := newKeyClient(t, addr)
	anonClient := newAnonClient(t, addr)

	return &stack{
		t:           t,
		httpURL:     httpSrv.URL,
		sshAddr:     addr,
		repo:        repo,
		tokenRepo:   tokenRepo,
		blobs:       blobs,
		keyedClient: keyedClient,
		keyedOwner:  keyedOwner,
		anonClient:  anonClient,
	}
}

// run executes one ssh command against the keyed client and returns
// (stdout, stderr, exit-status). Stdin is the optional body.
func (s *stack) run(cmd string, stdin []byte) (string, string, int) {
	return s.runOn(s.keyedClient, cmd, stdin)
}

func (s *stack) runAnon(cmd string, stdin []byte) (string, string, int) {
	return s.runOn(s.anonClient, cmd, stdin)
}

func (s *stack) runOn(cli *xssh.Client, cmd string, stdin []byte) (string, string, int) {
	s.t.Helper()
	sess, err := cli.NewSession()
	if err != nil {
		s.t.Fatalf("session: %v", err)
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
		var exitErr *xssh.ExitError
		if asExitErr(err, &exitErr) {
			exit = exitErr.ExitStatus()
		} else {
			s.t.Fatalf("run %q: %v\nstderr: %s", cmd, err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), exit
}

func TestVerbList_Empty(t *testing.T) {
	s := startStack(t)
	stdout, stderr, exit := s.run("list", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %s)", exit, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "no active pastes") {
		t.Fatalf("expected no-pastes notice on stderr, got %q", stderr)
	}
}

func TestVerbList_AfterUpload(t *testing.T) {
	s := startStack(t)
	// Upload one with --name, one without.
	stdout1, stderr1, exit1 := s.run(`--name "demo"`, []byte("<!doctype html><h1>1</h1>"))
	stdout2, stderr2, exit2 := s.run("", []byte("# md\n"))
	if !strings.Contains(stdout1, "/p/") || !strings.Contains(stdout2, "/p/") {
		t.Fatalf("uploads didn't return URLs\nstdout1=%q stderr1=%q exit1=%d\nstdout2=%q stderr2=%q exit2=%d",
			stdout1, stderr1, exit1, stdout2, stderr2, exit2)
	}
	stdout, _, exit := s.run("list", nil)
	if exit != 0 {
		t.Fatalf("list exit: %d", exit)
	}
	if !strings.Contains(stdout, "demo") {
		t.Fatalf("named paste should appear in list: %q", stdout)
	}
	// Two rows of tab-separated output
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 list rows, got %d:\n%s", len(lines), stdout)
	}
}

func TestVerbWhoami_AnonAndKeyed(t *testing.T) {
	s := startStack(t)
	_, stderr, _ := s.runAnon("whoami", nil)
	if !strings.Contains(stderr, "anonymous") {
		t.Fatalf("anon whoami should say so: %q", stderr)
	}
	stdout, _, _ := s.run("whoami", nil)
	if !strings.Contains(stdout, s.keyedOwner) {
		t.Fatalf("keyed whoami should show owner: %q", stdout)
	}
	if !strings.Contains(stdout, "active:") {
		t.Fatalf("expected active count line: %q", stdout)
	}
}

func TestVerbShow_OwnerOnly(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>hello</p>"))
	slug := extractSlug(stdout)
	body, _, exit := s.run("show "+slug, nil)
	if exit != 0 {
		t.Fatalf("show exit: %d", exit)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("show returned wrong body: %q", body)
	}
	// Anon trying to show should get not-found / forbidden.
	_, stderr, exitAnon := s.runAnon("show "+slug, nil)
	if exitAnon == 0 {
		t.Fatalf("anon show should fail, got 0 exit (stderr=%q)", stderr)
	}
}

func TestVerbDelete_Roundtrip(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>delete me</p>"))
	slug := extractSlug(stdout)

	// Confirm it serves via http first.
	resp, err := http.Get(s.httpURL + "/p/" + slug)
	if err != nil {
		t.Fatalf("get before delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 before delete, got %d", resp.StatusCode)
	}

	// Delete.
	_, stderr, exit := s.run("delete "+slug, nil)
	if exit != 0 {
		t.Fatalf("delete exit: %d (%q)", exit, stderr)
	}
	if !strings.Contains(stderr, "deleted") {
		t.Fatalf("expected 'deleted' confirmation, got %q", stderr)
	}

	// Confirm it 404s.
	resp, err = http.Get(s.httpURL + "/p/" + slug)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestVerbRename(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>x</p>"))
	slug := extractSlug(stdout)
	_, stderr, exit := s.run(`rename `+slug+` "new label"`, nil)
	if exit != 0 {
		t.Fatalf("rename exit: %d", exit)
	}
	if !strings.Contains(stderr, "renamed") {
		t.Fatalf("expected renamed confirmation: %q", stderr)
	}
	// list should show the new name
	listStdout, _, _ := s.run("list", nil)
	if !strings.Contains(listStdout, "new label") {
		t.Fatalf("expected new label in list: %q", listStdout)
	}
}

func TestVerbUpdate_AppendsVersion(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)
	// Update
	_, stderr, exit := s.run(slug, []byte("<!doctype html><p>v2</p>"))
	if exit != 0 {
		t.Fatalf("update exit: %d (%q)", exit, stderr)
	}
	if !strings.Contains(stderr, "v2") {
		t.Fatalf("expected v2 in update stderr: %q", stderr)
	}
	// versions verb should list both
	stdoutV, _, _ := s.run("versions "+slug, nil)
	if !strings.Contains(stdoutV, "v1") || !strings.Contains(stdoutV, "v2") {
		t.Fatalf("expected v1 + v2 in versions output: %q", stdoutV)
	}
	// http should serve v2
	resp, _ := http.Get(s.httpURL + "/p/" + slug)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "v2") {
		t.Fatalf("expected v2 served: %q", body)
	}
	// Pin v1, http should now serve v1
	_, _, _ = s.run("pin "+slug+" 1", nil)
	resp, _ = http.Get(s.httpURL + "/p/" + slug)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "v1") {
		t.Fatalf("expected v1 served after pin: %q", body)
	}
}

func TestVerbTokenCreate(t *testing.T) {
	s := startStack(t)
	stdout, stderr, exit := s.run("token create", nil)
	if exit != 0 {
		t.Fatalf("token create exit: %d (%q)", exit, stderr)
	}
	tok := strings.TrimSpace(stdout)
	if !strings.HasPrefix(tok, "htst_live_") {
		t.Fatalf("token format wrong: %q", tok)
	}
}

func TestVerbHelp_OnUnknown(t *testing.T) {
	s := startStack(t)
	_, stderr, exit := s.run("nonsense", nil)
	if exit == 0 {
		t.Fatalf("unknown command should exit nonzero")
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("expected 'unknown command': %q", stderr)
	}
	if !strings.Contains(stderr, "hostthis — pipe") {
		t.Fatalf("expected help text after the error: %q", stderr)
	}
}

func TestAnonCannotManage(t *testing.T) {
	s := startStack(t)
	// Anon upload first
	stdout, _, _ := s.runAnon("", []byte("<!doctype html><p>anon</p>"))
	slug := extractSlug(stdout)
	// Anon list → ErrEmptyOwner
	_, stderr, exit := s.runAnon("list", nil)
	if exit == 0 {
		t.Fatalf("anon list should fail")
	}
	if !strings.Contains(stderr, "add an ssh key") {
		t.Fatalf("expected anon nudge: %q", stderr)
	}
	// Anon delete should also fail
	_, _, exit = s.runAnon("delete "+slug, nil)
	if exit == 0 {
		t.Fatalf("anon delete should fail")
	}
}

// -- helpers ----------------------------------------------------------------

func extractSlug(stdoutURL string) string {
	url := strings.TrimSpace(stdoutURL)
	i := strings.LastIndex(url, "/p/")
	if i == -1 {
		return ""
	}
	return url[i+len("/p/"):]
}

func asExitErr(err error, target **xssh.ExitError) bool {
	if e, ok := err.(*xssh.ExitError); ok {
		*target = e
		return true
	}
	return false
}
