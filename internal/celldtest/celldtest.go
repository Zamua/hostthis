// Package celldtest runs the celld Worker locally (workerd via Miniflare, from
// celld/localrt) for tests. One runtime per test binary; each Endpoint is its
// own cell namespace, so tests share the process but never the state.
package celldtest

import (
	"bufio"
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

var (
	once    sync.Once
	baseURL string
	bootErr error
	seq     atomic.Int64
	mu      sync.Mutex
	byTest  = map[testing.TB]string{}
	// stdin holds the runtime's stdin open; the runtime exits when it closes,
	// so a killed test binary leaves no orphan.
	stdin io.Closer
)

func localrtDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "celld", "localrt")
}

func start() {
	dir := localrtDir()
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "miniflare")); err != nil {
		bootErr = fmt.Errorf("celldtest: run `npm ci` in %s first: %w", dir, err)
		return
	}
	cmd := exec.Command("node", "serve.mjs")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		bootErr = err
		return
	}
	stdin = in
	out, err := cmd.StdoutPipe()
	if err != nil {
		bootErr = err
		return
	}
	if err := cmd.Start(); err != nil {
		bootErr = fmt.Errorf("celldtest: start node: %w", err)
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
	case baseURL = <-ready:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		bootErr = fmt.Errorf("celldtest: runtime not ready after 30s")
	}
}

// Endpoint is t's cell namespace on the shared local runtime, in the shape the
// celld adapters and CelldBlobStore take as their base URL. It is empty on the
// first call and the same on every later call in t, so every adapter a test
// builds shares one store, as pastes and sites share one slug space in
// production.
func Endpoint(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if bootErr != nil {
		t.Fatal(bootErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if ep, ok := byTest[t]; ok {
		return ep
	}
	ep := fmt.Sprintf("%s/ns/t%d-%d", baseURL, os.Getpid(), seq.Add(1))
	byTest[t] = ep
	t.Cleanup(func() {
		mu.Lock()
		delete(byTest, t)
		mu.Unlock()
	})
	return ep
}

// Stop ends the runtime early; it otherwise exits with the test binary.
func Stop() {
	if stdin != nil {
		_ = stdin.Close()
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
