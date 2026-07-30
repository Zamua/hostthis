// Package archive is the gzip-tar adapter for site uploads.
//
// It owns the codec and nothing else. Every security guard lives in the domain,
// because none of them is about tar. Exists so the domain need not import
// archive/tar and compress/gzip: a wire format is a mechanism the domain has no
// business knowing.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/Zamua/hostthis/internal/domain"
)

// Untar streams a gzip-tar archive from src, applying the domain's guards to
// every entry.
//
// quotaBudget is the identity's remaining quota in bytes.
//
// No durable I/O of its own. On error the caller persists nothing; blobs the
// sink already wrote are content-addressed and GC'd if unreferenced.
func Untar(src io.Reader, sink domain.FileSink, quotaBudget int64) (domain.Manifest, error) {
	ex := domain.NewSiteExtractor(quotaBudget)

	gz, err := gzip.NewReader(src)
	if err != nil {
		// Truncated or corrupt: an unsupported upload, not a server error.
		return domain.Manifest{}, fmt.Errorf("%w: not a valid gzip stream: %v", domain.ErrUnsupportedKind, err)
	}
	defer gz.Close() //nolint:errcheck

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.Manifest{}, fmt.Errorf("%w: corrupt tar: %v", domain.ErrUnsupportedKind, err)
		}
		// tr reads the current entry and is valid only until the next Next().
		if err := ex.Add(entryOf(hdr), tr, sink); err != nil {
			return domain.Manifest{}, err
		}
	}
	return ex.Finish(), nil
}

// legacyTypeRegA is tar's pre-1.11 alias for a regular file. Go's reader
// normalises it to TypeReg, so this arm is defensive. Spelled as the raw byte
// because the named constant is deprecated.
const legacyTypeRegA = '\x00'

// entryOf maps a tar header onto the domain's format-neutral entry.
//
// Deliberately closed: anything not a plain file or directory becomes
// EntryOther, which the domain rejects. Listing allowed rather than disallowed
// types keeps an exotic type flag from defaulting into "allowed".
func entryOf(hdr *tar.Header) domain.ArchiveEntry {
	kind := domain.EntryOther
	switch hdr.Typeflag {
	case tar.TypeDir:
		kind = domain.EntryDir
	case tar.TypeReg, legacyTypeRegA:
		kind = domain.EntryFile
	}
	return domain.ArchiveEntry{
		Name:     hdr.Name,
		Size:     hdr.Size,
		Kind:     kind,
		TypeCode: int(hdr.Typeflag),
	}
}
