package ssh_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"github.com/Zamua/hostthis/internal/storagetest"
)

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

// A gzip-tar archive piped through a real ssh client, with no verb and no flag,
// deploys a site whose files each serve back with the right content-type and
// sandbox headers, and whose route-shaped misses fall back to the root index.
func TestDeploySiteAndServe(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storagetest.NewRepo(t)
	sites := storage.NewArtifactSites(storagetest.NewRepo(t))
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	deploy := service.NewDeploySite(sites, repo, blobUnit)

	httpSrv := httptest.NewServer((&httpapi.Server{
		Pastes: repo, Sites: sites, Blobs: blobUnit, ApexDomain: "paste.test",
	}).Handler())
	t.Cleanup(httpSrv.Close)

	sshListener := mustListen(t)
	sshAddr := sshListener.Addr().String()
	_ = sshListener.Close()

	sshSrv := &hostssh.Server{
		Addr:       sshAddr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Deploy:     deploy,
		BuildURL: func(s domain.Slug) string {
			// Path mode: site files hang off /p/<slug>/<path>.
			return httpSrv.URL + "/p/" + s.String()
		},
		Logger: log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, sshAddr)

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
	client, err := xssh.Dial("tcp", sshAddr, cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()

	arc := makeSiteArchive(t, map[string]string{
		"index.html":    "<!doctype html><h1>home</h1>",
		"css/style.css": "body{color:green}",
		"about.html":    "<h1>about</h1>",
	})
	sess.Stdin = bytes.NewReader(arc)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// An empty command is an upload; the gzip magic routes it to the site path.
	if err := sess.Run(""); err != nil {
		t.Fatalf("ssh run: %v\nstderr: %s", err, stderr.String())
	}

	base := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(base, httpSrv.URL+"/p/") {
		t.Fatalf("stdout doesn't look like a URL: %q (stderr %q)", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "site:") {
		t.Fatalf("stderr should mention site, got %q", stderr.String())
	}

	checks := []struct {
		path  string
		body  string
		ctype string
	}{
		{"/", "<!doctype html><h1>home</h1>", "text/html; charset=utf-8"},
		{"/about.html", "<h1>about</h1>", "text/html; charset=utf-8"},
		{"/css/style.css", "body{color:green}", "text/css; charset=utf-8"},
	}
	for _, c := range checks {
		url := base + strings.TrimSuffix(c.path, "/")
		if c.path == "/" {
			url = base // bare slug → index.html
		}
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: status %d", url, resp.StatusCode)
		}
		if string(got) != c.body {
			t.Fatalf("GET %s body: got %q, want %q", url, got, c.body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.ctype {
			t.Fatalf("GET %s content-type: got %q, want %q", url, ct, c.ctype)
		}
		if resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("GET %s missing sandbox header", url)
		}
	}

	// A route-shaped miss (no extension, or ".html") serves the ROOT index.html
	// with a 200 so a client-side router can render the route.
	rootIndex := "<!doctype html><h1>home</h1>"
	routeChecks := []string{"/about-page", "/does-not-exist.html", "/users/42"}
	for _, p := range routeChecks {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatalf("GET route %s: %v", p, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET route %s: status %d, want 200 (SPA fallback)", p, resp.StatusCode)
		}
		if string(got) != rootIndex {
			t.Fatalf("GET route %s body: got %q, want root index %q", p, got, rootIndex)
		}
	}

	// An asset-shaped miss still 404s: never index.html served as JS.
	resp, err := http.Get(base + "/assets/nope.js")
	if err != nil {
		t.Fatalf("GET missing asset: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing asset: got %d, want 404", resp.StatusCode)
	}
}

// An archive with no web content is rejected like any unsupported upload, with
// a nonzero exit and an explanatory stderr line.
func TestDeploySite_NoWebContentRejected(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storagetest.NewRepo(t)
	sites := storage.NewArtifactSites(storagetest.NewRepo(t))

	sshListener := mustListen(t)
	sshAddr := sshListener.Addr().String()
	_ = sshListener.Close()

	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	sshSrv := &hostssh.Server{
		Addr:       sshAddr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Deploy:     service.NewDeploySite(sites, repo, blobUnit),
		BuildURL:   func(s domain.Slug) string { return "http://x/p/" + s.String() },
		Logger:     log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, sshAddr)

	_, priv, _ := genEd25519()
	signer, _ := xssh.NewSignerFromKey(priv)
	client, err := xssh.Dial("tcp", sshAddr, &xssh.ClientConfig{
		User:            "anyone",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sess, _ := client.NewSession()
	defer sess.Close()

	arc := makeSiteArchive(t, map[string]string{"data.json": "{}", "notes.txt": "hi"})
	sess.Stdin = bytes.NewReader(arc)
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	err = sess.Run("")
	if err == nil {
		t.Fatalf("expected nonzero exit for no-web-content archive")
	}
	if !strings.Contains(stderr.String(), "no web content") {
		t.Fatalf("stderr should explain rejection, got %q", stderr.String())
	}
}
