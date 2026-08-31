package ssh_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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

// TestUploadAndServe pins the end-to-end round trip: a real ssh client pipes
// HTML into the real SSH + HTTP stack on localhost ports, and a GET of the
// returned URL yields the same bytes with the expected sandbox headers.
func TestUploadAndServe(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	// Compression wrapper mirrors the production wiring in cmd/hostthisd.
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storagetest.NewRepo(t)
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

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func TestServerRejectsHandshakeThatCompletesAfterShutdown(t *testing.T) {
	listener := mustListen(t)
	addr := listener.Addr().String()
	_ = listener.Close()
	srv := &hostssh.Server{
		Addr: addr, ApexDomain: "paste.test", Logger: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ListenAndServe() }()
	waitForSSH(t, addr)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial pre-auth connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	reader := bufio.NewReader(conn)
	banner, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read server banner: %v", err)
	}
	buffered := &replayConn{Conn: conn, reader: io.MultiReader(strings.NewReader(banner), reader)}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, privateKey, err := genEd25519()
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	config := &xssh.ClientConfig{
		User: "anyone", Auth: []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), Timeout: time.Second,
	}
	clientConn, channels, requests, err := xssh.NewClientConn(buffered, addr, config)
	if err != nil {
		t.Fatalf("finish accepted handshake: %v", err)
	}
	client := xssh.NewClient(clientConn, channels, requests)
	defer client.Close() //nolint:errcheck
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new post-shutdown session: %v", err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err := session.Run(""); err == nil {
		t.Fatal("post-shutdown session succeeded")
	}
	if !strings.Contains(stderr.String(), "service restarting") {
		t.Fatalf("post-shutdown stderr = %q", stderr.String())
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServe did not return after shutdown")
	}
}

type blockingPasteRepo struct {
	entered chan struct{}
	release chan struct{}
	paste   domain.Paste
}

func (r *blockingPasteRepo) InsertWithQuotaCheck(_ context.Context, paste domain.Paste, _ int64, _ time.Time) error {
	r.paste = paste
	close(r.entered)
	<-r.release
	return nil
}

func (r *blockingPasteRepo) Get(domain.Slug) (domain.Paste, error) { return r.paste, nil }
func (r *blockingPasteRepo) MarkReady(domain.Paste) error          { return nil }
func (r *blockingPasteRepo) MarkFailed(domain.Paste) error         { return nil }

func TestServerForceCloseWaitsForActiveUploadHandler(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	repo := &blockingPasteRepo{entered: make(chan struct{}), release: make(chan struct{})}
	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(rawBlobs)))

	listener := mustListen(t)
	addr := listener.Addr().String()
	_ = listener.Close()
	srv := &hostssh.Server{
		Addr: addr, ApexDomain: "paste.test", Upload: upload,
		BuildURL: func(slug domain.Slug) string { return "https://" + slug.String() + ".paste.test" },
		Logger:   log.New(io.Discard, "", 0),
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ListenAndServe() }()
	waitForSSH(t, addr)

	_, privateKey, err := genEd25519()
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	client, err := xssh.Dial("tcp", addr, &xssh.ClientConfig{
		User: "anyone", Auth: []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("dial active SSH connection: %v", err)
	}
	defer client.Close() //nolint:errcheck
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	session.Stdin = strings.NewReader("<!doctype html><h1>shutdown</h1>")
	runDone := make(chan error, 1)
	go func() { runDone <- session.Run("") }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("upload did not reach metadata insert")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("graceful shutdown = %v, want deadline", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("force close returned before active handler: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(repo.release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("force close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("force close did not wait for handler completion")
	}
	upload.WaitFinalize()
	<-runDone
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServe did not return after Close")
	}
}

func TestServerShutdownStopsListeningAndCloseForcesConnections(t *testing.T) {
	listener := mustListen(t)
	addr := listener.Addr().String()
	_ = listener.Close()
	srv := &hostssh.Server{
		Addr: addr, ApexDomain: "paste.test", Logger: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ListenAndServe() }()
	waitForSSH(t, addr)

	_, privateKey, err := genEd25519()
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	client, err := xssh.Dial("tcp", addr, &xssh.ClientConfig{
		User: "anyone", Auth: []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("dial active SSH connection: %v", err)
	}
	defer client.Close() //nolint:errcheck
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with active connection = %v, want deadline", err)
	}
	select {
	case err := <-clientDone:
		t.Fatalf("graceful shutdown closed active client: %v", err)
	default:
	}
	if conn, err := net.DialTimeout("tcp", addr, 25*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("new connection succeeded after shutdown")
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("force close: %v", err)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("active SSH client survived force close")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServe did not return after Close")
	}
}

func TestServerShutdownBeforeListenPreventsStartup(t *testing.T) {
	listener := mustListen(t)
	addr := listener.Addr().String()
	_ = listener.Close()
	srv := &hostssh.Server{
		Addr: addr, ApexDomain: "paste.test", Logger: log.New(io.Discard, "", 0),
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown before listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listen after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServe started after shutdown")
	}
	if conn, err := net.DialTimeout("tcp", addr, 25*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("listener survived shutdown-before-start")
	}
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
