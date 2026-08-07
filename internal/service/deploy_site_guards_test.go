package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// The safe-untar guards at the SERVICE boundary, driven against a real
// metadata repo + a real compressed blob store: a tripped guard must leave NOTHING durable (no
// site row, no active bytes charged). The happy path and the error-surface
// mapping live in deploy_site_test.go.

// hugeGzipTar builds a gzip-tar whose single entry has a body of n bytes. The
// body is a long run of one byte, so the archive is tiny on the wire while the
// uncompressed expansion is n: a decompression bomb.
func hugeGzipTar(t *testing.T, name string, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(n), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("hdr: %v", err)
	}
	chunk := bytes.Repeat([]byte("A"), 64<<10)
	for written := 0; written < n; {
		w := len(chunk)
		if written+w > n {
			w = n - written
		}
		if _, err := tw.Write(chunk[:w]); err != nil {
			t.Fatalf("write: %v", err)
		}
		written += w
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// TestGuard_DeployBombStoresNothing: the decompression-bomb guard aborts
// mid-untar and persists nothing (SPEC: "The aborted upload writes nothing
// durable"). The archive inflates to 64 MiB against a 10 MiB per-identity cap.
func TestGuard_DeployBombStoresNothing(t *testing.T) {
	withSmallQuota(t, 10<<20)
	d, sites, _ := deployFixture(t)
	owner := "key:bomber"

	arc := hugeGzipTar(t, "index.html", 64<<20) // 64 MiB uncompressed
	_, err := d.Deploy(bytes.NewReader(arc), owner)
	if !errors.Is(err, ErrOverQuota) {
		t.Fatalf("bomb deploy: got %v, want ErrOverQuota", err)
	}

	// The db is fresh, so any listed site means a row was persisted.
	rows, err := sites.ListSitesByOwner(owner, d.Now().UTC())
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("bomb left site rows behind: %v", rows)
	}

	// Zero bytes charged: the abort never crossed the persistence boundary, so
	// a follow-up deploy has the whole budget available.
	used, err := sites.SumActiveBytesByOwner(owner, d.Now().UTC())
	if err != nil {
		t.Fatalf("sum active: %v", err)
	}
	if used != 0 {
		t.Fatalf("bomb charged %d active bytes, want 0", used)
	}

	// The abort is non-destructive: a small site from the same identity still
	// deploys cleanly afterward.
	good := gzipTar(t, map[string]string{"index.html": "<h1>recovered</h1>"})
	if _, err := d.Deploy(bytes.NewReader(good), owner); err != nil {
		t.Fatalf("post-bomb legitimate deploy failed: %v", err)
	}
}

// TestGuard_DeployTraversalStoresNothing: the traversal guard is
// all-or-nothing. One unsafe entry voids the whole deploy and persists no site
// row, even though the archive also carries a valid index.html.
func TestGuard_DeployTraversalStoresNothing(t *testing.T) {
	d, sites, _ := deployFixture(t)
	arc := gzipTar(t, map[string]string{
		"index.html":       "<h1>ok</h1>",
		"../../etc/passwd": "root:x:0:0",
		"assets/app.css":   "body{}",
	})
	_, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Fatalf("traversal deploy: got %v, want ErrUnsafeArchive", err)
	}
	rows, err := sites.ListSitesByOwner("key:test", d.Now().UTC())
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("traversal left site rows behind: %v", rows)
	}
}

// TestGuard_DeployManifestSizeCapStoresNothing: the manifest path-text cap
// rejects with ErrTooManyFiles and persists no site row, through the full
// service path (the domain test pins the cap in isolation).
func TestGuard_DeployManifestSizeCapStoresNothing(t *testing.T) {
	d, sites, _ := deployFixture(t)

	// ~900-byte paths so the path-text total crosses MaxManifestBytes (1 MiB)
	// well before MaxSiteFiles (5000), isolating the cap under test.
	files := map[string]string{"index.html": "<h1>ok</h1>"}
	stem := bytes.Repeat([]byte("a"), 900-len("dir000000.html"))
	for i := range 2000 {
		name := "dir" + pad6svc(i) + string(stem) + ".html"
		files[name] = "x"
	}
	arc := gzipTar(t, files)

	_, err := d.Deploy(bytes.NewReader(arc), "key:test")
	if !errors.Is(err, domain.ErrTooManyFiles) {
		t.Fatalf("manifest cap deploy: got %v, want ErrTooManyFiles", err)
	}
	rows, err := sites.ListSitesByOwner("key:test", d.Now().UTC())
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("manifest-cap deploy left site rows behind: %v", rows)
	}
}

func pad6svc(n int) string {
	s := []byte{'0', '0', '0', '0', '0', '0'}
	i := len(s) - 1
	for n > 0 && i >= 0 {
		s[i] = byte('0' + n%10)
		n /= 10
		i--
	}
	return string(s)
}

// withSmallQuota shrinks the per-identity quota for one test and restores it.
// Duplicated from the external test package, which package service cannot
// import. Shrinking the cap beats generating 100+ MiB of high-entropy data.
func withSmallQuota(t *testing.T, n int) {
	t.Helper()
	orig := domain.UserQuotaBytes
	domain.UserQuotaBytes = n
	t.Cleanup(func() { domain.UserQuotaBytes = orig })
}
