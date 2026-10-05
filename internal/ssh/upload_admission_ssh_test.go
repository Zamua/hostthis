package ssh_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// slowBlobs records how many Puts run at once and can park them until
// released, so a test can hold an upload inside its synchronous blob write.
type slowBlobs struct {
	storage.InnerBlobStore
	delay    time.Duration
	park     atomic.Bool
	unpark   chan struct{}
	once     sync.Once
	parked   atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func newSlowBlobs() *slowBlobs { return &slowBlobs{unpark: make(chan struct{})} }

func (b *slowBlobs) wrap(raw storage.InnerBlobStore) storage.InnerBlobStore {
	b.InnerBlobStore = raw
	return b
}

func (b *slowBlobs) Put(key string, r io.Reader, size int64) error {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for p := b.peak.Load(); n > p && !b.peak.CompareAndSwap(p, n); p = b.peak.Load() {
	}
	if b.park.Load() {
		b.parked.Add(1)
		<-b.unpark
	}
	time.Sleep(b.delay)
	return b.InnerBlobStore.Put(key, r, size)
}

// release lets every parked and future Put through.
func (b *slowBlobs) release() {
	b.once.Do(func() {
		b.park.Store(false)
		close(b.unpark)
	})
}

type result struct {
	stdout, stderr string
	exit           int
	err            error
}

// runAsync runs cmd on its own connection, so concurrent uploads never share
// an SSH client.
func runAsync(t *testing.T, s *stack, cmd string, stdin io.Reader) <-chan result {
	t.Helper()
	_, cfg := freshKeyConfig(t)
	cli, err := xssh.Dial("tcp", s.sshAddr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	out := make(chan result, 1)
	go func() { out <- runOnce(cli, cmd, stdin) }()
	return out
}

// runKeyed runs cmd on the stack's keyed client without blocking the test, so
// a gate regression fails on a timeout instead of hanging.
func runKeyed(s *stack, cmd string, stdin io.Reader) <-chan result {
	out := make(chan result, 1)
	go func() { out <- runOnce(s.keyed, cmd, stdin) }()
	return out
}

func runOnce(cli *xssh.Client, cmd string, stdin io.Reader) result {
	sess, err := cli.NewSession()
	if err != nil {
		return result{err: err}
	}
	defer sess.Close() //nolint:errcheck
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr, sess.Stdin = &stdout, &stderr, stdin
	res := result{}
	if err := sess.Run(cmd); err != nil {
		if e, ok := err.(*xssh.ExitError); ok {
			res.exit = e.ExitStatus()
		} else {
			res.err = err
		}
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func await(t *testing.T, ch <-chan result, within time.Duration) result {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("session: %v (stderr %q)", r.err, r.stderr)
		}
		return r
	case <-time.After(within):
		t.Fatalf("session did not finish within %s", within)
		return result{}
	}
}

func mustAdmission(t *testing.T, limit int, wait time.Duration) *hostssh.UploadAdmission {
	t.Helper()
	a, err := hostssh.NewUploadAdmission(limit, wait)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// holdSlot starts a site deploy that parks in its blob write, so it occupies
// one admission slot until blobs.release.
func holdSlot(t *testing.T, s *stack, blobs *slowBlobs) <-chan result {
	t.Helper()
	t.Cleanup(blobs.release)
	blobs.park.Store(true)
	before := blobs.parked.Load()
	arc := makeSiteArchive(t, map[string]string{"index.html": fmt.Sprintf("<h1>held %d</h1>", before)})
	ch := runAsync(t, s, "", bytes.NewReader(arc))
	waitFor(t, func() bool { return blobs.parked.Load() > before })
	return ch
}

// At most the limit of uploads run past admission at once, and the rest
// queue rather than fail.
func TestUploadAdmission_BoundsConcurrentUploads(t *testing.T) {
	blobs := newSlowBlobs()
	blobs.delay = 150 * time.Millisecond
	s := startStack(t, withSites(), withRawBlobs(blobs.wrap), withUploads(mustAdmission(t, 2, 30*time.Second)))

	var chans []<-chan result
	for i := range 5 {
		arc := makeSiteArchive(t, map[string]string{"index.html": fmt.Sprintf("<h1>%d</h1>", i)})
		chans = append(chans, runAsync(t, s, "", bytes.NewReader(arc)))
	}
	for i, ch := range chans {
		if r := await(t, ch, 15*time.Second); r.exit != hostssh.ExitOK {
			t.Fatalf("upload %d: exit %d, stderr %q", i, r.exit, r.stderr)
		}
	}
	if got := blobs.peak.Load(); got != 2 {
		t.Fatalf("peak concurrent uploads = %d, want 2", got)
	}
}

// countingReader counts how many bytes the client has pulled to send.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// A queued upload's body stays on the client: the server reads none of it,
// so the client stalls once the SSH channel window fills.
func TestUploadAdmission_WaitingUploadReadsNoBody(t *testing.T) {
	blobs := newSlowBlobs()
	gate := mustAdmission(t, 1, 30*time.Second)
	s := startStack(t, withSites(), withRawBlobs(blobs.wrap), withUploads(gate))
	held := holdSlot(t, s, blobs)

	const bodySize = 8 << 20
	body := &countingReader{r: strings.NewReader(strings.Repeat("a", bodySize))}
	queued := runAsync(t, s, "", body)
	waitFor(t, func() bool { return gate.Waiting() == 1 })
	time.Sleep(300 * time.Millisecond)

	// The receive window the SSH library grants a channel, plus copy slack.
	const window = 2<<20 + 256<<10
	if got := body.n.Load(); got > window {
		t.Fatalf("client sent %d bytes while queued, want at most the %d-byte channel window", got, window)
	}

	blobs.release()
	if r := await(t, held, 10*time.Second); r.exit != hostssh.ExitOK {
		t.Fatalf("held upload: exit %d, stderr %q", r.exit, r.stderr)
	}
	if r := await(t, queued, 10*time.Second); r.exit != hostssh.ExitOK {
		t.Fatalf("queued upload: exit %d, stderr %q", r.exit, r.stderr)
	}
	if got := body.n.Load(); got != bodySize {
		t.Fatalf("queued upload sent %d bytes after admission, want %d", got, bodySize)
	}
}

// An upload that waits past the limit gets the busy message, on every
// upload verb.
func TestUploadAdmission_WaitExpiryIsBusy(t *testing.T) {
	blobs := newSlowBlobs()
	s := startStack(t, withSites(), withRawBlobs(blobs.wrap), withUploads(mustAdmission(t, 1, 200*time.Millisecond)))
	pasteOut, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	arc := makeSiteArchive(t, map[string]string{"index.html": "<h1>v1</h1>"})
	siteOut, _, _ := s.run("", arc)

	held := holdSlot(t, s, blobs)
	defer func() {
		blobs.release()
		await(t, held, 10*time.Second)
	}()

	for _, tc := range []struct {
		name  string
		slug  string
		stdin []byte
	}{
		{"new paste", "", []byte("<!doctype html><p>new</p>")},
		{"paste update", extractSlug(pasteOut), []byte("<!doctype html><p>v2</p>")},
		{"site deploy", "", arc},
		{"site redeploy", extractSlug(siteOut), arc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := await(t, runKeyed(s, tc.slug, bytes.NewReader(tc.stdin)), 5*time.Second)
			if r.exit != hostssh.ExitErr {
				t.Fatalf("exit = %d, want %d (stderr %q)", r.exit, hostssh.ExitErr, r.stderr)
			}
			if want := "hostthis: busy storing other uploads; try again in a minute\n"; r.stderr != want {
				t.Fatalf("stderr = %q, want %q", r.stderr, want)
			}
			if r.stdout != "" {
				t.Fatalf("stdout = %q, want no URL", r.stdout)
			}
		})
	}
}

// A client that disconnects mid-upload frees its slot for the next upload.
func TestUploadAdmission_DisconnectReleasesSlot(t *testing.T) {
	s := startStack(t, withUploads(mustAdmission(t, 1, 300*time.Millisecond)))

	_, cfg := freshKeyConfig(t)
	cli, err := xssh.Dial("tcp", s.sshAddr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	pr, pw := io.Pipe()
	done := make(chan result, 1)
	go func() { done <- runOnce(cli, "", pr) }()
	// More than the channel window, so the write completes only once the
	// server is reading, which proves the partial upload holds the only slot.
	if _, err := pw.Write([]byte(strings.Repeat("a", 3<<20))); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, stderr, exit := s.run("", []byte("<!doctype html><p>probe</p>"))
	if exit != hostssh.ExitErr || !strings.Contains(stderr, "busy") {
		t.Fatalf("probe while slot held: exit %d, stderr %q; want busy", exit, stderr)
	}

	_ = cli.Close()
	_ = pw.Close()
	<-done

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, stderr, exit := s.run("", []byte("<!doctype html><p>after</p>"))
		if exit == hostssh.ExitOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never released after disconnect: exit %d, stderr %q", exit, stderr)
		}
	}
}

// Reads are not gated: `list`, `get` and HTTP serve while every slot is held.
func TestUploadAdmission_ReadsUngated(t *testing.T) {
	blobs := newSlowBlobs()
	s := startStack(t, withSites(), withRawBlobs(blobs.wrap), withUploads(mustAdmission(t, 1, time.Minute)))
	out, _, _ := s.run("", []byte("<!doctype html><p>readable</p>"))
	url := strings.TrimSpace(out)
	slug := extractSlug(out)

	held := holdSlot(t, s, blobs)
	defer func() {
		blobs.release()
		await(t, held, 10*time.Second)
	}()

	for _, cmd := range []string{"list", "get " + slug} {
		if r := await(t, runKeyed(s, cmd, nil), 5*time.Second); r.exit != hostssh.ExitOK {
			t.Fatalf("%s: exit %d, stderr %q", cmd, r.exit, r.stderr)
		}
	}

	httpDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		httpDone <- err
	}()
	select {
	case err := <-httpDone:
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP read blocked while upload slots were held")
	}
}
