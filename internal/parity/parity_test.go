// Package parity holds the response parity suite: every request goes to
// hostthisd's HTTP surface and to the celld Worker's public surface, and the
// two answers must agree (docs/SPEC.md "The public surface") on status, the
// content headers and the body, but for the differences in allowances_test.go.
//
// By default it runs locally: it builds hostthisd, starts it beside the Worker
// under Miniflare with the public surface on, uploads a corpus over SSH and
// compares the Worker both directly (by Host) and in forwarded mode. With
// PARITY_HOSTTHISD_URL set it runs against a deployed environment instead,
// in forwarded mode only (see external_test.go).
package parity

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParity(t *testing.T) {
	if testing.Short() {
		t.Skip("starts hostthisd and the Worker")
	}
	if cfg, ok := readExternalConfig(t); ok {
		w := externalWorld(t, cfg)
		runParity(t, w)
		t.Logf("compared %d requests", w.compared.Load())
		return
	}
	w := localWorld(t)
	runParity(t, w)
	t.Run("allowances", func(t *testing.T) { w.checkAllowancesUsed(t) })
	t.Logf("compared %d requests", w.compared.Load())
}

// TestParityAgainstURLs runs the deployed-environment mode against the local
// servers, as an operator would against theirs: only the environment
// variables, the public hosts and forwarded mode.
func TestParityAgainstURLs(t *testing.T) {
	if testing.Short() {
		t.Skip("starts hostthisd and the Worker")
	}
	if os.Getenv("PARITY_HOSTTHISD_URL") != "" {
		t.Skip("TestParity runs against the configured environment")
	}
	w := localServers(t)
	var slugs []string
	for _, d := range documents[:2] {
		slugs = append(slugs, w.upload(t, d.args, []byte(d.body)))
	}
	slugs = append(slugs,
		w.upload(t, "", tarGz(t, knowledgeBase)),
		w.upload(t, "", tarGz(t, readTree(t, filepath.Join(repoRoot(), "testdata", "sitefixtures", "react", "dist")))),
	)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(w.workers[1].token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARITY_HOSTTHISD_URL", "http://"+w.apex+":"+portOf(w.hostthisd.addr))
	t.Setenv("PARITY_WORKER_URL", "http://"+w.workers[1].addr)
	t.Setenv("PARITY_TOKEN_FILE", tokenFile)
	t.Setenv("PARITY_SLUGS", strings.Join(slugs, ", "))
	t.Setenv("PARITY_ALLOW_WRITES", "1")
	t.Setenv("PARITY_WRITE_SLUG", w.roomApp(t))
	cfg, ok := readExternalConfig(t)
	if !ok {
		t.Fatal("the environment did not configure a run")
	}
	// A resolver that does not map names under localhost to loopback (RFC
	// 6761) cannot dial the public hosts; the daemon's address stands in.
	if _, err := net.LookupHost(slugs[0] + "." + w.apex); err != nil {
		t.Logf("names under %s do not resolve (%v); dialing hostthisd at %s", w.apex, err, w.hostthisd.addr)
		cfg.pin = w.hostthisd.addr
	}
	ext := externalWorld(t, cfg)
	if len(ext.slugs) != len(slugs) || len(ext.slugs[2].targets) == 0 || len(ext.slugs[3].targets) == 0 {
		t.Fatalf("discovery found nothing in the knowledge base or the site: %+v", ext.slugs)
	}
	runParity(t, ext)
}

func runParity(t *testing.T, w *world) {
	t.Run("reads", func(t *testing.T) { testReads(t, w) })
	t.Run("rooms", func(t *testing.T) { testRooms(t, w) })
	t.Run("websocket", func(t *testing.T) { testWebSocket(t, w) })
}

func portOf(addr string) string {
	return addr[strings.LastIndex(addr, ":")+1:]
}
