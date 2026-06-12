package domain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// recordingSink captures every file SafeUntar hands it, computing the
// SHA the same way the real blob sink does (over uncompressed bytes).
type recordingSink struct {
	files map[string][]byte
}

func newRecordingSink() *recordingSink { return &recordingSink{files: map[string][]byte{}} }

func (s *recordingSink) Store(p string, r io.Reader, _ int64) (string, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		// Propagate the cap sentinel unchanged, like the production sink.
		if errors.Is(err, ErrArchiveTooLarge) {
			return "", ErrArchiveTooLarge
		}
		return "", err
	}
	body := buf.Bytes()
	s.files[p] = body
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// tarEntry describes one entry to write into a test archive.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// makeGzipTar builds a gzip-tar from entries. typeflag 0 defaults to a
// regular file.
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
	man, err := SafeUntar(bytes.NewReader(arc), sink, UserQuotaBytes)
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

func TestSafeUntar_RejectsSymlink(t *testing.T) {
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>ok</h1>"},
		{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("symlink: got %v, want ErrUnsafeArchive", err)
	}
}

func TestSafeUntar_RejectsHardlink(t *testing.T) {
	arc := makeGzipTar(t, []tarEntry{
		{name: "a.html", body: "<h1>ok</h1>"},
		{name: "b", typeflag: tar.TypeLink, linkname: "a.html"},
	})
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("hardlink: got %v, want ErrUnsafeArchive", err)
	}
}

func TestSafeUntar_RejectsDeviceAndFifo(t *testing.T) {
	for _, tf := range []byte{tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		arc := makeGzipTar(t, []tarEntry{{name: "dev", typeflag: tf}})
		_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
		if !errors.Is(err, ErrUnsafeArchive) {
			t.Fatalf("typeflag %d: got %v, want ErrUnsafeArchive", tf, err)
		}
	}
}

func TestSafeUntar_RejectsTraversal(t *testing.T) {
	cases := []string{
		"../escape.html",
		"../../etc/passwd",
		"a/../../b.html",
		"/absolute.html",
		"foo/../../bar.html",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			arc := makeGzipTar(t, []tarEntry{{name: name, body: "x"}})
			_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("path %q: got %v, want ErrUnsafeArchive", name, err)
			}
		})
	}
}

func TestSafeUntar_RejectsBackslashTraversal(t *testing.T) {
	arc := makeGzipTar(t, []tarEntry{{name: `..\windows\evil.html`, body: "x"}})
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("backslash: got %v, want ErrUnsafeArchive", err)
	}
}

func TestSafeUntar_AllowsDotSlashPrefix(t *testing.T) {
	// "./index.html" is the common `tar czf - .` shape - must be accepted
	// and clean to "index.html".
	arc := makeGzipTar(t, []tarEntry{{name: "./index.html", body: "<h1>ok</h1>"}})
	man, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if err != nil {
		t.Fatalf("SafeUntar: %v", err)
	}
	if _, ok := man.Files["index.html"]; !ok {
		t.Fatalf("expected cleaned path index.html, got %v", keys(man.Files))
	}
}

func TestSafeUntar_DecompressionBombAbortsMidStream(t *testing.T) {
	// One file far larger than the cap. The cap is enforced on the bytes
	// as they stream, so this aborts long before the whole file inflates.
	big := strings.Repeat("A", 4<<20) // 4 MiB
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "big.html", body: big},
	})
	// Budget of 1 MiB: the 4 MiB file must trip the guard.
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), 1<<20)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("bomb: got %v, want ErrArchiveTooLarge", err)
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
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), 1<<20)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("running total: got %v, want ErrArchiveTooLarge", err)
	}
}

func TestSafeUntar_FileCountCap(t *testing.T) {
	entries := make([]tarEntry, 0, MaxSiteFiles+2)
	entries = append(entries, tarEntry{name: "index.html", body: "x"})
	for i := 0; i < MaxSiteFiles+1; i++ {
		entries = append(entries, tarEntry{name: "f" + itoa(i) + ".txt", body: "y"})
	}
	_, err := SafeUntar(bytes.NewReader(makeGzipTar(t, entries)), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("file count: got %v, want ErrTooManyFiles", err)
	}
}

func TestSafeUntar_RejectsNonGzip(t *testing.T) {
	// Plain bytes, no gzip wrapper - SafeUntar must reject as unsupported.
	_, err := SafeUntar(strings.NewReader("not a gzip stream at all"), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("non-gzip: got %v, want ErrUnsupportedKind", err)
	}
}

func TestSafeUntar_CorruptTarInGzip(t *testing.T) {
	// Valid gzip wrapping garbage (not a tar).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(bytes.Repeat([]byte{0x42}, 2048))
	_ = gz.Close()
	_, err := SafeUntar(bytes.NewReader(buf.Bytes()), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("corrupt tar: got %v, want ErrUnsupportedKind", err)
	}
}

func TestSafeUntar_DedupesIdenticalFiles(t *testing.T) {
	same := "<h1>same bytes</h1>"
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: same},
		{name: "copy.html", body: same},
	})
	man, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if err != nil {
		t.Fatalf("SafeUntar: %v", err)
	}
	if man.Files["index.html"].SHA != man.Files["copy.html"].SHA {
		t.Fatalf("identical files should share a SHA")
	}
	// DedupedSize counts the shared blob once.
	if want := len(same); man.DedupedSize() != want {
		t.Fatalf("deduped size: got %d, want %d", man.DedupedSize(), want)
	}
}

// -- tiny helpers (avoid pulling strconv into the test for two callers) --

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func keys(m map[string]ManifestEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
