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

// siteStack is a full real stack (sqlite + blob store + http surface +
// ssh server) with the site-deploy path WIRED. The no-regression tests
// below use it to prove that wiring DeploySite does not change the
// single-file paste path: a plain .html still routes to a paste, only a
// gzip-tar routes to a site.
type siteStack struct {
	httpURL string
	sshAddr string
}

func newSiteStack(t *testing.T) *siteStack {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	repo := storage.NewPasteRepo(db)
	sites := storage.NewSiteRepo(db)

	httpSrv := httptest.NewServer((&httpapi.Server{
		Pastes: repo, Sites: sites, Blobs: blobs, ApexDomain: "paste.test",
	}).Handler())
	t.Cleanup(httpSrv.Close)

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close()

	sshSrv := &hostssh.Server{
		Addr:       addr,
		ApexDomain: "paste.test",
		Upload:     service.NewUpload(repo, blobs),
		Manage:     service.NewManage(repo, blobs),
		Deploy:     service.NewDeploySite(sites, repo, blobs),
		BuildURL:   func(s domain.Slug) string { return httpSrv.URL + "/p/" + s.String() },
		Logger:     log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)

	return &siteStack{httpURL: httpSrv.URL, sshAddr: addr}
}

// runUpload pipes body over a real ssh client (empty command = upload)
// and returns (stdout, stderr).
func (s *siteStack) runUpload(t *testing.T, body []byte) (string, string) {
	t.Helper()
	_, priv, err := genEd25519()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	client, err := xssh.Dial("tcp", s.sshAddr, &xssh.ClientConfig{
		User:            "anyone",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(""); err != nil {
		t.Fatalf("ssh run: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestSinglePasteUnchangedWhenDeployWired pins the no-regression property:
// with the site-deploy path WIRED on the SSH server, a plain single-file
// .html upload STILL routes to the paste path, not the site path. The
// gzip-magic sniff only diverts gzip-tar archives; everything else is a
// single-file paste exactly as before.
func TestSinglePasteUnchangedWhenDeployWired(t *testing.T) {
	st := newSiteStack(t)
	stdout, stderr := st.runUpload(t, []byte("<!doctype html><h1>just a paste</h1>"))

	url := strings.TrimSpace(stdout)
	if !strings.HasPrefix(url, st.httpURL+"/p/") {
		t.Fatalf("expected a paste URL on stdout, got %q (stderr %q)", stdout, stderr)
	}
	// The paste path says "expires in 7 days"; the site path says "site: N
	// file(s)". A single .html must take the PASTE path.
	if strings.Contains(stderr, "site:") {
		t.Fatalf("single .html wrongly routed to the site path: stderr %q", stderr)
	}
	if !strings.Contains(stderr, "expires in 7 days") {
		t.Fatalf("expected paste-path stderr, got %q", stderr)
	}

	// And it serves as a single-file paste: the whole body at the slug
	// root, byte-exact, as text/html.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET paste: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("paste GET status %d", resp.StatusCode)
	}
	if string(got) != "<!doctype html><h1>just a paste</h1>" {
		t.Fatalf("paste body mismatch: %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("paste content-type: got %q", ct)
	}
}

// TestArchiveAndPasteCoexist pins that the SAME wired server routes a
// gzip-tar to a SITE and a plain file to a PASTE, with the standard
// project asset layout (index.html + css/app.css + js/app.js) round-
// tripping byte-exact off the site URL with correct content-types.
func TestArchiveAndPasteCoexist(t *testing.T) {
	st := newSiteStack(t)

	// 1. Archive -> site.
	arc := makeSiteArchive(t, map[string]string{
		"index.html":  "<!doctype html><h1>home</h1>",
		"css/app.css": "body{color:teal}",
		"js/app.js":   "console.log('app')",
	})
	siteOut, siteErr := st.runUpload(t, arc)
	base := strings.TrimSpace(siteOut)
	if !strings.Contains(siteErr, "site:") {
		t.Fatalf("archive should route to site path, stderr %q", siteErr)
	}
	checks := []struct {
		path  string
		body  string
		ctype string
	}{
		{"", "<!doctype html><h1>home</h1>", "text/html; charset=utf-8"},
		{"/css/app.css", "body{color:teal}", "text/css; charset=utf-8"},
		{"/js/app.js", "console.log('app')", "text/javascript; charset=utf-8"},
	}
	for _, c := range checks {
		resp, err := http.Get(base + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s status %d", c.path, resp.StatusCode)
		}
		if string(got) != c.body {
			t.Fatalf("GET %s body: got %q want %q", c.path, got, c.body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.ctype {
			t.Fatalf("GET %s ctype: got %q want %q", c.path, ct, c.ctype)
		}
	}
	// Unmatched path 404s (no SPA fallback).
	resp, err := http.Get(base + "/nope/missing.js")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing site path: got %d, want 404", resp.StatusCode)
	}

	// 2. Plain file -> paste, on the SAME server. Different slug, paste path.
	pasteOut, pasteErr := st.runUpload(t, []byte("<!doctype html><p>sibling paste</p>"))
	if strings.Contains(pasteErr, "site:") {
		t.Fatalf("plain file wrongly routed to site path: %q", pasteErr)
	}
	if strings.TrimSpace(pasteOut) == base {
		t.Fatalf("paste and site got the same slug %q", base)
	}
}
