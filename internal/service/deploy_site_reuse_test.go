package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// reuseStack is a site deployer plus the manage service over the same cells
// and blobs, so a test can delete versions of what it deployed.
func reuseStack(t *testing.T) (*DeploySite, *Manage, *storage.CompressedBlobStore) {
	t.Helper()
	d, _, blobs, _ := deployStack(t)
	m := NewManage(newRepo(t), blobs)
	m.Now = func() time.Time { return fixedNow }
	return d, m, blobs
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func headOf(t *testing.T, d *DeploySite, slug domain.Slug, owner string) (int, domain.Manifest) {
	t.Helper()
	ver, man, err := d.Sites.Head(slug, domain.Identity(owner))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return ver, man
}

func servesBytes(t *testing.T, blobs *storage.CompressedBlobStore, man domain.Manifest, files map[string]string) {
	t.Helper()
	for p, want := range files {
		got, err := readEntry(t, blobs, man.Files[p])
		if err != nil || string(got) != want {
			t.Fatalf("%s: read %d bytes, %v; want %d bytes", p, len(got), err, len(want))
		}
	}
}

// docs/SPEC.md "Redeploy reuse": an unchanged standalone file points at the
// base's object and is not charged again; a changed or packed one is stored.
func TestDeployToSlug_ReusesUnchangedStandaloneFiles(t *testing.T) {
	d, _, blobs := reuseStack(t)
	owner := "key:reuse"
	audio := incompressible(t, 2<<20)
	v1 := map[string]string{"index.html": "<h1>one</h1>", "a.m4a": audio}
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, v1)), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := r1.Site.Slug
	_, head1 := headOf(t, d, slug, owner)
	charge1 := ownerCharge(t, owner)

	v2 := map[string]string{"index.html": "<h1>two</h1>", "a.m4a": audio}
	if _, err := d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, v2)), owner); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	ver, head2 := headOf(t, d, slug, owner)
	if ver != 2 || head2.Files["a.m4a"].Key != head1.Files["a.m4a"].Key {
		t.Fatalf("v%d a.m4a key %q, want v2 sharing %q", ver, head2.Files["a.m4a"].Key, head1.Files["a.m4a"].Key)
	}
	if head2.Files["a.m4a"].Digest != sha(audio) {
		t.Fatalf("digest %q, want %q", head2.Files["a.m4a"].Digest, sha(audio))
	}
	if grew := ownerCharge(t, owner) - charge1; grew <= 0 || grew >= int64(len(audio)) {
		t.Fatalf("redeploy charged %d more, want only the changed small file", grew)
	}
	servesBytes(t, blobs, head2, v2)
}

// A keep list carries base files the archive does not send: a standalone file
// by reference, a packed one copied into the new deploy's pack.
func TestDeployToSlug_KeepListCarriesUnsentFiles(t *testing.T) {
	d, _, blobs := reuseStack(t)
	owner := "key:keep"
	audio := incompressible(t, 2<<20)
	v1 := map[string]string{"index.html": "<h1>one</h1>", "a.m4a": audio, "app.js": "console.log(1)"}
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, v1)), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := r1.Site.Slug
	_, head1 := headOf(t, d, slug, owner)

	keep := fmt.Sprintf("base 1\n%s  a.m4a\n%s  ./app.js\n", sha(audio), sha("console.log(1)"))
	arc := gzipTar(t, map[string]string{"dist/index.html": "<h1>two</h1>", "dist/.hostthis-keep": keep})
	if _, err := d.DeployToSlug(slug, bytes.NewReader(arc), owner); err != nil {
		t.Fatalf("keep deploy: %v", err)
	}
	_, head2 := headOf(t, d, slug, owner)
	if len(head2.Files) != 3 {
		t.Fatalf("files %v, want index.html, a.m4a, app.js", head2.Files)
	}
	if head2.Files["a.m4a"].Key != head1.Files["a.m4a"].Key {
		t.Fatalf("kept standalone file not shared")
	}
	if js := head2.Files["app.js"]; js.Key == head1.Files["app.js"].Key || js.Length == 0 {
		t.Fatalf("kept packed file %+v, want a copy in the new pack", js)
	}
	servesBytes(t, blobs, head2, map[string]string{"index.html": "<h1>two</h1>", "a.m4a": audio, "app.js": "console.log(1)"})
}

// A keep list that cannot be honoured exactly refuses the whole deploy and
// leaves the head served.
func TestDeployToSlug_KeepListRefusals(t *testing.T) {
	d, _, _ := reuseStack(t)
	owner := "key:refuse"
	v1 := map[string]string{"index.html": "<h1>one</h1>", "a.js": "a"}
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, v1)), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := r1.Site.Slug
	for name, c := range map[string]struct {
		keep string
		sent map[string]string
		want error
	}{
		"stale base":   {"base 7\n" + sha("a") + "  a.js\n", nil, domain.ErrBaseMoved},
		"wrong digest": {"base 1\n" + sha("b") + "  a.js\n", nil, domain.ErrBadKeepList},
		"absent path":  {"base 1\n" + sha("a") + "  b.js\n", nil, domain.ErrBadKeepList},
		"also sent":    {"base 1\n" + sha("a") + "  a.js\n", map[string]string{"a.js": "a"}, domain.ErrBadKeepList},
		"escaped line": {"base 1\n\\" + sha("a") + "  a\\\\b.js\n", nil, domain.ErrBadKeepList},
		"no base line": {sha("a") + "  a.js\n", nil, domain.ErrBadKeepList},
		"listed twice": {"base 1\n" + sha("a") + "  a.js\n" + sha("a") + "  a.js\n", nil, domain.ErrBadKeepList},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"index.html": "<h1>x</h1>", ".hostthis-keep": c.keep}
			maps.Copy(files, c.sent)
			_, err := d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, files)), owner)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if ver, _ := headOf(t, d, slug, owner); ver != 1 {
				t.Fatalf("head moved to v%d", ver)
			}
		})
	}
}

// Deleting versions frees only what no live neighbour holds: the shared
// object survives the delete of the version that stored it, and goes with the
// last version holding it.
func TestDeleteVersion_FreesOnlyUnsharedObjects(t *testing.T) {
	d, m, blobs := reuseStack(t)
	owner := "key:free"
	audio := incompressible(t, 2<<20)
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "1", "a.m4a": audio})), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := r1.Site.Slug
	if _, err := d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "2", "a.m4a": audio})), owner); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	_, head := headOf(t, d, slug, owner)

	res, err := m.DeleteVersion(slug, owner, 1)
	if err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	if res.FreedBytes >= len(audio) {
		t.Fatalf("delete v1 freed %d, want only its unshared bytes", res.FreedBytes)
	}
	servesBytes(t, blobs, head, map[string]string{"a.m4a": audio})

	if _, err := d.DeployToSlug(slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "3"})), owner); err != nil {
		t.Fatalf("redeploy without audio: %v", err)
	}
	res, err = m.DeleteVersion(slug, owner, 2)
	if err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	if res.FreedBytes < len(audio) {
		t.Fatalf("delete v2 freed %d, want the audio too", res.FreedBytes)
	}
	if _, err := readEntry(t, blobs, head.Files["a.m4a"]); err == nil {
		t.Fatal("audio still readable after its last version was deleted")
	}
}

// have prints the head as a keep list, sorted, without unkeepable paths.
func TestHave(t *testing.T) {
	d, _, _ := reuseStack(t)
	owner := "key:have"
	r1, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "i", "b/c.js": "c"})), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	held, err := d.Have(r1.Site.Slug, owner)
	if err != nil {
		t.Fatalf("have: %v", err)
	}
	var lines []string
	for _, f := range held.Files {
		lines = append(lines, f.Digest+"  "+f.Path)
	}
	want := []string{sha("c") + "  b/c.js", sha("i") + "  index.html"}
	if held.Base != 1 || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("have = %d %v, want 1 %v", held.Base, lines, want)
	}
	if _, err := d.Have(r1.Site.Slug, "key:someone-else"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign have: %v, want not found", err)
	}
}
