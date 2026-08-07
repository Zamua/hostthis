package ssh_test

import (
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// TestList_IncludesSites pins that `list` shows deployed sites in both the
// table and -o json. A site counts against the same quota as pastes, so an
// owner who cannot see it can never free what it holds.
func TestList_IncludesSites(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobUnit := service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(rawBlobs))
	repo := storagetest.NewRepo(t)
	sites := storage.NewShaleSiteRepo(storagetest.NewRepo(t))
	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)

	sshListener := mustListen(t)
	sshAddr := sshListener.Addr().String()
	_ = sshListener.Close()
	srv := &hostssh.Server{
		Addr:       sshAddr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Manage:     service.NewManage(repo, blobUnit),
		Deploy:     service.NewDeploySite(sites, repo, blobUnit),
		BuildURL:   func(s domain.Slug) string { return "http://paste.test/p/" + s.String() },
		Logger:     log.New(io.Discard, "", 0),
	}
	go func() { _ = srv.ListenAndServe() }()
	waitForSSH(t, sshAddr)

	client, _ := dialKeyed(t, sshAddr)
	t.Cleanup(func() { _ = client.Close() })

	runSSH(t, client, "", []byte("<!doctype html><p>a text paste</p>"))
	// A gzip-tar body dispatches to the site-deploy path.
	arc := makeSiteArchive(t, map[string]string{
		"index.html":    "<!doctype html><h1>home</h1>",
		"css/style.css": "body{color:green}",
	})
	siteOut := runSSH(t, client, "", arc)
	if !strings.Contains(siteOut.stderr, "site:") {
		t.Fatalf("expected a site deploy, stderr=%q", siteOut.stderr)
	}

	// --- table output ---
	out := runSSH(t, client, "list", nil)
	lines := strings.Split(strings.TrimRight(out.stdout, "\n"), "\n")
	if len(lines) != 3 { // header + paste + site
		t.Fatalf("expected header + 2 rows, got %d:\n%s", len(lines), out.stdout)
	}
	var siteRow string
	for _, ln := range lines[1:] {
		if strings.Contains(ln, "site") {
			siteRow = ln
		}
	}
	if siteRow == "" {
		t.Fatalf("no site row (kind=site) in list output:\n%s", out.stdout)
	}
	// A site is not versioned, so its VERS column is "-" (the last field).
	if f := strings.Fields(siteRow); f[len(f)-1] != "-" {
		t.Fatalf("site VERS column should be '-', got %q in %q", f[len(f)-1], siteRow)
	}

	// --- json output ---
	jsonOut := runSSH(t, client, "list -o json", nil)
	var items []struct {
		Slug          string `json:"slug"`
		Kind          string `json:"kind"`
		SizeBytes     int    `json:"size_bytes"`
		ServedVersion *int   `json:"served_version"`
	}
	if err := json.Unmarshal([]byte(jsonOut.stdout), &items); err != nil {
		t.Fatalf("list -o json not an array: %v\n%q", err, jsonOut.stdout)
	}
	var site, paste int
	for _, it := range items {
		switch it.Kind {
		case "site":
			site++
			// A null version field is the site discriminator.
			if it.ServedVersion != nil {
				t.Fatalf("site served_version should be null, got %v", *it.ServedVersion)
			}
			if it.SizeBytes <= 0 {
				t.Fatalf("site size_bytes should be positive, got %d", it.SizeBytes)
			}
		default:
			paste++
			if it.ServedVersion == nil {
				t.Fatalf("paste %s served_version should be non-null", it.Slug)
			}
		}
	}
	if site != 1 || paste != 1 {
		t.Fatalf("want 1 site + 1 paste in json, got site=%d paste=%d", site, paste)
	}
}

type sshResult struct{ stdout, stderr string }

func runSSH(t *testing.T, client *xssh.Client, cmd string, stdin []byte) sshResult {
	t.Helper()
	stdout, stderr, _ := runCmd(t, client, cmd, stdin)
	return sshResult{stdout: stdout, stderr: stderr}
}
