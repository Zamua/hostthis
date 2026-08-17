//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// Flow is one filmstrip: NN-label.png steps under $E2E_ARTIFACTS/<name>/ plus
// the meta.json that binds them to a result in the report. See
// docs/E2E-REPORTS.md for the contract.
type Flow struct {
	t    *testing.T
	br   *Browser
	dir  string
	step int
}

var (
	flowsMu sync.Mutex
	flows   = map[string]bool{}
)

// NewFlow creates the flow directory and writes its meta.json. Two flows may
// not share a name: they would interleave step numbers in one directory and
// overwrite each other's screenshots.
func NewFlow(t *testing.T, br *Browser, name string) *Flow {
	t.Helper()

	flowsMu.Lock()
	duplicate := flows[name]
	flows[name] = true
	flowsMu.Unlock()
	if duplicate {
		t.Fatalf("flow %q already declared in this run", name)
	}

	dir := filepath.Join(artifactsRoot(t), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create flow dir: %v", err)
	}

	// The report matches nodeid against the name JUnit reports, which is the
	// test FUNCTION. A subtest's "Parent/child" would match no result.
	nodeid, _, _ := strings.Cut(t.Name(), "/")
	meta, err := json.Marshal(struct {
		Flow   string `json:"flow"`
		NodeID string `json:"nodeid"`
	}{Flow: name, NodeID: nodeid})
	if err != nil {
		t.Fatalf("marshal meta.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}

	return &Flow{t: t, br: br, dir: dir}
}

// Shot records the page as evidence for a human, never as an assertion: font
// rasterization differs between a runner and a laptop, so a pixel comparison
// would flake until the suite got ignored.
func (f *Flow) Shot(label string) {
	f.t.Helper()
	f.step++
	var png []byte
	// Quality 100 is what selects PNG over JPEG, and the report expects .png.
	if err := chromedp.Run(f.br.Ctx, chromedp.FullScreenshot(&png, 100)); err != nil {
		f.t.Fatalf("screenshot %q: %v", label, err)
	}
	name := fmt.Sprintf("%02d-%s.png", f.step, label)
	if err := os.WriteFile(filepath.Join(f.dir, name), png, 0o644); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

// artifactsRoot is $E2E_ARTIFACTS, or <repo>/artifacts locally so a run from
// any directory lands somewhere predictable.
func artifactsRoot(t *testing.T) string {
	// Resolved to absolute: a test process runs with the package directory as
	// its cwd, so a relative value would write flows to e2e/<dir> while the make
	// target and the report renderer both read <repo>/<dir>, losing every
	// screenshot silently.
	if dir := os.Getenv("E2E_ARTIFACTS"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("artifacts root %q: %v", dir, err)
		}
		return abs
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("artifacts root: %v", err)
	}
	return filepath.Join(root, "artifacts")
}
