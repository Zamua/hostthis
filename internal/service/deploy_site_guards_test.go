package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// This file pins the security guards at the SERVICE boundary: it drives
// DeploySite against real sqlite + a real compressed blob store so a
// guard trip is asserted to leave NOTHING durable (no site row, no active
// bytes charged). deploy_site_test.go already proves the happy path and
// the error-surface mapping; these tests pin the "aborts atomically,
// stores nothing" property the SPEC calls out for safe-untar.

// hugeGzipTar builds a gzip-tar whose single entry has a body of n bytes.
// Because the body is a long run of one byte it compresses tiny, so the
// archive on the wire is small while the uncompressed expansion is n: a
// decompression bomb in the SPEC's sense.
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

// TestGuard_DeployBombStoresNothing proves the decompression-bomb guard
// aborts the deploy AND leaves nothing durable: no site row, and zero
// active site bytes charged against the identity. A half-extracted site
// is never persisted (SPEC: "The aborted upload writes nothing durable").
//
// The archive is tiny on the wire (~few KiB compressed) but would inflate
// to 64 MiB, far past the 10 MiB per-identity cap. The mid-untar guard
// must abort it and the persistence step must never run.
func TestGuard_DeployBombStoresNothing(t *testing.T) {
	d, sites, _ := deployFixture(t)
	owner := "key:bomber"

	arc := hugeGzipTar(t, "index.html", 64<<20) // 64 MiB uncompressed
	_, err := d.Deploy(bytes.NewReader(arc), owner)
	if !errors.Is(err, ErrOverQuota) {
		t.Fatalf("bomb deploy: got %v, want ErrOverQuota", err)
	}

	// No site row was persisted: the union of all site manifests references
	// no SHAs (no site rows exist at all in this fresh db).
	refs, err := sites.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("bomb left site rows behind: %v", refs)
	}

	// And the identity is charged ZERO active site bytes - it never crossed
	// the persistence boundary, so a follow-up legitimate deploy has the
	// whole budget available.
	used, err := sites.SumActiveBytesByOwner(owner, d.Now().UTC())
	if err != nil {
		t.Fatalf("sum active: %v", err)
	}
	if used != 0 {
		t.Fatalf("bomb charged %d active bytes, want 0", used)
	}

	// Proof the abort was non-destructive: a real (small) site from the
	// same identity still deploys cleanly afterward.
	good := gzipTar(t, map[string]string{"index.html": "<h1>recovered</h1>"})
	if _, err := d.Deploy(bytes.NewReader(good), owner); err != nil {
		t.Fatalf("post-bomb legitimate deploy failed: %v", err)
	}
}

// TestGuard_DeployTraversalStoresNothing proves a traversal entry aborts
// the deploy with ErrUnsafeArchive and persists no site row, even though
// the archive also contains a perfectly valid index.html. The guard is
// all-or-nothing: one unsafe entry voids the whole deploy.
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
	refs, err := sites.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("traversal left site rows behind: %v", refs)
	}
}

// TestGuard_DeployManifestSizeCapStoresNothing exercises the manifest
// path-text cap through the full service path and asserts the deploy is
// rejected with ErrTooManyFiles and no site row is persisted. This pins
// the third safe-untar guard at the service boundary (the domain test
// pins it in isolation).
func TestGuard_DeployManifestSizeCapStoresNothing(t *testing.T) {
	d, sites, _ := deployFixture(t)

	// ~900-byte paths so the path-text total crosses MaxManifestBytes
	// (1 MiB) well before MaxSiteFiles (5000).
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
	refs, err := sites.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced site shas: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("manifest-cap deploy left site rows behind: %v", refs)
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
