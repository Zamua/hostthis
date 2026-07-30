package domain

import (
	"fmt"
	"io"
	"path"
	"strings"
)

// GzipMagic is the two-byte gzip member header (RFC 1952).
var GzipMagic = [2]byte{0x1f, 0x8b}

// HasGzipMagic reports whether b begins with the gzip magic bytes.
func HasGzipMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == GzipMagic[0] && b[1] == GzipMagic[1]
}

// FileSink receives one safe, fully-validated regular file from the archive.
// SafeUntar streams the file's bytes to the sink so the caller can hash +
// store each file as a content-addressed blob without SafeUntar importing any
// storage code.
type FileSink interface {
	// Store consumes exactly size bytes from r and returns the content SHA
	// it stored them under (content-addressing is by the file's
	// uncompressed bytes, so the manifest can reference the blob). size is
	// the tar header's declared size; the decompression-bomb guard has
	// already admitted these bytes against the running total.
	Store(p string, r io.Reader, size int64) (sha string, compressedSize int, err error)
}

// isJunkPath reports whether rel is an OS-generated metadata file that must
// never be published: macOS AppleDouble sidecars (._name), .DS_Store, and the
// __MACOSX/ container Finder adds to archives.
func isJunkPath(rel string) bool {
	if rel == "__MACOSX" || strings.HasPrefix(rel, "__MACOSX/") {
		return true
	}
	base := path.Base(rel)
	return base == ".DS_Store" || strings.HasPrefix(base, "._")
}

// cleanArchivePath validates and normalizes one tar entry name to a safe,
// site-root-relative, slash-separated path. Returns ErrUnsafeArchive for
// anything absolute, escaping the root via "..", or using a backslash
// (Windows-style separators can hide a traversal on some extractors).
//
// Returns ("", nil) for entries that clean to the root itself ("." or "./"),
// which the caller skips.
func cleanArchivePath(name string) (string, error) {
	// A path like "..\\etc" is a traversal path.Clean (which only knows "/")
	// would not catch.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("%w: backslash in %q", ErrUnsafeArchive, name)
	}
	if strings.ContainsRune(name, 0x00) {
		return "", fmt.Errorf("%w: NUL byte in path", ErrUnsafeArchive)
	}

	// Absolute paths escape the root by definition.
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafeArchive, name)
	}

	clean := path.Clean(name)

	// path.Clean turns "" and "./" into ".".
	if clean == "." {
		return "", nil
	}
	// path.Clean keeps leading ".." segments, so this catches every traversal
	// that survived normalization.
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: path %q escapes site root", ErrUnsafeArchive, name)
	}
	// Clean never yields a leading "/" for a relative input; guard anyway so
	// a future Clean change cannot open a hole.
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("%w: path %q resolves absolute", ErrUnsafeArchive, name)
	}

	return clean, nil
}

// cappedReader wraps a tar entry reader and aborts with ErrArchiveTooLarge the
// instant the RUNNING uncompressed total (shared across every entry via the
// running pointer) would cross capBytes. The cap is enforced on bytes as they
// come out of the decompressor, so a decompression bomb is stopped mid-stream,
// never after full inflation.
type cappedReader struct {
	r        io.Reader
	running  *int64 // shared running total across all entries
	capBytes int64
	read     int64 // bytes read for THIS entry (manifest size)
	tripped  bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	// Pre-trim the read window so no single Read pulls more than the
	// remaining budget out of the decompressor.
	remaining := c.capBytes - *c.running
	if remaining <= 0 {
		c.tripped = true
		return 0, ErrArchiveTooLarge
	}
	if int64(len(p)) > remaining {
		// One byte past the budget, so overflow is detectable: bytes still
		// arriving once the whole budget is consumed mean an over-cap site.
		p = p[:remaining+1]
	}
	n, err := c.r.Read(p)
	if n > 0 {
		*c.running += int64(n)
		c.read += int64(n)
		if *c.running > c.capBytes {
			c.tripped = true
			return n, ErrArchiveTooLarge
		}
	}
	return n, err
}
