//go:build e2e

// Package e2e drives a real hostthisd through the two surfaces a user has: an
// ssh upload and a browser. A paste is rendered client-side, so no assertion
// below the browser can tell a working renderer from a blank page.
package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// readyTimeout bounds the wait for the daemon's first served request. Generous
// because a cold start opens the metadata backend before it listens.
const readyTimeout = 30 * time.Second

// stopTimeout is how long a SIGTERM gets before the process is killed.
const stopTimeout = 10 * time.Second

// Server is a hostthisd process on ephemeral ports over a scratch data dir.
type Server struct {
	// BaseURL is the http origin, e.g. http://127.0.0.1:54321.
	BaseURL string
	// SSHAddr is host:port for the ssh listener.
	SSHAddr string

	signer xssh.Signer
	logs   *syncBuffer
}

// StartServer builds and starts hostthisd, waits until it serves, and stops it
// when the test ends. Ports are ephemeral and the data dir is per-test, so
// parallel tests never collide.
func StartServer(t *testing.T) *Server {
	t.Helper()

	bin, err := buildDaemon()
	if err != nil {
		t.Fatalf("build daemon: %v", err)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	// 127.0.0.1 rather than localhost for the apex: the apex is also the Host
	// the browser sends, and localhost can resolve to ::1 while the daemon
	// listens on v4 only.
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	sshAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	logs := &syncBuffer{}
	cmd := exec.Command(bin)
	// Path mode serves pastes at /p/<slug>, which needs no wildcard DNS.
	cmd.Env = append(envWithoutHostthis(),
		"HOSTTHIS_URL_MODE=path",
		"HOSTTHIS_PUBLIC_SCHEME=http",
		"HOSTTHIS_APEX_DOMAIN="+httpAddr,
		"HOSTTHIS_HTTP_ADDR="+httpAddr,
		"HOSTTHIS_SSH_ADDR="+sshAddr,
		"HOSTTHIS_METRICS_ADDR="+metricsAddr,
		"HOSTTHIS_DATA_DIR="+t.TempDir(),
		"HOSTTHIS_LANDING="+filepath.Join(root, "web", "landing.html"),
	)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	// Registered before the stop cleanup, so it runs after it and prints a log
	// that already has the shutdown lines in it.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("hostthisd log:\n%s", logs.String())
		}
	})

	// Closed, not sent on: both the readiness wait and the stop cleanup need to
	// observe the exit, and a value would only reach whichever read it first.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(stopTimeout):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	s := &Server{
		BaseURL: "http://" + httpAddr,
		SSHAddr: sshAddr,
		signer:  newSigner(t),
		logs:    logs,
	}
	s.waitReady(t, done)
	return s
}

// waitReady blocks until BOTH listeners are up. The two bind independently and
// /healthz answers first, so waiting on http alone hands back a server whose
// ssh port still refuses the upload that follows. An early exit is reported as
// itself, so a config error reads as one instead of as a timeout.
func (s *Server) waitReady(t *testing.T, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("hostthisd exited before serving\n%s", s.logs.String())
		default:
		}
		if s.httpUp() && s.sshUp() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("hostthisd did not serve http+ssh within %s\n%s", readyTimeout, s.logs.String())
}

func (s *Server) httpUp() bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(s.BaseURL + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Server) sshUp() bool {
	conn, err := net.DialTimeout("tcp", s.SSHAddr, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// UploadOpts carries the optional upload flags. The zero value uploads neither
// and lets the server sniff the kind.
type UploadOpts struct {
	// Type is the --type kind hint ("md", "csv", "mermaid", ...).
	Type string
	// Name is the --name label. It must not begin with "--".
	Name string
}

// Paste is a paste that exists on the running server.
type Paste struct {
	Slug string
	// URL is the browsable page, as the server printed it.
	URL string
}

// Upload pipes content over ssh, which is the only way a user creates a paste.
// Seeding through the service layer would skip the sniffing and kind routing
// that decide which renderer the browser is handed.
func (s *Server) Upload(t *testing.T, content []byte, opts UploadOpts) Paste {
	t.Helper()

	client, err := xssh.Dial("tcp", s.SSHAddr, &xssh.ClientConfig{
		User:            "e2e",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(s.signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial %s: %v", s.SSHAddr, err)
	}
	defer client.Close() //nolint:errcheck

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	var stdout, stderr bytes.Buffer
	sess.Stdin = bytes.NewReader(content)
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(uploadCommand(opts)); err != nil {
		t.Fatalf("ssh upload: %v\nstderr: %s", err, stderr.String())
	}

	raw := strings.TrimSpace(stdout.String())
	slug, err := slugFromURL(raw)
	if err != nil {
		t.Fatalf("upload printed %q: %v\nstderr: %s", raw, err, stderr.String())
	}
	return Paste{Slug: slug, URL: raw}
}

// pasteReadyTimeout bounds the wait for a fresh upload to leave the pending
// state.
const pasteReadyTimeout = 20 * time.Second

// WaitReady blocks until the paste serves its own content rather than the
// pending loading page. An upload returns as soon as the slug exists, so a
// header read that races the finalizer sees the loading page's text/html and
// reads as a content-type regression. A browser assertion rides the loading
// page's meta refresh and does not need this; a header assertion does.
func (s *Server) WaitReady(t *testing.T, p Paste) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(pasteReadyTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(p.URL)
		if err == nil {
			code, retryAfter := resp.StatusCode, resp.Header.Get("Retry-After")
			_ = resp.Body.Close()
			if code == http.StatusGone {
				t.Fatalf("paste %s failed to store\n%s", p.Slug, s.logs.String())
			}
			// Retry-After is set by the pending page alone, so its absence is
			// the readiness signal without matching on that page's copy.
			if code == http.StatusOK && retryAfter == "" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("paste %s still pending after %s\n%s", p.Slug, pasteReadyTimeout, s.logs.String())
}

// uploadCommand builds the argv the ssh client sends. --name goes last: the
// server joins every token after it up to the next flag into the label, so
// anything following it would be swallowed.
func uploadCommand(o UploadOpts) string {
	var argv []string
	if o.Type != "" {
		argv = append(argv, "--type", o.Type)
	}
	if o.Name != "" {
		argv = append(argv, "--name", o.Name)
	}
	return strings.Join(argv, " ")
}

// slugFromURL reads the slug out of a path-mode URL (http://apex/p/<slug>).
func slugFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	slug, ok := strings.CutPrefix(u.Path, "/p/")
	if !ok || slug == "" || strings.Contains(slug, "/") {
		return "", fmt.Errorf("not a path-mode paste url")
	}
	return slug, nil
}

// newSigner mints the identity every upload from one server uses. Reusing it
// keeps the per-subnet fresh-key gate from being spent a slot per paste.
func newSigner(t *testing.T) xssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// freePort reserves an ephemeral port and releases it for the daemon to bind.
// The gap is a race no OS API closes short of passing the listener into the
// child, which is not worth the coupling.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	return ln.Addr().(*net.TCPAddr).Port
}

var (
	buildOnce sync.Once
	daemonBin string
	daemonDir string
	buildErr  error
)

// buildDaemon compiles cmd/hostthisd once per test binary, so a suite pays one
// compile rather than one per server.
func buildDaemon() (string, error) {
	buildOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		daemonDir, err = os.MkdirTemp("", "hostthis-e2e")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(daemonDir, "hostthisd")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/hostthisd")
		cmd.Dir = root
		// GOWORK=off so the daemon links the go.mod pins. A gitignored go.work
		// redirects dependencies at a local worktree, which would let the browser
		// suite pass against a binary CI can never produce, and the divergence is
		// invisible precisely because CI has no go.work.
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/hostthisd: %w\n%s", err, out)
			return
		}
		daemonBin = bin
	})
	return daemonBin, buildErr
}

// removeDaemonBuild drops the compiled daemon. TestMain calls it.
func removeDaemonBuild() {
	if daemonDir != "" {
		_ = os.RemoveAll(daemonDir)
	}
}

// repoRoot walks up from the working directory to the module root, which is
// where the daemon's landing page and the default artifacts dir live.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod at or above %s", dir)
		}
		dir = parent
	}
}

// syncBuffer serializes the daemon's stdout and stderr, which os/exec copies
// on two goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// envWithoutHostthis returns the ambient environment with every HOSTTHIS_ key
// dropped, so the daemon under test is configured only by what StartServer
// appends. Inheriting them wholesale is not a hygiene nit: an operator who has
// sourced a production .env in the same shell would otherwise hand the suite a
// real cache backend and credentials, and the uploads it makes would fire live
// CDN purges against the production zone.
func envWithoutHostthis() []string {
	all := os.Environ()
	kept := make([]string, 0, len(all))
	for _, kv := range all {
		if strings.HasPrefix(kv, "HOSTTHIS_") {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}
