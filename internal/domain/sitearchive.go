package domain

import (
	"errors"
	"fmt"
	"io"
)

// Site-archive extraction RULES, expressed over format-neutral entries.
//
// This used to be SafeUntar, which opened a gzip stream and drove a tar reader
// itself. That put two wire formats in the domain for no gain: none of the
// guards below are about tar. They are about paths, counts and bytes, and they
// would be identical for a zip, a directory walk, or anything else that yields
// (name, size, kind, bytes).
//
// So the split is: an adapter owns the CODEC (open the gzip, iterate the tar,
// map its type flags onto ArchiveEntryKind) and this owns the POLICY. The
// domain never learns what a tarball is, and the guards stay in one place
// where they can be read as a security argument rather than as loop control.

// ArchiveEntryKind is what an entry IS, independent of how the archive encodes
// it. A format adapter maps its own type codes onto these three.
type ArchiveEntryKind int

const (
	// EntryOther is anything that is not a plain file or a directory:
	// symlinks, hardlinks, devices, FIFOs. Always rejected - it is the
	// category that carries the link-following and device-node attacks.
	EntryOther ArchiveEntryKind = iota
	EntryFile
	EntryDir
)

// ArchiveEntry is one entry's metadata, format-neutral.
type ArchiveEntry struct {
	Name string // the raw, UNTRUSTED path as the archive declares it
	Size int64  // ADVISORY only; a lying header must not be believed
	Kind ArchiveEntryKind

	// TypeCode is the adapter's own type value, carried solely so a rejection
	// message can name what was actually in the archive. Never decided on.
	TypeCode int
}

// SiteExtractor applies the site-upload guards across a stream of entries and
// accumulates the resulting Manifest. Not safe for concurrent use; one
// extractor per archive.
//
// The guards, all of which abort the whole deploy on trip (a partially
// extracted site is never published):
//
//   - Type safety: only regular files and directories.
//   - Path safety: cleaned, and rejected if absolute, containing "..", or
//     escaping the site root (zip-slip).
//   - Decompression bomb: the running UNCOMPRESSED total is capped, checked as
//     bytes are read rather than from the declared size.
//   - File-count, entry-count, path-length and manifest-size caps.
type SiteExtractor struct {
	capBytes int64
	running  int64
	files    int
	entries  int
	man      Manifest
}

// NewSiteExtractor starts an extraction with quotaBudget bytes of the
// identity's remaining quota available. The effective cap is
// min(quotaBudget, MaxSiteBytes): a site may never exceed the per-site
// ceiling, and may not exceed what the owner has left either.
func NewSiteExtractor(quotaBudget int64) *SiteExtractor {
	return &SiteExtractor{
		capBytes: max(min(quotaBudget, int64(MaxSiteBytes)), 0),
		man:      NewManifest(),
	}
}

// Add applies every guard to one entry and, if it is an admissible file,
// streams its bytes to sink and records it in the manifest.
//
// body must yield exactly this entry's bytes. The byte cap is enforced on what
// is actually READ from body, not on ent.Size: a header that under-reports
// could otherwise stream gigabytes past a cap that already approved it.
func (e *SiteExtractor) Add(ent ArchiveEntry, body io.Reader, sink FileSink) error {
	// Bound the TOTAL entries seen, not just admitted files. A directory and a
	// path that cleans away both return early below, so counting only files
	// would let an archive of a million directories iterate unbounded.
	e.entries++
	if e.entries > MaxSiteFiles {
		return fmt.Errorf("%w: more than %d archive entries", ErrTooManyFiles, MaxSiteFiles)
	}

	switch ent.Kind {
	case EntryDir:
		// Directories carry no bytes and are not recorded (the manifest is
		// files only), but the path is still validated so a malicious
		// directory entry cannot slip through unchecked.
		if _, err := cleanArchivePath(ent.Name); err != nil {
			return err
		}
		return nil
	case EntryFile:
		// Fall through.
	default:
		return fmt.Errorf("%w: %q has disallowed type %d", ErrUnsafeArchive, ent.Name, ent.TypeCode)
	}

	rel, err := cleanArchivePath(ent.Name)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil // cleans to nothing (e.g. "./")
	}
	if isJunkPath(rel) {
		// OS-generated sidecar; never published, and deliberately does NOT
		// count toward the caps.
		return nil
	}
	if len(rel) > MaxSitePathLen {
		return fmt.Errorf("%w: path %q exceeds %d bytes", ErrTooManyFiles, rel, MaxSitePathLen)
	}

	e.files++
	if e.files > MaxSiteFiles {
		return fmt.Errorf("%w: more than %d files", ErrTooManyFiles, MaxSiteFiles)
	}

	capped := &cappedReader{r: body, running: &e.running, capBytes: e.capBytes}
	sha, compressedSize, err := sink.Store(rel, capped, ent.Size)
	if err != nil {
		if errors.Is(err, ErrArchiveTooLarge) {
			return ErrArchiveTooLarge
		}
		return fmt.Errorf("store %q: %w", rel, err)
	}
	// The counter can trip exactly at EOF, which leaves Store returning
	// cleanly. Surface it anyway.
	if capped.tripped {
		return ErrArchiveTooLarge
	}

	e.man.Add(rel, ManifestEntry{
		SHA:            sha,
		Size:           int(capped.read),
		CompressedSize: compressedSize,
		ContentType:    contentTypeByExt(rel),
	})

	// Checked incrementally so a flood of long names aborts before the map
	// grows unbounded, rather than after.
	if e.man.PathTextBytes() > MaxManifestBytes {
		return fmt.Errorf("%w: manifest path text exceeds %d bytes", ErrTooManyFiles, MaxManifestBytes)
	}
	return nil
}

// Finish returns the completed manifest. Call once, after the last Add.
func (e *SiteExtractor) Finish() Manifest {
	// Strip a single shared top-level directory so `tar czf - site/` serves
	// index.html at the root rather than under /site/. No-op when files are
	// already at root or span multiple directories.
	e.man.StripCommonLeadingDir()
	return e.man
}
