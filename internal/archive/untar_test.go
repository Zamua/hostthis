package archive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/archive"
	"github.com/Zamua/hostthis/internal/domain"
)

// recordingSink captures every file Untar hands it, computing the SHA the same
// way the real blob sink does (over uncompressed bytes), and the total bytes
// it was handed across all files.
type recordingSink struct {
	files map[string][]byte
	total int64
}

func newRecordingSink() *recordingSink { return &recordingSink{files: map[string][]byte{}} }

func (s *recordingSink) Store(p string, r io.Reader, _ int64) (string, int, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, r)
	s.total += n
	if err != nil {
		// Propagate the cap sentinel unchanged, like the production sink.
		if errors.Is(err, domain.ErrArchiveTooLarge) {
			return "", 0, domain.ErrArchiveTooLarge
		}
		return "", 0, err
	}
	body := buf.Bytes()
	s.files[p] = body
	sum := sha256.Sum256(body)
	// Test double doesn't compress; report the raw length as the "stored" size.
	return hex.EncodeToString(sum[:]), len(body), nil
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// makeGzipTar builds a gzip-tar from entries; typeflag 0 defaults to a regular
// file.
func makeGzipTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Typeflag: tf,
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestSafeUntar_HappyPath(t *testing.T) {
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<!doctype html><h1>hi</h1>"},
		{name: "css/style.css", body: "body{color:red}"},
		{name: "js/app.js", body: "console.log(1)"},
		{name: "sub/", typeflag: tar.TypeDir},
		{name: "sub/page.html", body: "<p>x</p>"},
	})
	sink := newRecordingSink()
	man, err := archive.Untar(bytes.NewReader(arc), sink, int64(domain.UserQuotaBytes))
	if err != nil {
		t.Fatalf("SafeUntar: %v", err)
	}
	// Directory entry must NOT appear in the manifest (files only).
	if _, ok := man.Files["sub"]; ok {
		t.Fatalf("directory leaked into manifest")
	}
	for _, want := range []string{"index.html", "css/style.css", "js/app.js", "sub/page.html"} {
		e, ok := man.Files[want]
		if !ok {
			t.Fatalf("missing %q from manifest", want)
		}
		if e.SHA == "" || e.Size == 0 {
			t.Fatalf("entry %q has empty sha/size: %+v", want, e)
		}
	}
	// Content-type by extension.
	if got := man.Files["css/style.css"].ContentType; got != "text/css; charset=utf-8" {
		t.Fatalf("css content-type: got %q", got)
	}
	if got := man.Files["js/app.js"].ContentType; got != "text/javascript; charset=utf-8" {
		t.Fatalf("js content-type: got %q", got)
	}
	if !man.HasWebContent() {
		t.Fatalf("expected web content")
	}
}

// TestUntar_RejectsUnsafeEntries pins every entry-shape guard: a non-regular
// type or an escaping path is ErrUnsafeArchive, and no unsafe path reaches
// the sink. The valid index makes the rejection unambiguously the guard
// firing rather than an empty archive.
func TestUntar_RejectsUnsafeEntries(t *testing.T) {
	cases := []struct {
		name  string
		entry tarEntry
	}{
		{"symlink", tarEntry{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		{"hardlink", tarEntry{name: "b", typeflag: tar.TypeLink, linkname: "index.html"}},
		{"char device", tarEntry{name: "dev", typeflag: tar.TypeChar}},
		{"block device", tarEntry{name: "dev", typeflag: tar.TypeBlock}},
		{"fifo", tarEntry{name: "dev", typeflag: tar.TypeFifo}},
		{"relative climb", tarEntry{name: "../escape.html", body: "x"}},
		{"double climb", tarEntry{name: "../../etc/passwd", body: "x"}},
		{"climb after descent", tarEntry{name: "a/../../b.html", body: "x"}},
		{"climb after two descents", tarEntry{name: "foo/../../bar.html", body: "x"}},
		{"absolute path", tarEntry{name: "/absolute.html", body: "x"}},
		{"backslash climb", tarEntry{name: `..\windows\evil.html`, body: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			arc := makeGzipTar(t, []tarEntry{{name: "index.html", body: "<h1>ok</h1>"}, c.entry})
			sink := newRecordingSink()
			_, err := archive.Untar(bytes.NewReader(arc), sink, int64(domain.UserQuotaBytes))
			if !errors.Is(err, domain.ErrUnsafeArchive) {
				t.Fatalf("%s: got %v, want domain.ErrUnsafeArchive", c.name, err)
			}
			for p := range sink.files {
				if p != "index.html" {
					t.Fatalf("%s: unsafe entry %q reached the sink", c.name, p)
				}
			}
		})
	}
}

func TestSafeUntar_AllowsDotSlashPrefix(t *testing.T) {
	// "./index.html" is the common `tar czf - .` shape: accepted, and cleaned
	// to "index.html".
	arc := makeGzipTar(t, []tarEntry{{name: "./index.html", body: "<h1>ok</h1>"}})
	man, err := archive.Untar(bytes.NewReader(arc), newRecordingSink(), int64(domain.UserQuotaBytes))
	if err != nil {
		t.Fatalf("SafeUntar: %v", err)
	}
	if _, ok := man.Files["index.html"]; !ok {
		t.Fatalf("expected cleaned path index.html, got %v", keys(man.Files))
	}
}

func TestSafeUntar_RunningTotalAcrossEntries(t *testing.T) {
	// Several files that individually fit but together exceed the budget.
	// The guard tracks a RUNNING total, so the deploy must abort.
	chunk := strings.Repeat("B", 400*1024) // 400 KiB each
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "a.html", body: chunk},
		{name: "b.html", body: chunk},
		{name: "c.html", body: chunk}, // 1.2 MiB cumulative > 1 MiB budget
	})
	_, err := archive.Untar(bytes.NewReader(arc), newRecordingSink(), 1<<20)
	if !errors.Is(err, domain.ErrArchiveTooLarge) {
		t.Fatalf("running total: got %v, want domain.ErrArchiveTooLarge", err)
	}
}

func TestSafeUntar_FileCountCap(t *testing.T) {
	entries := make([]tarEntry, 0, domain.MaxSiteFiles+2)
	entries = append(entries, tarEntry{name: "index.html", body: "x"})
	for i := range domain.MaxSiteFiles + 1 {
		entries = append(entries, tarEntry{name: "f" + strconv.Itoa(i) + ".txt", body: "y"})
	}
	_, err := archive.Untar(bytes.NewReader(makeGzipTar(t, entries)), newRecordingSink(), int64(domain.UserQuotaBytes))
	if !errors.Is(err, domain.ErrTooManyFiles) {
		t.Fatalf("file count: got %v, want domain.ErrTooManyFiles", err)
	}
}

func TestSafeUntar_RejectsNonGzip(t *testing.T) {
	// Plain bytes, no gzip wrapper: rejected as unsupported.
	_, err := archive.Untar(strings.NewReader("not a gzip stream at all"), newRecordingSink(), int64(domain.UserQuotaBytes))
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("non-gzip: got %v, want domain.ErrUnsupportedKind", err)
	}
}

func TestSafeUntar_CorruptTarInGzip(t *testing.T) {
	// Valid gzip wrapping garbage (not a tar).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(bytes.Repeat([]byte{0x42}, 2048))
	_ = gz.Close()
	_, err := archive.Untar(bytes.NewReader(buf.Bytes()), newRecordingSink(), int64(domain.UserQuotaBytes))
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("corrupt tar: got %v, want domain.ErrUnsupportedKind", err)
	}
}

func TestSafeUntar_IdenticalFilesShareASHAButNotAnEntry(t *testing.T) {
	same := "<h1>same bytes</h1>"
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: same},
		{name: "copy.html", body: same},
	})
	man, err := archive.Untar(bytes.NewReader(arc), newRecordingSink(), int64(domain.UserQuotaBytes))
	if err != nil {
		t.Fatalf("SafeUntar: %v", err)
	}
	if man.Files["index.html"].SHA != man.Files["copy.html"].SHA {
		t.Fatalf("identical files should share a SHA")
	}
	// The SHA identifies the content; it does not collapse the two files. Each
	// path is its own manifest entry and its own object on disk, so the size
	// counts both.
	if want := 2 * len(same); man.Size() != want {
		t.Fatalf("size: got %d, want %d (both paths counted)", man.Size(), want)
	}
}

func keys(m map[string]domain.ManifestEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGuard_BombAbortsBeforeFullExpansion pins that the bomb guard checks the
// running UNCOMPRESSED total as bytes stream, not after inflating the whole
// file: an entry 64x the cap must abort having read at most cap+1 bytes.
func TestGuard_BombAbortsBeforeFullExpansion(t *testing.T) {
	const cap = 256 << 10 // 256 KiB budget
	bomb := strings.Repeat("A", 64*cap)
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "bomb.html", body: bomb},
	})

	sink := newRecordingSink()
	_, err := archive.Untar(bytes.NewReader(arc), sink, cap)
	if !errors.Is(err, domain.ErrArchiveTooLarge) {
		t.Fatalf("bomb: got %v, want domain.ErrArchiveTooLarge", err)
	}
	// The +1 is the lookahead probe that detects overflow.
	if sink.total > cap+1 {
		t.Fatalf("bomb read %d bytes, want <= cap+1 (%d): full expansion not aborted mid-stream", sink.total, cap+1)
	}
}

// TestGuard_ManifestPathTextCapRejected pins that total manifest path text is
// bounded by domain.MaxManifestBytes independent of the file count.
func TestGuard_ManifestPathTextCapRejected(t *testing.T) {
	// Each path is ~900 bytes (< domain.MaxSitePathLen 1024), so the path text
	// crosses domain.MaxManifestBytes (1 MiB) at ~1165 entries, well before
	// domain.MaxSiteFiles (5000). 2000 entries is safely over.
	const pathLen = 900
	stem := strings.Repeat("a", pathLen-len(".html")-6) // leave room for index + ext
	entries := make([]tarEntry, 0, 2001)
	entries = append(entries, tarEntry{name: "index.html", body: "<h1>ok</h1>"})
	for i := range 2000 {
		entries = append(entries, tarEntry{name: fmt.Sprintf("dir%06d%s.html", i, stem), body: "x"})
	}
	_, err := archive.Untar(bytes.NewReader(makeGzipTar(t, entries)), newRecordingSink(), int64(domain.UserQuotaBytes))
	if !errors.Is(err, domain.ErrTooManyFiles) {
		t.Fatalf("manifest path-text cap: got %v, want domain.ErrTooManyFiles", err)
	}
}

// TestGuard_PerPathLengthCapRejected pins domain.MaxSitePathLen: one absurdly
// long path is rejected even though the file count and manifest size are tiny.
func TestGuard_PerPathLengthCapRejected(t *testing.T) {
	long := strings.Repeat("z", domain.MaxSitePathLen+10) + ".html"
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>ok</h1>"},
		{name: long, body: "x"},
	})
	_, err := archive.Untar(bytes.NewReader(arc), newRecordingSink(), int64(domain.UserQuotaBytes))
	if !errors.Is(err, domain.ErrTooManyFiles) {
		t.Fatalf("per-path length cap: got %v, want domain.ErrTooManyFiles", err)
	}
}
