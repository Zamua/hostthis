package ssh_test

import (
	"bytes"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// workerLog records the Worker requests hostthisd makes, as "METHOD /path"
// with the test namespace and the query dropped.
type workerLog struct {
	mu   sync.Mutex
	reqs []string
	next http.RoundTripper
}

var nsPrefix = regexp.MustCompile(`^/ns/[^/]+`)

func (l *workerLog) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, req.Method+" "+nsPrefix.ReplaceAllString(req.URL.Path, ""))
	l.mu.Unlock()
	return l.next.RoundTrip(req)
}

func (l *workerLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.reqs
	l.reqs = nil
	return out
}

// The background finalize's requests: the bytes, then the status flip.
func isFinalize(req string) bool {
	return strings.HasPrefix(req, "PUT /blob/") || req == "POST /paste/status"
}

// A plain upload from a known key waits on two Worker requests: the Sybil
// gate's admission and one publish. Pinned so a new round trip on the path a
// client waits on cannot land unnoticed.
func TestPlainUploadWaitsOnOnePublishRequest(t *testing.T) {
	log := &workerLog{}
	s := startStack(t, withKeyGate(20), withDenylist(), withPods(),
		withTransport(func(rt http.RoundTripper) http.RoundTripper { log.next = rt; return log }))
	// The key's first session notes its subnet; the case pinned is a key the
	// gate already knows.
	if _, stderr, code := s.run("", []byte("# warm\n")); code != 0 {
		t.Fatalf("first upload: exit %d: %s", code, stderr)
	}
	log.take()

	sess, err := s.keyed.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	var stdout bytes.Buffer
	sess.Stdout = &stdout
	sess.Stdin = strings.NewReader("# t\n")
	if err := sess.Run(""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	s.upload.WaitFinalize()
	if extractSlug(stdout.String()) == "" {
		t.Fatalf("no URL: %q", stdout.String())
	}

	var waited, finalize []string
	for _, req := range log.take() {
		if isFinalize(req) {
			finalize = append(finalize, req)
		} else {
			waited = append(waited, req)
		}
	}
	if want := []string{"POST /subnet/admit", "POST /publish/paste"}; !slices.Equal(waited, want) {
		t.Fatalf("requests the client waited on = %q, want %q", waited, want)
	}
	if len(finalize) != 2 || !strings.HasPrefix(finalize[0], "PUT /blob/") || finalize[1] != "POST /paste/status" {
		t.Fatalf("finalize requests = %q, want the blob write then the status flip", finalize)
	}
}
