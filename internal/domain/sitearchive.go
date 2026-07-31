package domain

import (
	"errors"
	"fmt"
	"io"
)

// Site-archive extraction rules, over format-neutral entries. No guard here is
// about tar: they concern paths, counts and bytes, and would be identical for a
// zip or a directory walk. An adapter owns the codec; this owns the policy.

// ArchiveEntryKind is what an entry is, independent of the archive's encoding.
// A format adapter maps its own type codes onto these.
type ArchiveEntryKind int

const (
	// EntryOther is symlinks, hardlinks, devices, FIFOs. Always rejected: this
	// category carries the link-following and device-node attacks.
	EntryOther ArchiveEntryKind = iota
	EntryFile
	EntryDir
)

// ArchiveEntry is one entry's metadata, format-neutral.
type ArchiveEntry struct {
	Name string // the raw, UNTRUSTED path as the archive declares it
	Size int64  // ADVISORY only; a lying header must not be believed
	Kind ArchiveEntryKind

	// TypeCode is the adapter's own type value, carried only so a rejection
	// message can name what was in the archive. Never decided on.
	TypeCode int
}

// SiteExtractor applies the site-upload guards across a stream of entries and
// accumulates a Manifest. One extractor per archive; not concurrent-safe.
//
// Every guard aborts the whole deploy on trip, so a partially extracted site is
// never published:
//
//   - only regular files and directories.
//   - paths cleaned, and rejected if absolute, containing "..", or escaping the
//     site root.
//   - the running uncompressed total is capped, measured as bytes are read
//     rather than taken from the declared size.
//   - file-count, entry-count, path-length and manifest-size caps.
type SiteExtractor struct {
	capBytes int64
	running  int64
	files    int
	entries  int
	// pathBytes carries Manifest.PathTextBytes() forward across Adds. Re-summing
	// the manifest per file is quadratic in an entry count the client controls.
	// It counts a path once, so it stays exact when an archive repeats one.
	pathBytes int
	man       Manifest
}

// NewSiteExtractor starts an extraction with quotaBudget bytes available. The
// effective cap is min(quotaBudget, MaxSiteBytes).
func NewSiteExtractor(quotaBudget int64) *SiteExtractor {
	return &SiteExtractor{
		capBytes: max(min(quotaBudget, int64(MaxSiteBytes)), 0),
		man:      NewManifest(),
	}
}

// Add applies every guard to one entry and, if admissible, streams its bytes to
// sink and records it.
//
// The byte cap is enforced on what is actually READ from body, not on ent.Size:
// a header that under-reports could stream past a cap that already approved it.
func (e *SiteExtractor) Add(ent ArchiveEntry, body io.Reader, sink FileSink) error {
	// TOTAL entries are bounded, not just admitted files: directories and paths
	// that clean away return early, so counting only files would leave an
	// archive of a million directories unbounded.
	e.entries++
	if e.entries > MaxSiteFiles {
		return fmt.Errorf("%w: more than %d archive entries", ErrTooManyFiles, MaxSiteFiles)
	}

	switch ent.Kind {
	case EntryDir:
		// Not recorded (the manifest is files only), but still validated.
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
		// OS-generated sidecar: never published, never counted toward a cap.
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
	// The counter can trip exactly at EOF, leaving Store to return cleanly.
	if capped.tripped {
		return ErrArchiveTooLarge
	}

	// Charged only for a path the manifest does not already hold, so the running
	// total equals Manifest.PathTextBytes() even when an archive repeats a path.
	if _, dup := e.man.Files[rel]; !dup {
		e.pathBytes += len(rel)
	}
	e.man.Add(rel, ManifestEntry{
		SHA:            sha,
		Size:           int(capped.read),
		CompressedSize: compressedSize,
		ContentType:    contentTypeByExt(rel),
	})

	// Checked incrementally, so a flood of long names aborts before the map
	// grows.
	if e.pathBytes > MaxManifestBytes {
		return fmt.Errorf("%w: manifest path text exceeds %d bytes", ErrTooManyFiles, MaxManifestBytes)
	}
	return nil
}

// Finish returns the manifest. Call once, after the last Add.
func (e *SiteExtractor) Finish() Manifest {
	// Strip a shared top-level directory so `tar czf - site/` serves index.html
	// at the root. No-op when files are at root or span multiple directories.
	e.man.StripCommonLeadingDir()
	return e.man
}
