package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// A stored document, created or updated, carries its input's byte count as
// its decoded size.
func TestUpload_StoredDocumentRecordsDecodedSize(t *testing.T) {
	u, m, repo := newStack(t)
	body := strings.Repeat("# notes\n\nsome body text\n", 50)
	res, err := u.Create(strings.NewReader(body), "key:decoded", "", "")
	if err != nil {
		t.Fatal(err)
	}
	u.WaitFinalize()
	assertRootDecodedSize(t, repo.Get, res.Paste.Slug, len(body))

	updated := "<!doctype html><p>v2</p>"
	if _, err := m.Update(res.Paste.Slug, "key:decoded", strings.NewReader(updated), ""); err != nil {
		t.Fatal(err)
	}
	assertRootDecodedSize(t, repo.Get, res.Paste.Slug, len(updated))
}

func assertRootDecodedSize(t *testing.T, get func(domain.Slug) (domain.Paste, error), slug domain.Slug, want int) {
	t.Helper()
	p, err := get(slug)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RootEntry().DecodedSize; got != want {
		t.Fatalf("stored DecodedSize %d, want %d", got, want)
	}
}

// Every stored site and knowledge base file carries the bytes read out of its
// archive entry as its decoded size.
func TestDeploySite_StoredFilesRecordDecodedSize(t *testing.T) {
	d, sites, _ := deployFixture(t)
	for _, files := range []map[string]string{
		{"index.html": "<h1>site</h1>", "app.js": strings.Repeat("console.log(1);\n", 300)},
		{"README.md": "# kb\n", "guide/intro.md": strings.Repeat("text ", 400)},
	} {
		res, err := d.Deploy(bytes.NewReader(gzipTar(t, files)), "key:decoded-site")
		if err != nil {
			t.Fatal(err)
		}
		got, err := sites.Get(res.Site.Slug)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if n := got.Manifest.Files[name].DecodedSize; n != len(body) {
				t.Errorf("%s (%s): stored DecodedSize %d, want %d", name, got.Kind, n, len(body))
			}
		}
	}
}
