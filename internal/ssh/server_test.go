package ssh_test

import (
	"bytes"
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
)

// TestUploadAndServe pins the end-to-end round trip: a real ssh client pipes
// HTML into the real SSH + HTTP stack on localhost ports, and a GET of the
// returned URL yields the same bytes with the expected sandbox headers.
func TestUploadAndServe(t *testing.T) {
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
	// Compression wrapper mirrors the production wiring in cmd/hostthisd.
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storage.NewPasteRepo(db)
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)

	httpSrv := httptest.NewServer((&httpapi.Server{Pastes: repo, Blobs: blobUnit}).Handler())
	t.Cleanup(httpSrv.Close)

	sshListener := mustListen(t)
	sshAddr := sshListener.Addr().String()
	_ = sshListener.Close() // gliderlabs opens its own listener; this only reserved the port

	sshSrv := &hostssh.Server{
		Addr:       sshAddr,
		ApexDomain: "paste.test",
		Upload:     upload,
		BuildURL: func(s domain.Slug) string {
			// Path-mode shape, pointing at the httptest server.
			return httpSrv.URL + "/p/" + s.String()
		},
		Logger: log.New(io.Discard, "", 0),
	}
	go func() {
		_ = sshSrv.ListenAndServe()
	}()
	waitForSSH(t, sshAddr)

	// A fresh ed25519 key: uploads require an authenticated identity. Host-key
	// verification is off because the server is this test's own localhost one.
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

	htmlBody := []byte("<!doctype html><h1>integration ok</h1>")
	sess.Stdin = bytes.NewReader(htmlBody)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// Empty command = upload, matching "cat foo | ssh paste.test".
	if err := sess.Run(""); err != nil {
		t.Fatalf("ssh run: %v\nstderr: %s", err, stderr.String())
	}

	url := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(url, httpSrv.URL+"/p/") {
		t.Fatalf("stdout doesn't look like a paste URL: %q (stderr: %q)",
			stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "expires in 30 days") {
		t.Fatalf("stderr should mention expiry, got %q", stderr.String())
	}

	// The blob write and the flip to ready run in a background finalizer, so
	// the GET below needs it drained first.
	upload.WaitFinalize()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != 200 {
		t.Fatalf("http status: got %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, htmlBody) {
		t.Fatalf("body mismatch:\n got  %q\n want %q", got, htmlBody)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q, want text/html…", ct)
	}
	// Paste reads carry NO Content-Security-Policy: origin isolation is the
	// security boundary, not CSP. Pinned so adding one has to be deliberate.
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
		t.Fatalf("Content-Security-Policy should be unset on paste reads, got %q", csp)
	}
	if xfo := resp.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Fatalf("X-Frame-Options: got %q, want DENY", xfo)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp != "no-referrer" {
		t.Fatalf("Referrer-Policy: got %q, want no-referrer", rp)
	}
	if pp := resp.Header.Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") {
		t.Fatalf("Permissions-Policy: got %q, want camera=() at minimum", pp)
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return l
}

// waitForSSH polls the SSH port until it accepts a TCP connection, because
// gliderlabs's ListenAndServe exposes no "ready" signal.
func waitForSSH(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ssh server never came up on %s", addr)
}
