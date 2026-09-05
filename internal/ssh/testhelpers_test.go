package ssh_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
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
	"github.com/Zamua/hostthis/internal/storagetest"
)

// genEd25519 wraps crypto/ed25519.GenerateKey to a 2-tuple the test
// caller wants (pub, priv).
func genEd25519() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// fingerprintSigner mirrors server-side fingerprintKey so tests can
// assert "the owner the server captured" without parsing logs.
func fingerprintSigner(pk xssh.PublicKey) string {
	sum := sha256.Sum256(pk.Marshal())
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// qrGlyphs are the half-block runes qrterminal emits in HalfBlocks mode. At
// least one marks a rendered QR; none may appear on stdout, which must stay a
// clean URL.
const qrGlyphs = "█▀▄"

// stack is one real hostthisd-shaped deployment: metadata repo, blob store,
// http surface, ssh server, plus a keyed and an anonymous client. Options wire
// the optional services (site deploy, Sybil gate, PROXY protocol).
type stack struct {
	t          *testing.T
	httpURL    string
	sshAddr    string
	repo       *storage.MemRepo
	upload     *service.Upload
	keyGate    *service.KeyGate
	keyed      *xssh.Client
	keyedOwner string
	anon       *xssh.Client
}

type stackOpts struct {
	keyGateCap int
	sites      bool
	proxyProto bool
}

type stackOpt func(*stackOpts)

// withKeyGate wires a live KeyGate at the given per-subnet fresh-key cap
// (window fixed at 24h). Loopback traffic all shares 127.0.0.0/24.
func withKeyGate(cap int) stackOpt { return func(o *stackOpts) { o.keyGateCap = cap } }

// withSites wires the static-site deploy path on both the ssh and http side.
func withSites() stackOpt { return func(o *stackOpts) { o.sites = true } }

// withProxyProto requires a PROXY v1 header on every connection, so the
// default keyed and anon clients are not dialed; use dialWithProxy.
func withProxyProto() stackOpt { return func(o *stackOpts) { o.proxyProto = true } }

func startStack(t *testing.T, opts ...stackOpt) *stack {
	t.Helper()
	var o stackOpts
	for _, opt := range opts {
		opt(&o)
	}
	if o.proxyProto {
		t.Setenv("HOSTTHIS_SSH_PROXY_PROTOCOL", "true")
	}
	rawBlobs, err := storage.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	// Same wrapping as cmd/hostthisd: service and http surface both go through
	// the compression layer.
	blobUnit := service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(rawBlobs))
	repo := storagetest.NewRepo(t)
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	manage := service.NewManage(repo, blobUnit)

	httpSrv := &httpapi.Server{Pastes: repo, Blobs: blobUnit}
	sshSrv := &hostssh.Server{
		ApexDomain: "paste.test",
		Upload:     upload,
		Manage:     manage,
		Pastes:     repo,
		Logger:     log.New(io.Discard, "", 0),
	}
	if o.sites {
		sites := storage.NewSites(storagetest.NewRepo(t))
		httpSrv.Sites = sites
		httpSrv.ApexDomain = "paste.test"
		sshSrv.Deploy = service.NewDeploySite(sites, repo, blobUnit)
	}
	var keyGate *service.KeyGate
	if o.keyGateCap > 0 {
		keyGate = service.NewKeyGate(storagetest.NewRepo(t))
		keyGate.MaxFreshKeysPerSubnet = o.keyGateCap
		manage.KeyGate = keyGate
		sshSrv.KeyGate = keyGate
	}

	hs := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(hs.Close)
	sshSrv.BuildURL = func(s domain.Slug) string { return hs.URL + "/p/" + s.String() }

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close() // gliderlabs opens its own listener; this only reserved the port
	sshSrv.Addr = addr
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)

	s := &stack{t: t, httpURL: hs.URL, sshAddr: addr, repo: repo, upload: upload, keyGate: keyGate}
	if !o.proxyProto {
		s.keyed, s.keyedOwner = newKeyClient(t, addr)
		s.anon = newAnonClient(t, addr)
	}
	return s
}

// newKeyClient dials with a fresh ed25519 key and returns the client plus the
// owner fingerprint, so tests can assert ownership without parsing the live
// ssh greeting.
func newKeyClient(t *testing.T, addr string) (*xssh.Client, string) {
	t.Helper()
	signer, cfg := freshKeyConfig(t)
	cli, err := xssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, fingerprintSigner(signer.PublicKey())
}

// freshKeyConfig is a client config authenticating with a brand-new ed25519
// key. Host-key verification is off: the server is the test's own.
func freshKeyConfig(t *testing.T) (xssh.Signer, *xssh.ClientConfig) {
	t.Helper()
	_, priv, err := genEd25519()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, &xssh.ClientConfig{
		User:            "anyone",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
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

// dialWithProxy opens a TCP connection, writes a PROXY v1 header (family
// "TCP4" or "TCP6") claiming srcIP:srcPort, then runs an SSH handshake with a
// fresh key on top. PROXY v1 requires src and dst to share a family; the gate
// only ever reads src.
func dialWithProxy(t *testing.T, addr, family, srcIP string, srcPort int) *xssh.Client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dst := "127.0.0.1"
	if family == "TCP6" {
		dst = "::1"
	}
	if _, err := fmt.Fprintf(c, "PROXY %s %s %s %d 2222\r\n", family, srcIP, dst, srcPort); err != nil {
		t.Fatalf("write proxy header: %v", err)
	}
	_, cfg := freshKeyConfig(t)
	clientConn, chans, reqs, err := xssh.NewClientConn(c, addr, cfg)
	if err != nil {
		_ = c.Close()
		t.Fatalf("ssh handshake (with proxy header): %v", err)
	}
	cli := xssh.NewClient(clientConn, chans, reqs)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// run executes one ssh command on the default keyed client.
func (s *stack) run(cmd string, stdin []byte) (string, string, int) {
	s.t.Helper()
	return s.runOn(s.keyed, cmd, stdin)
}

func (s *stack) runAnon(cmd string, stdin []byte) (string, string, int) {
	s.t.Helper()
	return s.runOn(s.anon, cmd, stdin)
}

func (s *stack) runOn(cli *xssh.Client, cmd string, stdin []byte) (string, string, int) {
	s.t.Helper()
	return s.session(cli, cmd, stdin, false)
}

// runPty runs cmd on the default keyed client with a PTY allocated, which
// drives the PTY-vs-pipe rendering split.
func (s *stack) runPty(cmd string) (string, string, int) {
	s.t.Helper()
	return s.session(s.keyed, cmd, nil, true)
}

// session is the one session runner: (stdout, stderr, exit) for cmd over cli.
// After the command it drains the upload finalizer, so a read later in the
// same test sees a ready paste instead of racing the background blob write.
func (s *stack) session(cli *xssh.Client, cmd string, stdin []byte, pty bool) (string, string, int) {
	s.t.Helper()
	sess, err := cli.NewSession()
	if err != nil {
		s.t.Fatalf("session: %v", err)
	}
	defer sess.Close() //nolint:errcheck
	if pty {
		// The size and mode flags are arbitrary: only the PTY's presence
		// changes server behavior.
		modes := xssh.TerminalModes{xssh.ECHO: 0, xssh.TTY_OP_ISPEED: 14400, xssh.TTY_OP_OSPEED: 14400}
		if err := sess.RequestPty("xterm", 24, 80, modes); err != nil {
			s.t.Fatalf("requestpty: %v", err)
		}
	}
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
	s.upload.WaitFinalize()
	return stdout.String(), stderr.String(), exit
}

func asExitErr(err error, target **xssh.ExitError) bool {
	if e, ok := err.(*xssh.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// extractSlug pulls the slug off a path-mode URL (httpURL+"/p/<slug>").
func extractSlug(stdoutURL string) string {
	url := strings.TrimSpace(stdoutURL)
	i := strings.LastIndex(url, "/p/")
	if i == -1 {
		return ""
	}
	return url[i+len("/p/"):]
}

// httpGet GETs url and returns the response with its body already read and
// closed.
func httpGet(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(got)
}

// getBody is httpGet reduced to (status, body).
func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, body := httpGet(t, url)
	return resp.StatusCode, body
}

func makeSiteArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}
