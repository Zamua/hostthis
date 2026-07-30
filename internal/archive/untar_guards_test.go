package archive_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/archive"
	"github.com/Zamua/hostthis/internal/domain"
)

// One focused test per security-critical SafeUntar guard (path safety,
// decompression bomb, count / manifest-size caps), so a regression names
// exactly which guard broke. untar_test.go covers the happy path and the broad
// guard surface.

// countingSink records the TOTAL bytes handed to it across all files.
type countingSink struct {
	total int64
}

func (c *countingSink) Store(_ string, r io.Reader, _ int64) (string, int, error) {
	n, err := io.Copy(io.Discard, r)
	c.total += n
	if err != nil {
		if errors.Is(err, domain.ErrArchiveTooLarge) {
			return "", 0, domain.ErrArchiveTooLarge
		}
		return "", 0, err
	}
	return "sha", int(n), nil
}

// --- Guard 1: path safety (zip-slip / traversal / non-regular types) ---

// TestGuard_PathTraversalRejected pins that a relative climb, an absolute
// path, and a symlink are each rejected, and that no unsafe path reaches the
// sink. Sub-tested by shape so a regression points at which one broke.
func TestGuard_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name  string
		entry tarEntry
	}{
		{"relative climb", tarEntry{name: "../escape.html", body: "<h1>bad</h1>"}},
		{"absolute path", tarEntry{name: "/etc/cron.d/evil", body: "* * * * *"}},
		{"symlink out of root", tarEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The valid index makes the rejection unambiguously the GUARD
			// firing rather than an empty/garbage archive.
			arc := makeGzipTar(t, []tarEntry{
				{name: "index.html", body: "<h1>ok</h1>"},
				c.entry,
			})
			sink := newRecordingSink()
			_, err := archive.Untar(bytes.NewReader(arc), sink, int64(domain.UserQuotaBytes))
			if !errors.Is(err, domain.ErrUnsafeArchive) {
				t.Fatalf("%s: got %v, want domain.ErrUnsafeArchive", c.name, err)
			}
			// index.html may or may not have been seen depending on tar
			// order; the invariant is that no UNSAFE path was stored.
			for p := range sink.files {
				if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || p == "link" {
					t.Fatalf("%s: unsafe path %q leaked to sink", c.name, p)
				}
			}
		})
	}
}

// --- Guard 2: decompression bomb aborts mid-stream, stores nothing ---

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

	sink := &countingSink{}
	_, err := archive.Untar(bytes.NewReader(arc), sink, cap)
	if !errors.Is(err, domain.ErrArchiveTooLarge) {
		t.Fatalf("bomb: got %v, want domain.ErrArchiveTooLarge", err)
	}
	// The +1 is the lookahead probe that detects overflow.
	if sink.total > cap+1 {
		t.Fatalf("bomb read %d bytes, want <= cap+1 (%d): full expansion not aborted mid-stream", sink.total, cap+1)
	}
}

// --- Guard 3: file-count + manifest-size (path-text) caps ---

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
		// dir/<6-digit>aaaa....html, each cleaned path ~pathLen bytes.
		name := "dir" + pad6(i) + stem + ".html"
		entries = append(entries, tarEntry{name: name, body: "x"})
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

// pad6 zero-pads n to six digits so generated paths are uniform length.
func pad6(n int) string {
	s := itoa(n)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}
