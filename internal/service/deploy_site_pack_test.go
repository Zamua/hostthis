package service

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// Small files share one pack object, a large file stands alone, and every
// entry reads back its own bytes.
func TestDeploySite_PacksSmallFiles(t *testing.T) {
	d, _, blobs, root := deployStack(t)
	files := map[string]string{"index.html": "<h1>packed</h1>"}
	for i := range 40 {
		files[fmt.Sprintf("assets/%02d.js", i)] = fmt.Sprintf("console.log(%d)", i)
	}
	large := make([]byte, domain.PackMemberMaxBytes+1)
	_, _ = rand.Read(large)
	files["model.bin"] = string(large)

	res, err := d.Deploy(bytes.NewReader(gzipTar(t, files)), "key:pack")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if n := objectsUnder(t, root); n != 2 {
		t.Fatalf("deploy wrote %d objects, want 2 (one pack, one large file)", n)
	}
	man := res.Site.Manifest.Files
	packKey := man["index.html"].Key
	validators := map[string]string{}
	for p, body := range files {
		e := man[p]
		if p == "model.bin" {
			if e.Length != 0 || e.Key == packKey {
				t.Fatalf("large file entry %+v is packed", e)
			}
		} else {
			if e.Key != packKey || e.Length <= 0 || e.CompressedSize != int(e.Length) {
				t.Fatalf("%s entry %+v is not a member of pack %s", p, e, packKey)
			}
			if other, dup := validators[e.Validator()]; dup {
				t.Fatalf("%s and %s share validator %q", p, other, e.Validator())
			}
			validators[e.Validator()] = p
		}
		got, err := readEntry(t, blobs, e)
		if err != nil || string(got) != body {
			t.Fatalf("read %s = %d bytes, %v; want its %d bytes", p, len(got), err, len(body))
		}
	}
}

// A pack is written once it reaches the target, so incompressible small files
// beyond it span more than one pack.
func TestDeploySite_PackSplitsAtTarget(t *testing.T) {
	d, _, blobs, root := deployStack(t)
	files := map[string]string{}
	for i := range 10 {
		b := make([]byte, domain.PackMemberMaxBytes)
		_, _ = rand.Read(b)
		files[fmt.Sprintf("f%d.bin", i)] = string(b)
	}
	res, err := d.Deploy(bytes.NewReader(gzipTar(t, files)), "key:split")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if n := objectsUnder(t, root); n != 2 {
		t.Fatalf("deploy wrote %d objects, want 2 packs", n)
	}
	for p, body := range files {
		got, err := readEntry(t, blobs, res.Site.Manifest.Files[p])
		if err != nil || string(got) != body {
			t.Fatalf("read %s: %v", p, err)
		}
	}
}

// A pack that fails to write fails the deploy and leaves no site behind.
func TestDeploySite_PackWriteFailureFailsDeploy(t *testing.T) {
	raw := newFakeBlobs()
	raw.failPut = true
	sites := storage.NewSites(newRepo(t))
	d := NewDeploySite(sites, newRepo(t), storage.NewCompressedBlobStore(raw))
	d.Now = func() time.Time { return fixedNow }
	arc := gzipTar(t, map[string]string{"index.html": "<h1>never</h1>"})
	if _, err := d.Deploy(bytes.NewReader(arc), "key:packfail"); err == nil {
		t.Fatal("deploy with a failing pack write succeeded")
	}
	noSites(t, d, sites, "key:packfail")
	if len(raw.deleted) == 0 {
		t.Fatal("failed deploy did not delete its upload")
	}
}
