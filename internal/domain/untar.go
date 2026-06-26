package domain

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// GzipMagic is the two-byte gzip member header (RFC 1952). Detection
// sniffs for it the same way DetectKind sniffs HTML vs Markdown.
var GzipMagic = [2]byte{0x1f, 0x8b}

// HasGzipMagic reports whether b begins with the gzip magic bytes.
// The format gate uses it to decide whether to try the archive path.
func HasGzipMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == GzipMagic[0] && b[1] == GzipMagic[1]
}

// FileSink receives one safe, fully-validated regular file from the
// archive: its cleaned relative path and the SHA + uncompressed size of
// its bytes. SafeUntar streams the file's bytes to the sink's writer so
// the caller (the DeploySite service) can hash + store each file as a
// content-addressed blob without SafeUntar importing any storage code.
//
// The sink returns the SHA it computed (content-addressing is by the
// file's uncompressed bytes) so the manifest can reference the blob.
type FileSink interface {
	// Store consumes the file's bytes from r (exactly size bytes) and
	// returns the content SHA it stored them under. size is the tar
	// header's declared size; the decompression-bomb guard has already
	// admitted these bytes against the running total before Store is
	// called.
	Store(p string, r io.Reader, size int64) (sha string, err error)
}

// SafeUntar streams a gzip-tar archive from src, enforcing the three
// security guards described in docs/SPEC.md "Safe-untar", and builds a
// Manifest by handing each safe regular file to sink.Store.
//
// quotaBudget is the identity's REMAINING quota in bytes: the maximum
// total uncompressed size this site may occupy. The effective byte cap
// is min(quotaBudget, MaxSiteBytes); the running uncompressed total is
// checked against it AS the tar streams, so a tiny archive that would
// expand to gigabytes aborts the instant it crosses the cap - never
// after decompressing fully.
//
// Guards, all of which abort the whole deploy on trip:
//
//   - Path safety: every entry must be a regular file or a directory;
//     symlinks, hardlinks, devices, and FIFOs are rejected. Each path
//     is cleaned and rejected if absolute, containing "..", or escaping
//     the site root. The manifest only ever holds safe relative paths.
//   - Decompression bomb: the running uncompressed byte total is capped.
//   - File-count + manifest-size: at most MaxSiteFiles regular files and
//     MaxManifestBytes of path text.
//
// SafeUntar itself performs NO durable I/O: it reads src and calls
// sink.Store. If it returns an error, the caller treats the deploy as
// failed and persists nothing (blobs the sink may have written are
// harmless - they're content-addressed and GC'd if unreferenced).
func SafeUntar(src io.Reader, sink FileSink, quotaBudget int64) (Manifest, error) {
	man := NewManifest()

	cap := max(min(quotaBudget, int64(MaxSiteBytes)), 0)

	gz, err := gzip.NewReader(src)
	if err != nil {
		// Not a valid gzip stream after all (truncated / corrupt). Treat
		// as an unsupported upload rather than a server error.
		return Manifest{}, fmt.Errorf("%w: not a valid gzip stream: %v", ErrUnsupportedKind, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var running int64
	var fileCount int
	var entries int

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: corrupt tar: %v", ErrUnsupportedKind, err)
		}

		// Bound the TOTAL entries iterated, not just regular files: a
		// directory entry (TypeDir) and an entry whose path cleans to ""
		// both `continue` before the file-count check below, so counting
		// only files would let a million-directories archive run the loop
		// unbounded. Cap every header read.
		entries++
		if entries > MaxSiteFiles {
			return Manifest{}, fmt.Errorf("%w: more than %d archive entries", ErrTooManyFiles, MaxSiteFiles)
		}

		// 1. Type safety: only regular files and directories are allowed.
		// Everything else - symlinks, hardlinks, char/block devices,
		// FIFOs, the legacy TypeRegA alias for files - is decided here.
		switch hdr.Typeflag {
		case tar.TypeDir:
			// Directories carry no bytes; we don't record them in the
			// manifest (the manifest is files only). Still validate the
			// path so a malicious dir entry can't sneak through.
			if _, err := cleanArchivePath(hdr.Name); err != nil {
				return Manifest{}, err
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
			// Regular file - fall through to extraction below.
		default:
			// Symlink, hardlink, device, FIFO, or anything exotic.
			return Manifest{}, fmt.Errorf("%w: %q has disallowed type %d", ErrUnsafeArchive, hdr.Name, hdr.Typeflag)
		}

		// 2. Path safety (zip-slip / traversal).
		rel, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return Manifest{}, err
		}
		if rel == "" {
			// Entry that cleans to nothing (e.g. "./") - skip, no file.
			continue
		}
		if isJunkPath(rel) {
			// OS-generated sidecar (macOS AppleDouble / .DS_Store /
			// __MACOSX); never published and doesn't count toward the caps.
			continue
		}
		if len(rel) > MaxSitePathLen {
			return Manifest{}, fmt.Errorf("%w: path %q exceeds %d bytes", ErrTooManyFiles, rel, MaxSitePathLen)
		}

		// 3. File-count cap.
		fileCount++
		if fileCount > MaxSiteFiles {
			return Manifest{}, fmt.Errorf("%w: more than %d files", ErrTooManyFiles, MaxSiteFiles)
		}

		// 4. Decompression-bomb guard. Header size is advisory; enforce
		// the real cap on the bytes actually read by wrapping the entry
		// reader with a counter that aborts when the RUNNING TOTAL across
		// all entries would cross the cap. We never trust hdr.Size alone -
		// a lying header could under-report and still stream gigabytes.
		entryReader := &cappedTarReader{
			r:        tr,
			running:  &running,
			capBytes: cap,
		}

		sha, err := sink.Store(rel, entryReader, hdr.Size)
		if err != nil {
			if errors.Is(err, ErrArchiveTooLarge) {
				return Manifest{}, ErrArchiveTooLarge
			}
			return Manifest{}, fmt.Errorf("store %q: %w", rel, err)
		}
		// The counter may have tripped exactly at EOF; surface it even
		// when Store consumed everything cleanly.
		if entryReader.tripped {
			return Manifest{}, ErrArchiveTooLarge
		}

		man.Add(rel, ManifestEntry{
			SHA:         sha,
			Size:        int(entryReader.read),
			ContentType: contentTypeByExt(rel),
		})

		// 5. Manifest-size cap (path text). Checked incrementally so a
		// flood of long names aborts before the map grows unbounded.
		if man.PathTextBytes() > MaxManifestBytes {
			return Manifest{}, fmt.Errorf("%w: manifest path text exceeds %d bytes", ErrTooManyFiles, MaxManifestBytes)
		}
	}

	// Strip a single shared top-level directory so a directory archive
	// (`tar czf - site/`) serves index.html at the root rather than under
	// /site/. No-op when files are already at root or span multiple dirs.
	man.StripCommonLeadingDir()
	return man, nil
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

// cleanArchivePath validates and normalizes one tar entry name to a
// safe, site-root-relative, slash-separated path. Returns ErrUnsafeArchive
// for anything that is absolute, escapes the root via "..", or uses a
// backslash (Windows-style separators are not trusted - they can hide a
// traversal on some extractors).
//
// Returns ("", nil) for entries that clean to the root itself ("." or
// "./"), which the caller skips. Returns the cleaned path otherwise.
func cleanArchivePath(name string) (string, error) {
	// Reject backslashes outright: a path like "..\\etc" is a traversal
	// that path.Clean (which only knows "/") would not catch.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("%w: backslash in %q", ErrUnsafeArchive, name)
	}
	// Reject NUL and other control bytes that have no place in a path.
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
	// After cleaning, a leading "../" (or a bare "..") means the entry
	// climbed out of the root. path.Clean keeps leading ".." segments,
	// so this catches every traversal that survived normalization.
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: path %q escapes site root", ErrUnsafeArchive, name)
	}
	// Defensive: Clean never yields a leading "/" for a relative input,
	// but guard anyway so a future Clean change can't open a hole.
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("%w: path %q resolves absolute", ErrUnsafeArchive, name)
	}

	return clean, nil
}

// cappedTarReader wraps a tar entry reader and aborts the read with
// ErrArchiveTooLarge the instant the RUNNING uncompressed total (shared
// across every entry via the running pointer) would cross capBytes. The
// cap is enforced on bytes as they are read out of the decompressor, so
// a decompression bomb is stopped mid-stream, never after full inflation.
type cappedTarReader struct {
	r        io.Reader
	running  *int64 // shared running total across all entries
	capBytes int64
	read     int64 // bytes read for THIS entry (manifest size)
	tripped  bool
}

func (c *cappedTarReader) Read(p []byte) (int, error) {
	// Pre-trim the read window so we never pull more than the remaining
	// budget out of the decompressor in a single Read.
	remaining := c.capBytes - *c.running
	if remaining <= 0 {
		c.tripped = true
		return 0, ErrArchiveTooLarge
	}
	if int64(len(p)) > remaining {
		// Read one byte past the budget so we can detect overflow: if the
		// source still has bytes after we've consumed the whole budget,
		// the site is over-cap. We cap the slice at remaining+1.
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
