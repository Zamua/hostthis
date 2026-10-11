package parity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/celldtest"
	"github.com/Zamua/hostthis/internal/domain"
)

// The apex both servers serve locally, in production's shape (subdomain URLs,
// no path mode). Every name under it resolves to loopback.
const localApex = "localhost"

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// localWorld builds hostthisd, starts it against a Worker whose public surface
// is on, and uploads the corpus.
func localWorld(t *testing.T) *world {
	t.Helper()
	w := localServers(t)
	w.buildCorpus(t)
	return w
}

// localServers is localWorld before any upload.
func localServers(t *testing.T) *world {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hostthisd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/hostthisd")
	build.Dir = repoRoot()
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hostthisd: %v\n%s", err, out)
	}
	rt := celldtest.StartPublic(t, celldtest.Public{
		Apex:           localApex,
		Scheme:         "http",
		ClientIPSource: "xff-rightmost",
	})
	d := startDaemon(t, bin, rt)
	endpoint := strings.TrimPrefix(rt.URL, "http://")
	return &world{
		local:     true,
		apex:      localApex,
		hostthisd: &server{name: "hostthisd", via: viaHostthisd, addr: d.httpAddr, trusts: true},
		workers: []*server{
			{name: "worker", via: viaDirect, addr: endpoint, trusts: true},
			{name: "worker (forwarded)", via: viaForwarded, addr: endpoint, token: rt.Token},
		},
		cell:     newCellAPI(t, rt.URL, rt.Token),
		ssh:      d.ssh,
		writable: true,
	}
}

// daemon is a running hostthisd.
type daemon struct {
	httpAddr string
	ssh      *sshClient
}

// freeAddrs holds n listeners at once so no address repeats, then frees them.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var addrs []string
	var ls []net.Listener
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		addrs = append(addrs, l.Addr().String())
	}
	for _, l := range ls {
		_ = l.Close()
	}
	return addrs
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startDaemon(t *testing.T, bin string, rt *celldtest.PublicRuntime) *daemon {
	t.Helper()
	addrs := freeAddrs(t, 3)
	cmd := exec.Command(bin)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOSTTHIS_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env,
		"HOSTTHIS_APEX_DOMAIN="+localApex,
		"HOSTTHIS_URL_MODE=subdomain",
		"HOSTTHIS_PUBLIC_SCHEME=http",
		"HOSTTHIS_HTTP_ADDR="+addrs[0],
		"HOSTTHIS_SSH_ADDR="+addrs[1],
		"HOSTTHIS_METRICS_ADDR="+addrs[2],
		"HOSTTHIS_DATA_DIR="+t.TempDir(),
		"HOSTTHIS_CELLD_ENDPOINT="+rt.URL,
		"HOSTTHIS_CELLD_TOKEN="+rt.Token,
		// The client address comes from X-Forwarded-For on both servers, so
		// requests can choose their room creation bucket.
		"HOSTTHIS_HTTP_TRUST_XFF=true",
		"HOSTTHIS_LANDING="+filepath.Join(repoRoot(), "web", "landing.html"),
	)
	logs := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		if t.Failed() {
			t.Logf("hostthisd log:\n%s", logs.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := http.Get("http://" + addrs[0] + "/healthz")
		if err == nil {
			_ = res.Body.Close()
			if c, err := net.Dial("tcp", addrs[1]); err == nil {
				_ = c.Close()
				break
			}
		}
		select {
		case <-exited:
			t.Fatalf("hostthisd exited before serving:\n%s", logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("hostthisd not serving after 30s:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &daemon{httpAddr: addrs[0], ssh: newSSHClient(t, addrs[1])}
}

// sshClient runs verbs as one identity, the way a user's ssh does.
type sshClient struct {
	addr   string
	config *xssh.ClientConfig
}

func newSSHClient(t *testing.T, addr string) *sshClient {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &sshClient{addr: addr, config: &xssh.ClientConfig{
		User:            "parity",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // a daemon this test started
		Timeout:         15 * time.Second,
	}}
}

// run sends command with stdin and returns stdout, failing t on a non-zero
// exit.
func (c *sshClient) run(t *testing.T, command string, stdin []byte) string {
	t.Helper()
	conn, err := xssh.Dial("tcp", c.addr, c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	s, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	var out, errOut bytes.Buffer
	s.Stdin, s.Stdout, s.Stderr = bytes.NewReader(stdin), &out, &errOut
	if err := s.Run(command); err != nil {
		t.Fatalf("ssh %q: %v\n%s", command, err, errOut.String())
	}
	return out.String()
}

// upload creates a paste and returns its slug once it serves its content.
func (w *world) upload(t *testing.T, args string, content []byte) string {
	t.Helper()
	out := strings.TrimSpace(w.ssh.run(t, args, content))
	u, err := url.Parse(out)
	if err != nil || !strings.HasSuffix(u.Host, "."+w.apex) {
		t.Fatalf("upload printed %q, not a subdomain URL", out)
	}
	slug := strings.TrimSuffix(u.Host, "."+w.apex)
	w.waitReady(t, slug)
	return slug
}

// waitReady blocks until slug is no longer pending.
func (w *world) waitReady(t *testing.T, slug string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, err := w.cell.pastes.Get(domain.Slug(slug))
		if err == nil && p.Status != domain.PasteStatusPending {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("paste %s still pending after 20s", slug)
}

// cellAPI is the Worker's internal endpoint, which forces states no upload
// reaches: a paste stuck pending or failed, a damaged object or manifest, a
// full creation ledger.
type cellAPI struct {
	base   string
	client *http.Client
	pastes *celld.PasteRepo
}

func newCellAPI(t *testing.T, base, token string) *cellAPI {
	t.Helper()
	rt, err := celld.Authorize(nil, base, token)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: rt}
	return &cellAPI{base: base, client: client, pastes: celld.NewPasteRepo(base, client)}
}

func (c *cellAPI) call(t *testing.T, method, path string, body io.Reader) []byte {
	t.Helper()
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		t.Fatalf("%s %s = %d %s", method, path, res.StatusCode, out)
	}
	return out
}

// pendingPaste inserts a row whose bytes never land, as an upload looks until
// its finalizer runs.
func (c *cellAPI) pendingPaste(t *testing.T, kind domain.ContentKind) domain.Paste {
	t.Helper()
	now := time.Now().UTC()
	upload := domain.NewUploadID()
	p := domain.Paste{
		Slug:       domain.NewRandomSlug(),
		Identity:   "key:SHA256:parity-forced",
		Generation: domain.NewPasteGeneration(),
		Status:     domain.PasteStatusPending,
		Kind:       kind,
		UploadID:   upload,
		Size:       5,
		CreatedAt:  now,
		UpdatedAt:  now,
		Manifest: domain.DocumentManifest(domain.ManifestEntry{
			Key: domain.UploadObjectKey(upload, 0), Size: 5, CompressedSize: 5, Kind: string(kind),
		}),
	}
	if err := c.pastes.InsertWithQuotaCheck(context.Background(), p, int64(domain.UserQuotaBytes), now); err != nil {
		t.Fatalf("insert pending paste: %v", err)
	}
	return p
}

func (c *cellAPI) failedPaste(t *testing.T, kind domain.ContentKind) domain.Paste {
	t.Helper()
	p := c.pendingPaste(t, kind)
	if err := c.pastes.MarkFailed(p); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	return p
}

// damageObject replaces slug's served object with bytes that are not in the
// at-rest format.
func (c *cellAPI) damageObject(t *testing.T, slug string) {
	t.Helper()
	p, err := c.pastes.Get(domain.Slug(slug))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := p.Manifest.Files[domain.Root]
	if !ok || entry.Key == "" {
		t.Fatalf("paste %s has no root object", slug)
	}
	c.call(t, "PUT", "/blob/"+entry.Key, strings.NewReader("not the at-rest format"))
}

// damagedManifest writes a ready row whose manifest does not decode.
func (c *cellAPI) damagedManifest(t *testing.T) string {
	t.Helper()
	slug := domain.NewRandomSlug().String()
	now := time.Now().UnixMilli()
	body := fmt.Sprintf(`{"generation":"parity-damaged","fingerprint":"parity","row":{"slug":%q,`+
		`"identity":"key:SHA256:parity-forced","status":"ready","kind":"html","size":3,"createdAt":%d,`+
		`"updatedAt":%d,"uploadId":%q,"manifest":"oops"}}`, slug, now, now, domain.NewUploadID())
	c.call(t, "POST", "/paste/put?slug="+slug, strings.NewReader(body))
	return slug
}

// fillRoomLedger records n room creations for app from subnet, as if they
// had just happened.
func (c *cellAPI) fillRoomLedger(t *testing.T, app, subnet string, n int) {
	t.Helper()
	now := time.Now().UnixMilli()
	for i := range n {
		body := fmt.Sprintf(`{"id":"parity-ledger-%s-%d","subnet":%q,"at":%d}`, subnet, i, subnet, now)
		c.call(t, "POST", "/paste/roomcreated?slug="+app, strings.NewReader(body))
	}
}
