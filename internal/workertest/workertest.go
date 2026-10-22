// Package workertest runs the Worker locally (workerd via Miniflare, from
// worker/localrt) for tests. One runtime per test binary; each Endpoint is its
// own cell namespace, so tests share the process but never the state.
package workertest

import (
	"bufio"
	"bytes"
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
	return filepath.Join(filepath.Dir(file), "..", "..", "worker", "localrt")
}

func (r *localRuntime) start() {
	// Set even when empty, so an inherited token never gates the open runtime.
	r.baseURL, r.stdin, _, r.bootErr = launch(append(runtimeEnv(), "HOSTTHIS_WORKER_TOKEN="+r.token), os.Stderr)
}

// runtimeEnv is this process's environment without the variables that turn
// the runtime's public surface on, so only StartPublic ever does.
func runtimeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOSTTHIS_LOCALRT_") {
			env = append(env, kv)
		}
	}
	return env
}

// launch starts one runtime with env and waits for its listener. Closing stdin
// stops it; done closes once it has exited.
func launch(env []string, stderr io.Writer) (baseURL string, stdin io.Closer, done <-chan struct{}, err error) {
	dir := localrtDir()
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "miniflare")); err != nil {
		return "", nil, nil, fmt.Errorf("workertest: run `npm ci` in %s first: %w", dir, err)
	}
	cmd := exec.Command("node", "serve.mjs")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, nil, fmt.Errorf("workertest: start node: %w", err)
	}
	ready := make(chan string, 1)
	exited := make(chan struct{})
	go func() {
		defer close(ready)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "LISTENING "); ok {
				ready <- u
			}
		}
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case u, ok := <-ready:
		if !ok {
			return "", nil, nil, fmt.Errorf("workertest: runtime exited before serving; its stderr says why (a missing or stale bundle needs `make bundle`)")
		}
		return u, in, exited, nil
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return "", nil, nil, fmt.Errorf("workertest: runtime not ready after 30s")
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
// worker adapters and WorkerBlobStore take as their base URL. It is empty on the
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

// Target is WORKER_TEST_ENDPOINT when it names a real node, else Endpoint. A
// real node is shared and durable, so a caller must namespace its own names.
func Target(t testing.TB) string {
	t.Helper()
	if base := os.Getenv("WORKER_TEST_ENDPOINT"); base != "" {
		return base
	}
	return Endpoint(t)
}

// Public configures StartPublic's runtime: each field sets the Worker
// variable named beside it (docs/SPEC.md "Public and internal hosts").
type Public struct {
	Apex           string // HOSTTHIS_APEX_DOMAIN, required
	URLMode        string // HOSTTHIS_URL_MODE
	Scheme         string // HOSTTHIS_PUBLIC_SCHEME
	ClientIPSource string // HOSTTHIS_CLIENT_IP_SOURCE
	SolidKey       string // HOSTTHIS_SOLID_KEY
	SolidOrigin    string // HOSTTHIS_SOLID_ORIGIN
}

// PublicRuntime is a Worker with its public surface on, as a Cloudflare deploy
// runs it: static assets beside it, and a token on the internal routes.
type PublicRuntime struct {
	// URL is the listener, http://127.0.0.1:<port>. Addressed by that host it is
	// the internal endpoint, which takes Token; addressed by a host under the
	// apex it is the public surface.
	URL   string
	Token string
}

// StartPublic starts a runtime of t's own with its public surface configured
// by cfg, and stops it when t ends. Its cells are not namespaced: nothing it
// holds is shared with another test. Its log reaches t only if t fails.
func StartPublic(t testing.TB, cfg Public) *PublicRuntime {
	t.Helper()
	if cfg.Apex == "" {
		t.Fatal("workertest: StartPublic needs an apex")
	}
	token := newToken()
	env := append(runtimeEnv(),
		"HOSTTHIS_WORKER_TOKEN="+token,
		"HOSTTHIS_LOCALRT_APEX_DOMAIN="+cfg.Apex,
		"HOSTTHIS_LOCALRT_URL_MODE="+cfg.URLMode,
		"HOSTTHIS_LOCALRT_PUBLIC_SCHEME="+cfg.Scheme,
		"HOSTTHIS_LOCALRT_CLIENT_IP_SOURCE="+cfg.ClientIPSource,
		"HOSTTHIS_LOCALRT_SOLID_KEY="+cfg.SolidKey,
		"HOSTTHIS_LOCALRT_SOLID_ORIGIN="+cfg.SolidOrigin,
	)
	logs := &syncBuffer{}
	url, stdin, done, err := launch(env, logs)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("workertest: public runtime still running 10s after its stdin closed")
		}
		if t.Failed() {
			t.Logf("worker runtime log:\n%s", logs)
		}
	})
	return &PublicRuntime{URL: url, Token: token}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
