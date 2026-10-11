package parity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// A 1x1 PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// documents are single-file pastes, one per kind, each with the --type that
// forces it ("" sniffs).
var documents = []struct {
	label, args, body string
}{
	{"markdown", `--type md --name release notes 2026`, "# Parity\n\nSome *markdown* with <b>inline html</b> and [a link](https://example.com).\n"},
	{"markdown sniffed", ``, "# Sniffed\n\n- one\n- two\n"},
	{"diff", `--type diff`, "--- a/x.txt\n+++ b/x.txt\n@@ -1,2 +1,2 @@\n line\n-old\n+new\n"},
	{"mermaid", `--type mermaid`, "graph TD\n  A-->B\n"},
	{"pdf", `--type pdf`, "%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n"},
	{"csv", `--type csv --name données été`, "a,b\n1,2\n3,4\n"},
	{"json", `--type json`, `{"a":[1,2,{"b":null}],"c":"é"}` + "\n"},
	{"flamegraph", `--type flamegraph`, "main;encode 30\nmain;flush 10\n"},
	{"text", `--type txt --name 日本語 のメモ`, "plain text, with unicode: é日\n"},
	{"log", `--type log`, `{"level":"info","msg":"hello"}` + "\n" + `{"level":"warn","msg":"careful"}` + "\n"},
	{"html", `--type html`, "<!doctype html><title>parity</title><h1>hello</h1>\n"},
}

// knowledgeBase is a directory with no root index: nested markdown, HTML,
// images, a files.json that is only a file, and names with spaces and
// non-ASCII.
var knowledgeBase = map[string][]byte{
	"README.md":                 []byte("# Knowledge base\n\nStart with [the intro](guide/intro.md).\n"),
	"guide/intro.md":            []byte("# Intro\n\n## Section\n\nBody.\n"),
	"guide/deep/nested.md":      []byte("# Nested\n"),
	"notes with spaces.md":      []byte("# Spaces\n"),
	"café/résumé.md":            []byte("# Résumé\n"),
	"日本語/ノート.md":                []byte("# ノート\n"),
	"page.html":                 []byte("<!doctype html><h1>linked out</h1>\n"),
	"img/logo.png":              pngBytes,
	"img/photo one.png":         pngBytes,
	"files.json":                []byte(`{"not":"the file list"}` + "\n"),
	"_files/listing.txt":        []byte("a file under _files\n"),
	"data/table.csv":            []byte("x,y\n1,2\n"),
	"guide/deep/deeper/last.md": []byte("# Last\n"),
}

// buildCorpus uploads every case the reads compare.
func (w *world) buildCorpus(t *testing.T) {
	t.Helper()
	for _, d := range documents {
		w.slugs = append(w.slugs, slugCase{slug: w.upload(t, d.args, []byte(d.body)), label: d.label})
	}
	// Several decoder steps' worth of bytes.
	big := bytes.Repeat([]byte("a line of text that repeats, with a counter-free body\n"), 20000)
	w.slugs = append(w.slugs, slugCase{slug: w.upload(t, "--type txt", big), label: "large text"})

	updated := w.upload(t, "--type html", []byte("<h1>v1</h1>\n"))
	w.ssh.run(t, updated, []byte("<h1>v2</h1>\n"))
	pinned := w.upload(t, "--type md", []byte("# v1\n"))
	w.ssh.run(t, pinned, []byte("# v2\n"))
	w.ssh.run(t, "pin "+pinned+" 1", nil)
	unpinned := w.upload(t, "--type md", []byte("# v1\n"))
	w.ssh.run(t, unpinned, []byte("# v2\n"))
	w.ssh.run(t, "pin "+unpinned+" 1", nil)
	w.ssh.run(t, "unpin "+unpinned, nil)
	deleted := w.upload(t, "--type md", []byte("# gone\n"))
	w.ssh.run(t, "delete "+deleted, nil)
	for _, s := range []string{updated, pinned, unpinned} {
		w.waitReady(t, s)
	}
	w.slugs = append(w.slugs,
		slugCase{slug: updated, label: "updated"},
		slugCase{slug: pinned, label: "pinned to v1"},
		slugCase{slug: unpinned, label: "unpinned"},
		slugCase{slug: deleted, label: "deleted"},
	)

	damaged := w.upload(t, "--type md", []byte("# soon damaged\n"))
	w.cell.damageObject(t, damaged)
	w.slugs = append(w.slugs,
		slugCase{slug: w.cell.pendingPaste(t, domain.KindMarkdown).Slug.String(), label: "pending"},
		slugCase{slug: w.cell.failedPaste(t, domain.KindHTML).Slug.String(), label: "failed"},
		slugCase{slug: damaged, label: "damaged object"},
		slugCase{slug: w.cell.damagedManifest(t), label: "damaged manifest"},
		slugCase{slug: domain.NewRandomSlug().String(), label: "never uploaded"},
	)

	for _, demo := range []string{"react", "vue", "svelte", "plain-static"} {
		files := readTree(t, filepath.Join(repoRoot(), "testdata", "sitefixtures", demo, "dist"))
		targets := []string{
			"/about", "/deep/client/route", "/route.html", "/assets/missing.js", "/missing.css",
			"/assets", "/assets/", "/css", "/css/", "/%69ndex.html", "/index.html/",
		}
		for _, name := range sortedKeys(files) {
			targets = append(targets, escapeTarget(name), escapeTarget(name)+"?raw=1")
			if dir, base, ok := strings.Cut(name, "/"); ok {
				targets = append(targets, "/"+dir+"%2F"+url.PathEscape(base), "/"+dir+"//"+base)
			}
		}
		w.slugs = append(w.slugs, slugCase{slug: w.upload(t, "", tarGz(t, files)), label: "site " + demo, targets: targets})
	}

	targets := []string{
		"/?files=1", "/guide/intro.md?files=1", "/nope?files", "/guide", "/guide/", "/guide/deep",
		"/guide/missing.md", "/missing.png", "/a/client/route", "/img/", "/_files/", "/files.json?files=1",
	}
	for _, name := range sortedKeys(knowledgeBase) {
		targets = append(targets, escapeTarget(name), escapeTarget(name)+"?raw=1", escapeTarget(name)+"?raw")
	}
	w.slugs = append(w.slugs, slugCase{slug: w.upload(t, "", tarGz(t, knowledgeBase)), label: "knowledge base", targets: targets})
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// escapeTarget is name as a browser requests it: each segment
// percent-encoded.
func escapeTarget(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
