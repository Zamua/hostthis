// Package celldtest runs the celld Worker locally (workerd via Miniflare, from
// celld/localrt) for tests. One runtime per test binary; each Endpoint is its
// own cell namespace, so tests share the process but never the state.
package celldtest

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// localRuntime is one Worker process, started on first use.
type localRuntime struct {
	token   string
	once    sync.Once
	baseURL string
	bootErr error
	mu      sync.Mutex
	byTest  map[testing.TB]string
	// stdin holds the runtime's stdin open; the runtime exits when it closes,
	// so a killed test binary leaves no orphan.
	stdin io.Closer
}

var (
	seq     atomic.Int64
	plain   = &localRuntime{byTest: map[testing.TB]string{}}
	tokened = &localRuntime{token: newToken(), byTest: map[testing.TB]string{}}
)

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func localrtDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "celld", "localrt")
}

func (r *localRuntime) start() {
	dir := localrtDir()
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "miniflare")); err != nil {
		r.bootErr = fmt.Errorf("celldtest: run `npm ci` in %s first: %w", dir, err)
		return
	}
	cmd := exec.Command("node", "serve.mjs")
	cmd.Dir = dir
	// Set even when empty, so an inherited token never gates the open runtime.
	cmd.Env = append(os.Environ(), "HOSTTHIS_CELLD_TOKEN="+r.token)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		r.bootErr = err
		return
	}
	r.stdin = in
	out, err := cmd.StdoutPipe()
	if err != nil {
		r.bootErr = err
		return
	}
	if err := cmd.Start(); err != nil {
		r.bootErr = fmt.Errorf("celldtest: start node: %w", err)
		return
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "LISTENING "); ok {
				ready <- u
			}
		}
	}()
	select {
	case r.baseURL = <-ready:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		r.bootErr = fmt.Errorf("celldtest: runtime not ready after 30s")
	}
}

func (r *localRuntime) endpoint(t testing.TB) string {
	t.Helper()
	r.once.Do(r.start)
	if r.bootErr != nil {
		t.Fatal(r.bootErr)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep, ok := r.byTest[t]; ok {
		return ep
	}
	ep := fmt.Sprintf("%s/ns/t%d-%d", r.baseURL, os.Getpid(), seq.Add(1))
	r.byTest[t] = ep
	t.Cleanup(func() {
		r.mu.Lock()
		delete(r.byTest, t)
		r.mu.Unlock()
	})
	return ep
}

// Endpoint is t's cell namespace on the shared local runtime, in the shape the
// celld adapters and CelldBlobStore take as their base URL. It is empty on the
// first call and the same on every later call in t, so every adapter a test
// builds shares one store, as pastes and sites share one slug space in
// production.
func Endpoint(t testing.TB) string {
	t.Helper()
	return plain.endpoint(t)
}

// TokenEndpoint is Endpoint on a second local runtime that requires a bearer
// token on every route but /healthz, and that token.
func TokenEndpoint(t testing.TB) (endpoint, token string) {
	t.Helper()
	return tokened.endpoint(t), tokened.token
}

// Stop ends the runtimes early; they otherwise exit with the test binary.
func Stop() {
	for _, r := range []*localRuntime{plain, tokened} {
		if r.stdin != nil {
			_ = r.stdin.Close()
		}
	}
}

// Target is CELLD_TEST_ENDPOINT when it names a real node, else Endpoint. A
// real node is shared and durable, so a caller must namespace its own names.
func Target(t testing.TB) string {
	t.Helper()
	if base := os.Getenv("CELLD_TEST_ENDPOINT"); base != "" {
		return base
	}
	return Endpoint(t)
}
