package domain

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// This file pins the security guards of SafeUntar as focused tests, each
// of which FAILS if its guard is removed (see the guard-removal notes in
// the test bodies). It complements untar_test.go: that file proves the
// happy path + the broad guard surface; this one isolates each guard the
// SPEC marks security-critical (path safety, decompression bomb, count /
// manifest-size caps) so a regression names exactly which guard broke.

// countingSink records the TOTAL bytes it was ever handed across all
// files. The decompression-bomb test asserts this stays bounded by the
// cap (plus the one lookahead byte the reader pulls to detect overflow),
// proving SafeUntar aborts MID-STREAM and never buffers the full
// expansion of a bomb.
type countingSink struct {
	total int64
}

func (c *countingSink) Store(_ string, r io.Reader, _ int64) (string, int, error) {
	n, err := io.Copy(io.Discard, r)
	c.total += n
	if err != nil {
		if errors.Is(err, ErrArchiveTooLarge) {
			return "", 0, ErrArchiveTooLarge
		}
		return "", 0, err
	}
	return "sha", int(n), nil
}

// --- Guard 1: path safety (zip-slip / traversal / non-regular types) ---

// TestGuard_PathTraversalRejected covers the three traversal shapes the
// SPEC names: a "../escape" relative climb, an absolute path, and a
// symlink. Each is its OWN sub-test so a regression points at the shape.
//
// Guard-removal demonstration: deleting the "../" check in
// cleanArchivePath (untar.go) makes the relative-climb case go green
// (SafeUntar returns nil + a manifest with the escaping path); deleting
// the symlink/type switch in SafeUntar makes the symlink case go green.
// Restoring both turns this red->green again. Verified locally by
// commenting out each guard in turn.
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
			// A valid index alongside the malicious entry so we know the
			// rejection is the GUARD firing, not an "empty/garbage archive".
			arc := makeGzipTar(t, []tarEntry{
				{name: "index.html", body: "<h1>ok</h1>"},
				c.entry,
			})
			sink := newRecordingSink()
			_, err := SafeUntar(bytes.NewReader(arc), sink, UserQuotaBytes)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("%s: got %v, want ErrUnsafeArchive", c.name, err)
			}
			// And the escaping path NEVER reaches the manifest/sink: the
			// guard fires before any unsafe path is recorded. (index.html may
			// or may not have been seen depending on tar order; the invariant
			// is that no UNSAFE path was stored.)
			for p := range sink.files {
				if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || p == "link" {
					t.Fatalf("%s: unsafe path %q leaked to sink", c.name, p)
				}
			}
		})
	}
}

// --- Guard 2: decompression bomb aborts mid-stream, stores nothing ---

// TestGuard_BombAbortsBeforeFullExpansion proves the bomb guard checks
// the running UNCOMPRESSED total as bytes stream, not after inflating
// the whole file. A single entry whose body is 64x the cap must abort
// having read at most cap (+1 lookahead) bytes - never the full 64x.
//
// Guard-removal demonstration: removing the running-total check in
// cappedTarReader.Read (untar.go) lets the whole file inflate, so
// c.total jumps from ~cap to the full 64*cap and the bounded-read
// assertion below fails (and the err==nil path fails too). Restoring it
// re-pins the abort. Verified locally.
func TestGuard_BombAbortsBeforeFullExpansion(t *testing.T) {
	const cap = 256 << 10 // 256 KiB budget
	// One file 64x the cap: a small compressed archive that would inflate
	// to 16 MiB if extracted whole.
	bomb := strings.Repeat("A", 64*cap)
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "bomb.html", body: bomb},
	})

	sink := &countingSink{}
	_, err := SafeUntar(bytes.NewReader(arc), sink, cap)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("bomb: got %v, want ErrArchiveTooLarge", err)
	}
	// The reader pulls at most cap+1 bytes (the +1 is the lookahead probe
	// that detects overflow). It must NOT have streamed the whole 64x body.
	if sink.total > cap+1 {
		t.Fatalf("bomb read %d bytes, want <= cap+1 (%d): full expansion not aborted mid-stream", sink.total, cap+1)
	}
}

// --- Guard 3: file-count + manifest-size (path-text) caps ---

// TestGuard_ManifestPathTextCapRejected pins the SECOND count-style guard
// the SPEC names: total manifest path text is bounded (MaxManifestBytes),
// independent of the file count. A "few thousand long-named files" stays
// well under MaxSiteFiles but blows the path-text budget, so it must be
// rejected with ErrTooManyFiles.
//
// Guard-removal demonstration: deleting the PathTextBytes() > MaxManifestBytes
// check at the bottom of SafeUntar's loop (untar.go) lets the archive
// extract fully (returns nil), failing this test. Restoring re-pins it.
// Verified locally.
func TestGuard_ManifestPathTextCapRejected(t *testing.T) {
	// Each path is ~900 bytes (< MaxSitePathLen 1024). The total path text
	// crosses MaxManifestBytes (1 MiB) well before MaxSiteFiles (5000):
	// 1 MiB / 900 ~= 1165 entries. We write 2000 to be safely over.
	const pathLen = 900
	stem := strings.Repeat("a", pathLen-len(".html")-6) // leave room for index + ext
	entries := make([]tarEntry, 0, 2001)
	entries = append(entries, tarEntry{name: "index.html", body: "<h1>ok</h1>"})
	for i := range 2000 {
		// dir/<6-digit>aaaa....html, each cleaned path ~pathLen bytes.
		name := "dir" + pad6(i) + stem + ".html"
		entries = append(entries, tarEntry{name: name, body: "x"})
	}
	_, err := SafeUntar(bytes.NewReader(makeGzipTar(t, entries)), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("manifest path-text cap: got %v, want ErrTooManyFiles", err)
	}
}

// TestGuard_PerPathLengthCapRejected pins the per-path length cap
// (MaxSitePathLen): a single absurdly long path is rejected even though
// the file count and total manifest size are tiny.
//
// Guard-removal demonstration: deleting the `len(rel) > MaxSitePathLen`
// check in SafeUntar (untar.go) lets the single long-named file through
// (returns nil), failing this test. Restoring re-pins it. Verified locally.
func TestGuard_PerPathLengthCapRejected(t *testing.T) {
	long := strings.Repeat("z", MaxSitePathLen+10) + ".html"
	arc := makeGzipTar(t, []tarEntry{
		{name: "index.html", body: "<h1>ok</h1>"},
		{name: long, body: "x"},
	})
	_, err := SafeUntar(bytes.NewReader(arc), newRecordingSink(), UserQuotaBytes)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("per-path length cap: got %v, want ErrTooManyFiles", err)
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
